# simo

simo is a math-driven simulator for logs, metrics and events. It generates realistic data for fleets of virtual devices with stochastic processes and state machines, with no machine learning in the generation path. A compact seed file (which an LLM can write) expands deterministically into as much data as you need.

Every random number is a pure function of `(seed, device, stream, tick)`, using the Philox counter-based generator. That makes output reproducible, parallel, and cheap to regenerate instead of storing it. Each device has a hidden state machine and a hidden health value. Its logs and metrics are noisy observations of that hidden state, which makes the data diagnosable rather than random.

The full design is in [docs/design.md](docs/design.md).

## Status

This is the phase 1 MVP: a CPU reference engine with a CLI. It runs the two example seeds end to end.

| Works now | Comes later (see the roadmap in the design) |
| --- | --- |
| Seed parsing with strict schema checks, validation errors with YAML paths, and a linter | SQLite store, checkpoints and lazy regeneration (phase 2) |
| Philox RNG, Poisson, OU metrics, Fourier seasonality, Zipf vocabularies | MCP server and the LLM seed-authoring loop (phase 3) |
| State machines with rate, health-dependent, timed and command transitions | Topology propagation, Hawkes bursts, clocks and skew, config system (phase 4) |
| Scheduled faults with ground-truth cause ids | Interactive devices and a stepped clock (phase 5) |
| A simulated responder that restarts broken devices | GPU backend (phase 6) |
| 64-byte binary records, rendered on read as text, JSON lines or RFC 5424 syslog | Sinks: OTLP, Prometheus, MQTT (phase 7) |
| Conformance tests against qmuntal/stateless and RFC 9293 | CPU ML module |

## Quick start

Requires Go 1.24 or later.

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

`run --out DIR` writes these files:

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
- **Conformance:**
  - Transition traces are replayed through [qmuntal/stateless](https://github.com/qmuntal/stateless) machines built from each seed, and from an independent RFC 9293 table for the TCP example.
  - Every log line is checked to be legal for its device's state.
  - Negative tests confirm that the referee catches illegal transitions and illegal lines.

## Layout

```text
cmd/simo/             CLI: validate, preview, run, logs
internal/rng/         Philox4x32-10, key derivation
internal/dist/        Poisson, inverse normal, Zipf, parameterised distributions
internal/spec/        seed types, YAML loading, expression parsers, validation and lint
internal/tmpl/        message template parser and generator table
internal/compile/     seed to IR: fleet expansion, jitter, thresholds, faults
internal/ir/          compiled world shared by all backends
internal/engine/      CPU reference engine
internal/render/      records to text, JSON lines and syslog
internal/record/      64-byte record encoding
internal/conformance/ state-machine referee built on qmuntal/stateless
examples/             example seeds
testdata/golden/      record-stream hashes
docs/design.md        design document
```

## Known gaps

- There are no checkpoints yet, so every window is simulated from tick 0. That is fast enough for weeks of a few hundred devices: a simulated week of the retail example takes about 15 s on 2 cores.
- Output is bit-identical on the same CPU architecture. Cross-architecture identity needs the shared polynomial `log`/`exp` planned for the GPU phase. The golden hashes were produced on linux/amd64.
- `excite` (Hawkes bursts) is accepted in seeds but ignored until phase 4. The validator warns about it.

## Licence

Not chosen yet.
