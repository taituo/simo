# simo

simo is a math-driven simulator for logs, metrics and events. It generates realistic data for fleets of virtual devices with stochastic processes and state machines, with no machine learning in the generation path. A compact seed file (which an LLM can write) expands deterministically into as much data as you need.

Every random number is a pure function of `(seed, device, stream, tick)`, using the Philox counter-based generator. That makes output reproducible, parallel, and cheap to regenerate instead of storing it. Each device has a hidden state machine and a hidden health value. Its logs and metrics are noisy observations of that hidden state, which makes the data diagnosable rather than random.

The full design is in [docs/design.md](docs/design.md).

## Status

Phases 1 and 2 are done: a CPU reference engine, a CLI, and a SQLite world store that reads any time window back without re-simulating from the start.

| Works now | Comes later (see the roadmap in the design) |
| --- | --- |
| Seed parsing with strict schema checks, validation errors with YAML paths, and a linter | MCP server and the LLM seed-authoring loop (phase 3) |
| Philox RNG, Poisson, OU metrics, Fourier seasonality, Zipf vocabularies | Topology propagation, Hawkes bursts, clocks and skew, config system (phase 4) |
| State machines with rate, health-dependent, timed and command transitions | Interactive devices and a stepped clock (phase 5) |
| Scheduled faults with ground-truth cause ids, and a simulated responder | GPU backend (phase 6) |
| 64-byte binary records, rendered on read as text, JSON lines or RFC 5424 syslog | Sinks: OTLP, Prometheus, MQTT (phase 7) |
| SQLite worlds: hourly checkpoints, 1-minute rollups and metrics, transitions, truth, optional raw events per day | CPU ML module |
| Conformance tests against qmuntal/stateless and RFC 9293 | |

## Quick start

Requires Go 1.24 or later and a C compiler: the SQLite driver, [mattn/go-sqlite3](https://github.com/mattn/go-sqlite3), uses cgo.

```bash
go build -o simo ./cmd/simo

./simo validate examples/retail-pos.yaml
./simo preview  examples/retail-pos.yaml --from "day2 09:00" --for 2h
./simo run      examples/tcp-connections.yaml --for 5m --format syslog | head

# Write a simulated week as binary records plus ground truth, then query it.
./simo run  examples/retail-pos.yaml --out out/week --trace-states --metrics 1m
./simo logs out/week --from "day2 09:10" --to "day2 10:00" --device "pos-07-*" --level WARN
```

The last command shows the scheduled fault: silence, then a burst of card-reader timeouts at site 7 starting at 09:15.

```
2026-10-06T09:15:42.934Z pos-07-05 WARN card reader timeout after 3801ms
2026-10-06T09:15:57.918Z pos-07-02 WARN card reader timeout after 3792ms
2026-10-06T09:16:29.494Z pos-07-02 WARN card reader timeout after 3466ms
```

## Worlds: store once, read any window

`run --db DIR` builds a world: a SQLite store that keeps the recipe rather than the meal. It holds the seed, hourly engine checkpoints, 1-minute event counts and metrics, every state transition, and the ground truth. Raw events are stored only for days you materialize. Reading a window that is not stored loads the nearest checkpoint and regenerates just that window, and the result is byte-identical to stored events.

```bash
./simo run examples/retail-pos.yaml --db worlds/retail          # a week in about 20 s, 62 MB
./simo logs  worlds/retail --from "day7 23:00" --for 1h --level ERROR --explain
./simo stats worlds/retail --from "day2 08:00" --to "day2 11:00" --step 1h --by level --device "pos-07-*"
./simo metrics worlds/retail --device pos-07-02 --from "day2 09:00" --for 1h --step 15m
./simo materialize worlds/retail --from day2 --to day3          # store day 2's raw events
./simo logs  worlds/retail --from "day2 09:14" --for 2m --mode sql
./simo devices worlds/retail --site 7
./simo truth   worlds/retail                                    # the answer key; keep it from agents under test
```

Reading the last hour of the simulated week takes 0.17 s, because it starts from the 23:00 checkpoint instead of re-simulating seven days. The stats query shows the fault hour at site 7:

```
from              level  count
2026-10-06 08:00  INFO   839
2026-10-06 08:00  WARN   5
2026-10-06 09:00  INFO   1,133
2026-10-06 09:00  WARN   113
2026-10-06 10:00  INFO   1,243
2026-10-06 10:00  WARN   23
```

A world directory holds these files:

| File | Contents |
| --- | --- |
| `seed.yaml` | The seed; its hash must match `world.db` |
| `world.db` | Catalog (classes, states, devices, templates), `checkpoints`, `rollup_1m`, `metrics_1m`, `transitions`, `truth`, and the list of `materialized` days |
| `events/day-0001.db` | Raw events of simulated day 1, in engine order, if materialized |

`logs --mode` chooses how a world is read: `auto` (stored days from SQL, the rest regenerated), `sql` (stored days only) or `regen` (always regenerate). The databases are plain SQLite, so `sqlite3 worlds/retail/world.db` works for ad hoc queries.

## Flat files

`run --out DIR` writes flat files instead of a world:

| File | Contents |
| --- | --- |
| `records.bin` | 64-byte records, sorted by time |
| `seed.yaml` | The seed the records came from |
| `manifest.json` | Engine version, seed hash, window and record hash |
| `truth.jsonl` | Each fault with its cause id, time window and affected devices |
| `transitions.jsonl` | With `--trace-states`: every state change and its cause |
| `metrics.jsonl` | With `--metrics STEP`: metric samples |
| `logs.*` | With an explicit `--format`: rendered lines |

## Writing a seed

A seed describes a world: classes of devices, what states they move between, what they log, how many there are, and what goes wrong. The two files in [examples/](examples) are commented starting points. `simo validate` explains anything it rejects.

| Construct | Example |
| --- | --- |
| Clock | `clock: { start: 2026-10-05T00:00:00Z, duration: 7d, tick: 1s }` |
| Pattern (daily or weekly rhythm) | `store_hours: { fourier: { period: 24h, harmonics: [[1, 0.9, 14h]] } }` |
| Event rate | `rate: 0.03/s * store_hours` |
| Transition laws | `1/2h`, `health_rate(0.5/day, gamma: 6)`, `fixed(90s)`, `on_command(restart)` |
| State effect on an event | `in_state: { degraded: x40 }`, `only_in: [ok, degraded]` |
| Emit on entering a state | `down: { emit: [crash] }` |
| Message template | `"timeout after {ms:uniform(3000, 5000)\|%d}ms"` |
| Slot generators | `hex(n)`, `int(a, b)`, `uniform`, `normal`, `lognormal`, `exp`, `choice(...)`, `vocab`, `ip(cidr)`, `uuid`, `metric(name)`, `seq` |
| Fleet | `{ class: pos_terminal, sites: 20, per_site: uniform(4, 8), name: pos-{site:02}-{n:02} }` |
| Per-device jitter | `jitter: { rate_scale: lognormal(0, 0.3) }` |
| Fault | `{ at: day2 09:15, for: 40m, target: site[7]/pos_terminal[*], effects: [...] }` |
| Simulated responder | `responder: { states: [down], command: restart, delay: exp(30m) }` |

## Tests

```bash
go test ./...
```

The tests check the math as well as the code:

- **Philox** matches the Random123 known-answer vectors.
- **Stationary distribution:** long-run time in each state matches the solution of πQ = 0 for the seed's rate matrix, and mean holding times match 1/rate.
- **Event rates:** counts match rate × time, including the integral of a Fourier pattern.
- **Determinism:** output is identical with 1 or 7 workers, and a time window equals the same slice of a full run.
- **Golden hashes:** the record stream of each example is pinned in `testdata/golden`. If a change is intended, bump `ir.EngineVersion` and run `go test ./internal/engine -run Golden -update`.
- **Checkpoints:** a run resumed from any checkpoint reproduces the full run's records and transitions exactly, and taking checkpoints does not change the output.
- **Store (the phase 2 exit gate):** a window read from stored events, the same window regenerated from a checkpoint, and a plain engine run from tick 0 are byte-identical, including across midnight and with filters. Rollup totals match the records.
- **Conformance:**
  - Transition traces are replayed through [qmuntal/stateless](https://github.com/qmuntal/stateless) machines built from each seed, and from an independent RFC 9293 table for the TCP example.
  - Every log line is checked to be legal for its device's state.
  - Negative tests confirm that the referee catches illegal transitions and illegal lines.

## Layout

```text
cmd/simo/             CLI: validate, preview, run, logs, stats, metrics, devices, materialize, truth
internal/rng/         Philox4x32-10, key derivation
internal/dist/        Poisson, inverse normal, Zipf, parameterised distributions
internal/spec/        seed types, YAML loading, expression parsers, validation and lint
internal/tmpl/        message template parser and generator table
internal/compile/     seed to IR: fleet expansion, jitter, thresholds, faults
internal/ir/          compiled world shared by all backends
internal/engine/      CPU reference engine, checkpoints
internal/store/       SQLite worlds: build, read (SQL or regenerate), rollups, metrics, materialize
internal/render/      records to text, JSON lines and syslog
internal/record/      64-byte record encoding
internal/conformance/ state-machine referee built on qmuntal/stateless
examples/             example seeds
testdata/golden/      record-stream hashes
docs/design.md        design document
```

## Known gaps

- Building a world runs single-pass on the CPU: a simulated week of the retail example (123 devices, 2.4M events) takes about 20 s on 2 cores. Rollups and metrics cover the build window only; reads beyond it regenerate from the last checkpoint.
- Output is bit-identical on the same CPU architecture. Cross-architecture identity needs the shared polynomial `log`/`exp` planned for the GPU phase. The golden hashes were produced on linux/amd64.
- `excite` (Hawkes bursts) is accepted in seeds but ignored until phase 4. The validator warns about it.

## Licence

Not chosen yet.
