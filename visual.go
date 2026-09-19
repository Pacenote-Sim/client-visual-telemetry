// Package visual is the client half of the visual-telemetry server plugin. At
// the end of every lap it sends the lap as it was driven — speed, revs, pedals,
// gear, the line round the circuit — and the plugin draws it. The chart's
// address comes back, and the companion's page is the list of them, newest
// first, so a driver leaving the car has every lap of the stint to look at.
//
// It costs nothing but the upload: the drawing is the server's, the pictures
// are the server's, and nothing here asks a vendor for anything.
package visual

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pacenote-sim/clientplugin"
	"github.com/pacenote-sim/protocol/wire"
)

func init() { clientplugin.RegisterCompanion(New()) }

// Name is the server plugin this is the client half of.
const Name = "visual-telemetry"

const (
	// MaxPoints is the most a lap may be drawn from. The plugin takes 4096;
	// a lap is sent thinned to that, evenly by distance, which is more than
	// a chart 1400 pixels wide can show anyway.
	MaxPoints = 4096
	// MinPoints is the fewest a lap can be drawn from at all.
	MinPoints = 2
	// KeepCharts is how many laps the page lists.
	KeepCharts = 12
	// PostTimeout bounds one upload. A lap is a few hundred kilobytes and the
	// next one is a minute away; a slow server should not hold up the stint,
	// and a driver closing the app should not wait long for the last one.
	PostTimeout = 15 * time.Second
)

// Companion is the companion.
type Companion struct {
	mu     sync.Mutex
	host   clientplugin.Host
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	stint  clientplugin.Stint
	charts []chart
	failed string
}

// chart is one lap that was drawn.
type chart struct {
	Lap   int
	LapMs int
	Kind  wire.Kind
	SVG   string
	PNG   string
}

// New is a companion with nothing drawn yet.
func New() *Companion { return &Companion{} }

// Name implements [clientplugin.Companion].
func (*Companion) Name() string { return Name }

// Wants implements [clientplugin.Companion]: the start of a stint, to clear
// the page, and every lap as it finishes.
func (*Companion) Wants() []clientplugin.EventKind {
	return []clientplugin.EventKind{clientplugin.KindStintStarted, clientplugin.KindLapCompleted}
}

// Start implements [clientplugin.Companion].
func (c *Companion) Start(ctx context.Context, h clientplugin.Host) error {
	c.mu.Lock()
	c.host = h
	c.ctx, c.cancel = context.WithCancel(ctx)
	c.charts, c.failed = nil, ""
	c.mu.Unlock()
	c.show()
	return nil
}

// Stop implements [clientplugin.Companion]: the uploads in flight are waited
// for, because a lap half sent is a lap not drawn.
func (c *Companion) Stop() error {
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	c.wg.Wait()
	return nil
}

// Notify implements [clientplugin.Companion].
func (c *Companion) Notify(_ context.Context, e clientplugin.Event) error {
	switch e := e.(type) {
	case *clientplugin.StintStarted:
		c.mu.Lock()
		c.stint, c.charts, c.failed = e.Stint, nil, ""
		c.mu.Unlock()
		c.show()
	case *clientplugin.LapCompleted:
		lap := *e
		c.spawn(func(ctx context.Context) { c.post(ctx, &lap) })
	}
	return nil
}

// spawn runs work that outlives the event that started it. A lap is drawn
// while the driver is on the next one.
func (c *Companion) spawn(f func(context.Context)) {
	c.mu.Lock()
	ctx := c.ctx
	c.mu.Unlock()
	if ctx == nil {
		return
	}
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		f(ctx)
	}()
}

// lapReport is what the plugin's POST /laps takes.
type lapReport struct {
	StintID      string            `json:"stint_id"`
	Lap          int               `json:"lap"`
	LapMs        int               `json:"lap_ms,omitempty"`
	Session      wire.SessionType  `json:"session,omitempty"`
	Track        string            `json:"track,omitempty"`
	Car          string            `json:"car,omitempty"`
	TrackLengthM int               `json:"track_length_m,omitempty"`
	Sectors      []float64         `json:"sectors,omitempty"`
	Corners      []reportCorner    `json:"corners,omitempty"`
	Trace        []wire.TracePoint `json:"trace"`
}

// reportCorner is a corner as the map wants it: its number and where it is.
type reportCorner struct {
	Turn    int `json:"turn"`
	ApexPct int `json:"apex_pct"`
}

// drawn is what the plugin answers with.
type drawn struct {
	Lap    string `json:"lap"`
	Points int    `json:"points"`
	SVG    string `json:"svg"`
	PNG    string `json:"png"`
}

// post sends one lap to be drawn and keeps where it was drawn to. A lap the
// plugin will not take is said once on the page and not retried: the next lap
// is a minute away and is the one the driver will look at.
func (c *Companion) post(ctx context.Context, e *clientplugin.LapCompleted) {
	trace := Resample(e.Trace, MaxPoints)
	if len(trace) < MinPoints {
		c.log().Info("a lap had too few points to draw", "lap", e.Lap, "points", len(trace))
		return
	}
	rep := lapReport{
		StintID: e.Stint.ID, Lap: e.Lap, LapMs: e.LapMs, Session: e.Stint.Session,
		Track: e.Stint.Track, Car: e.Stint.Car, TrackLengthM: e.Stint.TrackLengthM,
		Sectors: e.Stint.Sectors, Corners: cornersOf(e.Corners), Trace: trace,
	}
	body, err := json.Marshal(rep)
	if err != nil {
		c.log().Warn("a lap could not be written out", "lap", e.Lap, "reason", err.Error())
		return
	}

	// A lap that has been driven is a lap worth drawing, so the upload is not
	// cancelled by the stint ending or the app closing — it is only bounded.
	// Stop waits for it, which is why the bound is short.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), PostTimeout)
	defer cancel()
	h := c.hostOf()
	if h == nil {
		return
	}
	res, err := h.Do(ctx, http.MethodPost, "/laps", bytes.NewReader(body),
		http.Header{"Content-Type": {"application/json"}})
	if err != nil {
		c.fail(fmt.Sprintf("lap %d could not be sent", e.Lap))
		c.log().Warn("a lap could not be sent to be drawn", "lap", e.Lap, "reason", err.Error())
		return
	}
	defer res.Body.Close() //nolint:errcheck // a read answer.

	if res.StatusCode != http.StatusOK {
		why, _ := io.ReadAll(io.LimitReader(res.Body, 2048))
		c.fail(fmt.Sprintf("the plugin would not draw lap %d: %d", e.Lap, res.StatusCode))
		c.log().Warn("a lap was refused", "lap", e.Lap, "status", res.StatusCode,
			"reason", strings.TrimSpace(string(why)))
		return
	}
	var out drawn
	if err := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out); err != nil {
		c.fail(fmt.Sprintf("lap %d was drawn but the answer could not be read", e.Lap))
		c.log().Warn("the answer could not be read", "lap", e.Lap, "reason", err.Error())
		return
	}

	c.mu.Lock()
	c.keep(chart{Lap: e.Lap, LapMs: e.LapMs, Kind: e.LapKind, SVG: out.SVG, PNG: out.PNG})
	c.failed = ""
	c.mu.Unlock()
	c.log().Info("a lap was drawn", "lap", e.Lap, "points", out.Points, "chart", out.PNG)
	c.show()
}

// keep files a drawn lap, newest lap first. Laps are drawn while the next one
// is being driven, so two can come back out of order; the list is the driver's
// laps and reads by lap number, not by whichever the server finished first. A
// lap drawn twice replaces itself, as it does on the server. Called with the
// lock held.
func (c *Companion) keep(ch chart) {
	kept := make([]chart, 0, len(c.charts)+1)
	for _, old := range c.charts {
		if old.Lap != ch.Lap {
			kept = append(kept, old)
		}
	}
	kept = append(kept, ch)
	sort.Slice(kept, func(i, j int) bool { return kept[i].Lap > kept[j].Lap })
	if len(kept) > KeepCharts {
		kept = kept[:KeepCharts]
	}
	c.charts = kept
}

// cornersOf is the lap's corners as the map wants them.
func cornersOf(corners []clientplugin.Corner) []reportCorner {
	if len(corners) == 0 {
		return nil
	}
	out := make([]reportCorner, 0, len(corners))
	for _, c := range corners {
		out = append(out, reportCorner{Turn: c.Turn, ApexPct: c.ApexPct})
	}
	return out
}

// fail records why the last lap was not drawn, for the page.
func (c *Companion) fail(why string) {
	c.mu.Lock()
	c.failed = why
	c.mu.Unlock()
	c.show()
}

// hostOf is the app, or nothing before Start.
func (c *Companion) hostOf() clientplugin.Host {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.host
}

func (c *Companion) log() *slog.Logger {
	if h := c.hostOf(); h != nil {
		return h.Log()
	}
	return slog.New(slog.DiscardHandler)
}

// show writes the status line and the page.
func (c *Companion) show() {
	c.mu.Lock()
	h := c.host
	status := "no laps drawn yet"
	switch {
	case c.failed != "":
		status = c.failed
	case len(c.charts) == 1:
		status = "1 lap drawn"
	case len(c.charts) > 1:
		status = fmt.Sprintf("%d laps drawn", len(c.charts))
	}
	page := c.render(h)
	c.mu.Unlock()
	if h == nil {
		return
	}
	h.Status(status)
	h.Page(page)
}

// render is the page: every lap of the stint that was drawn, newest first,
// with where to look at it. Called with the lock held.
func (c *Companion) render(h clientplugin.Host) string {
	var b strings.Builder
	if c.failed != "" {
		b.WriteString("<p class=\"bad\">" + html.EscapeString(c.failed) + "</p>\n")
	}
	if len(c.charts) == 0 {
		b.WriteString("<p class=\"muted\">A chart is drawn for every lap you finish. The first one appears at " +
			"the end of your first full lap.</p>\n")
		return b.String()
	}
	server := ""
	if h != nil {
		server = strings.TrimSuffix(h.Server(), "/")
	}
	if all := c.compareAddress(server); all != "" {
		b.WriteString("<p><a href=\"" + html.EscapeString(all) + "\" target=\"_blank\" rel=\"noreferrer\">" +
			"Every lap of this stint, overlaid</a></p>\n")
	}
	b.WriteString("<ul>\n")
	for _, ch := range c.charts {
		b.WriteString("  <li>Lap " + fmt.Sprint(ch.Lap))
		if ch.LapMs > 0 {
			b.WriteString(", " + lapTime(ch.LapMs))
		}
		if ch.Kind != "" && ch.Kind != wire.KindClean {
			b.WriteString(" (" + html.EscapeString(string(ch.Kind)) + ")")
		}
		if addr := address(server, ch.PNG); addr != "" {
			b.WriteString(" — <a href=\"" + html.EscapeString(addr) + "\" target=\"_blank\" rel=\"noreferrer\">chart</a>")
		}
		b.WriteString("</li>\n")
	}
	b.WriteString("</ul>\n")
	return b.String()
}

// compareAddress is where the stint's laps are overlaid, or "" with fewer than
// two laps to overlay. The plugin takes two to four. Called with the lock held.
func (c *Companion) compareAddress(server string) string {
	if len(c.charts) < 2 || c.stint.ID == "" {
		return ""
	}
	var q strings.Builder
	q.WriteString("/plugin/" + Name + "/charts?")
	for i, ch := range c.charts {
		if i == MaxOverlaid {
			break
		}
		if i > 0 {
			q.WriteString("&")
		}
		// The plugin's own addresses name a lap "<stint>/<lap>", slash and
		// all, so the slash is left as it writes it.
		fmt.Fprintf(&q, "lap=%s/%d", c.stint.ID, ch.Lap)
	}
	return server + q.String()
}

// MaxOverlaid is how many laps the plugin will draw on one chart.
const MaxOverlaid = 4

// address is a path the plugin gave, under the server. A plugin that answered
// with an absolute address is left alone.
func address(server, path string) string {
	switch {
	case path == "":
		return ""
	case strings.HasPrefix(path, "http://"), strings.HasPrefix(path, "https://"):
		return path
	case strings.HasPrefix(path, "/"):
		return server + path
	default:
		return server + "/" + path
	}
}

// lapTime is a lap time as a person reads it.
func lapTime(ms int) string {
	if ms <= 0 {
		return ""
	}
	m, rest := ms/60000, ms%60000
	if m == 0 {
		return fmt.Sprintf("%.3f", float64(rest)/1000)
	}
	return fmt.Sprintf("%d:%06.3f", m, float64(rest)/1000)
}

// Resample thins a lap to at most n points, evenly by distance round the lap,
// keeping the first and the last. A chart is drawn from what this leaves, and
// a chart is fourteen hundred pixels wide: the points beyond that are bytes
// nobody can see.
func Resample(pts []wire.TracePoint, n int) []wire.TracePoint {
	if n < MinPoints || len(pts) <= n {
		return pts
	}
	out := make([]wire.TracePoint, 0, n)
	last := len(pts) - 1
	for i := range n {
		out = append(out, pts[i*last/(n-1)])
	}
	return out
}

var _ clientplugin.Companion = (*Companion)(nil)
