package visual_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pacenote-sim/clientplugin"
	"github.com/pacenote-sim/clientplugin/clientplugintest"
	"github.com/pacenote-sim/protocol/wire"

	visual "github.com/pacenote-sim/client-visual-telemetry"
)

// The plugin's POST /laps, from its README: what it takes and what it answers.

// lastPage is what the window is showing.
func lastPage(h *clientplugintest.Host) string {
	pages := h.Pages()
	if len(pages) == 0 {
		return ""
	}
	return pages[len(pages)-1]
}

func aTrace(n int) []wire.TracePoint {
	out := make([]wire.TracePoint, 0, n)
	for i := range n {
		out = append(out, wire.TracePoint{
			OffsetMs: i * 50, SpeedKmh: 100 + i%80, Throttle: 100, Gear: 4, RPM: 7000,
			DistPct: i * 1000 / n, La: 5043700 + i, Lo: 597140 + i,
		})
	}
	return out
}

func aStint() clientplugin.Stint {
	return clientplugin.Stint{
		ID: "7f0c2e1a", Session: wire.SessionQualifying, Track: "Okayama International Circuit",
		Car: "Mazda MX-5 Cup", TrackLengthM: 3650, Sectors: []float64{0, 0.26, 0.51, 0.69},
	}
}

func aLap(lap, ms int) *clientplugin.LapCompleted {
	return &clientplugin.LapCompleted{
		At: time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC), Stint: aStint(), Lap: lap, LapMs: ms,
		LapKind: wire.KindClean, Trace: aTrace(5000),
		Corners: []clientplugin.Corner{{Turn: 1, ApexPct: 66, ApexKmh: 80}, {Turn: 2, ApexPct: 136, ApexKmh: 70}},
	}
}

// The manifest and the code agree, and the companion registered itself.
func TestItIsAPlugin(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	m, err := clientplugin.LoadManifest(".")
	r.NoError(err)
	r.Equal(visual.Name, m.Name)
	r.Equal(visual.Name, m.ServerPlugin)
	r.Equal(clientplugin.KindCompanion, m.Kind)
	_, ok := clientplugin.Default.Companion(visual.Name)
	r.True(ok, "init() registered it")
}

// A finished lap is sent to be drawn, and the chart it was drawn to is on the
// companion's page with its lap time.
func TestALapIsSentToBeDrawn(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	posted := make(chan map[string]any, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /laps", func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		posted <- body
		lap, _ := body["lap"].(float64)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"lap": fmt.Sprintf("7f0c2e1a/%d", int(lap)), "points": 4096,
			"svg": "/plugin/visual-telemetry/charts/7f0c2e1a/1.svg",
			"png": "/plugin/visual-telemetry/charts/7f0c2e1a/1.png",
		})
	})
	c := visual.New()
	h := clientplugintest.NewHost(t, mux)
	r.NoError(c.Start(ctx, h))
	t.Cleanup(func() { _ = c.Stop() })

	r.NoError(clientplugintest.Deliver(ctx, c, &clientplugin.StintStarted{At: time.Now(), Stint: aStint()}))
	r.Contains(lastPage(h), "A chart is drawn for every lap")

	r.NoError(clientplugintest.Deliver(ctx, c, aLap(1, 98456)))
	r.NoError(c.Stop(), "stopping waits for the lap to be drawn")
	body := <-posted

	// Everything the plugin's contract asks for.
	r.Equal("7f0c2e1a", body["stint_id"])
	r.EqualValues(1, body["lap"])
	r.EqualValues(98456, body["lap_ms"])
	r.Equal("qualifying", body["session"])
	r.Equal("Okayama International Circuit", body["track"])
	r.Equal("Mazda MX-5 Cup", body["car"])
	r.EqualValues(3650, body["track_length_m"])
	r.Len(body["sectors"], 4)
	corners, _ := body["corners"].([]any)
	r.Len(corners, 2)
	first, _ := corners[0].(map[string]any)
	r.EqualValues(1, first["turn"])
	r.EqualValues(66, first["apex_pct"])
	_, speed := first["apex_kmh"]
	r.False(speed, "the map wants where a corner is, not how it was taken")

	// The lap of five thousand points went as four thousand and ninety-six,
	// the most the plugin draws from, first and last kept.
	trace, _ := body["trace"].([]any)
	r.Len(trace, visual.MaxPoints)
	head, _ := trace[0].(map[string]any)
	r.EqualValues(0, head["t"])

	// And the page says where to look.
	page := lastPage(h)
	r.Contains(page, "Lap 1")
	r.Contains(page, "1:38.456")
	r.Contains(page, h.Server()+"/plugin/visual-telemetry/charts/7f0c2e1a/1.png")
	r.Contains(h.Statuses()[len(h.Statuses())-1], "1 lap drawn")
}

// The page lists the laps newest first, keeps the stint's own, and offers the
// overlay once there are two.
func TestThePageListsTheStint(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	var n atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("POST /laps", func(w http.ResponseWriter, req *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(req.Body).Decode(&body)
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"png": "/plugin/visual-telemetry/charts/7f0c2e1a/" + strings.TrimSpace(req.URL.Path) + "x.png",
			"svg": "/plugin/visual-telemetry/charts/7f0c2e1a/x.svg", "points": 100,
		})
	})
	c := visual.New()
	h := clientplugintest.NewHost(t, mux)
	r.NoError(c.Start(ctx, h))

	r.NoError(clientplugintest.Deliver(ctx, c, &clientplugin.StintStarted{At: time.Now(), Stint: aStint()}))
	for lap := 1; lap <= 3; lap++ {
		r.NoError(clientplugintest.Deliver(ctx, c, aLap(lap, 98000+lap)))
	}
	r.NoError(c.Stop())
	r.EqualValues(3, n.Load())

	page := lastPage(h)
	r.Less(strings.Index(page, "Lap 3"), strings.Index(page, "Lap 2"), "newest lap first")
	r.Less(strings.Index(page, "Lap 2"), strings.Index(page, "Lap 1"), "however they came back")
	r.Contains(page, "Every lap of this stint, overlaid")
	r.Contains(page, "lap=7f0c2e1a/3&amp;lap=7f0c2e1a/2&amp;lap=7f0c2e1a/1",
		"the laps overlay in the order the plugin names them")
	r.Contains(h.Statuses()[len(h.Statuses())-1], "3 laps drawn")

	// A new stint starts the list again.
	r.NoError(clientplugintest.Deliver(ctx, c, &clientplugin.StintStarted{At: time.Now(), Stint: aStint()}))
	r.NotContains(lastPage(h), "Lap 3")
}

// A plugin that refuses a lap, or cannot be reached, is said on the page once
// and does not stop the next lap being drawn.
func TestALapThatCannotBeDrawn(t *testing.T) {
	t.Parallel()
	r := require.New(t)
	ctx := context.Background()

	var refuse atomic.Bool
	refuse.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /laps", func(w http.ResponseWriter, req *http.Request) {
		_, _ = io.Copy(io.Discard, req.Body)
		if refuse.Load() {
			http.Error(w, "the lap has too few points", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"png":"/plugin/visual-telemetry/charts/a/1.png","points":9}`))
	})
	c := visual.New()
	h := clientplugintest.NewHost(t, mux)
	r.NoError(c.Start(ctx, h))

	r.NoError(clientplugintest.Deliver(ctx, c, &clientplugin.StintStarted{At: time.Now(), Stint: aStint()}))
	r.NoError(clientplugintest.Deliver(ctx, c, aLap(1, 98000)))
	r.NoError(c.Stop())
	r.Contains(lastPage(h), "would not draw lap 1: 400")

	// The next lap goes through, and the page stops complaining.
	refuse.Store(false)
	r.NoError(c.Start(ctx, h))
	r.NoError(clientplugintest.Deliver(ctx, c, aLap(2, 97000)))
	r.NoError(c.Stop())
	r.NotContains(lastPage(h), "would not draw")
	r.Contains(lastPage(h), "Lap 2")

	// A lap too short to draw is not sent at all.
	short := aLap(3, 96000)
	short.Trace = short.Trace[:1]
	r.NoError(c.Start(ctx, h))
	r.NoError(clientplugintest.Deliver(ctx, c, short))
	r.NoError(c.Stop())
	r.NotContains(lastPage(h), "Lap 3")
}

// A lap is thinned evenly, keeping its ends, and a lap already small enough is
// sent as it was driven.
func TestResampleKeepsTheEnds(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	pts := aTrace(1000)
	got := visual.Resample(pts, 100)
	r.Len(got, 100)
	r.Equal(pts[0], got[0])
	r.Equal(pts[len(pts)-1], got[len(got)-1])
	for i := 1; i < len(got); i++ {
		r.Greater(got[i].OffsetMs, got[i-1].OffsetMs, "the lap went backwards")
	}

	r.Equal(pts, visual.Resample(pts, 1000), "already small enough")
	r.Equal(pts, visual.Resample(pts, 5000))
	r.Equal(pts, visual.Resample(pts, 1), "a lap cannot be drawn from one point, so it is left alone")
	r.Empty(visual.Resample(nil, 10))
	two := visual.Resample(pts, 2)
	r.Len(two, 2)
	r.Equal(pts[len(pts)-1], two[1])
}

// The small pieces the page is built from, each on its own: a lap time as a
// person reads it, an address under the server or already absolute, and a
// companion that has not started yet.
func TestThePiecesOfThePage(t *testing.T) {
	t.Parallel()
	r := require.New(t)

	r.Equal("1:38.456", visual.LapTime(98456))
	r.Equal("59.999", visual.LapTime(59999), "a lap under the minute is seconds")
	r.Equal("59.400", visual.LapTime(59400))
	r.Empty(visual.LapTime(0))
	r.Empty(visual.LapTime(-1))
	r.Equal("2:00.000", visual.LapTime(120000))

	const server = "https://pacenote.example"
	r.Equal(server+"/plugin/visual-telemetry/charts/a/1.png", visual.Address(server, "/plugin/visual-telemetry/charts/a/1.png"))
	r.Equal(server+"/charts/a/1.png", visual.Address(server, "charts/a/1.png"), "a path without its slash still lands")
	r.Equal("https://cdn.example/a.png", visual.Address(server, "https://cdn.example/a.png"), "left alone")
	r.Equal("http://cdn.example/a.png", visual.Address(server, "http://cdn.example/a.png"))
	r.Empty(visual.Address(server, ""))

	// A companion nobody started says nothing and breaks nothing.
	c := visual.New()
	r.Equal(visual.Name, c.Name())
	r.NoError(c.Stop())
	r.NoError(c.Notify(context.Background(), &clientplugin.LapCompleted{Lap: 1}), "no app, nothing to draw on")
	r.Len(c.Wants(), 2)
}

// What a lap costs to send. It happens once a lap, but the trace is the whole
// lap at the simulator's rate and the thinning is what keeps it from being a
// few hundred kilobytes of points a chart cannot show.
func BenchmarkThinningALap(b *testing.B) {
	trace := aTrace(6000)
	b.ReportAllocs()
	for b.Loop() {
		if got := visual.Resample(trace, visual.MaxPoints); len(got) != visual.MaxPoints {
			b.Fatalf("thinned to %d", len(got))
		}
	}
}
