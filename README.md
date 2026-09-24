# client-visual-telemetry

The client half of the [visual-telemetry](https://github.com/Pacenote-Sim/visual-telemetry) server
plugin. At the end of every lap it sends the lap as it was driven, and the plugin draws it: speed,
revs, throttle and brake against distance, the sectors marked, the circuit in the corner.

It is a Go module that registers itself as a companion when imported. The client the server builds
imports it when visual-telemetry is ticked, and it does nothing at all unless the server is running
the other half.

## What it does

| When | What |
|---|---|
| `stint.started` | clears the list: a stint's charts are that stint's |
| `lap.completed` | posts the lap to `POST /plugin/visual-telemetry/laps` and keeps where it was drawn |

Its page is the laps of this stint, newest first, each with its time and a link to its chart, and a
link that overlays the last four. Its status line says how many have been drawn.

## What it sends

Everything the plugin's contract asks for: the stint and lap that name the chart, the lap time, the
session, the circuit and the car, the circuit's length, the sectors, the corners for the map, and
the lap itself, thinned to the 4 096 points the plugin draws from.

A lap that has been driven is worth drawing, so an upload is not cancelled when the stint ends or
the app closes; closing waits for it. A lap the plugin will not draw is said once on the page and
not retried.

## Building and testing

```
make check          # format, build, vet, lint, test, coverage, tidy
```

Tested against a fake of the plugin's one route, written from its README. The app is not needed.
`TESTING.md` has the rest.

## Licence

GNU General Public License, version 3 — see `LICENSE` — with the Pacenote Plugin Exception in
`LICENSE-EXCEPTION`, the same one the client carries. It lets this plugin be compiled into a client
alongside plugins under other licences, including closed ones, and lets that client be handed out
under those plugins' own terms while this plugin's part stays GPL. The contract it is built on
(`github.com/pacenote-sim/clientplugin`) is Apache-2.0.
