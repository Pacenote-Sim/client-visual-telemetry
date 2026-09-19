# Testing the visual-telemetry companion

`make` runs what CI runs, in order: format, build, vet, lint, the suite with the race detector and
shuffled order, coverage over 90 %, every benchmark once, and `go mod tidy` a no-op.

```
make            # everything, in order
make bench      # what thinning a lap costs
```

## What the suite asserts

| | |
|---|---|
| what it sends | every field the plugin's contract asks for, the corners as the map wants them, and the lap thinned to the points the plugin draws from |
| the page | the laps of the stint newest first, each with its time and its chart, the overlay once there are two, and the list cleared when a new stint starts |
| out of order | two laps drawn at once come back in whichever order, and the page is still in lap order |
| what goes wrong | a lap the plugin refuses, a plugin that cannot be reached, and a lap too short to draw — each said once on the page, none of them stopping the next lap |
| thinning | the ends kept, the middle even, and a lap already short enough sent as it was driven |

It runs against a fake of the plugin's one route, written from that plugin's README.

## What it costs

Thinning a six-thousand-point lap to the four thousand the plugin draws from: 57 µs, one
allocation. It happens once a lap, while the driver is on the next one.
