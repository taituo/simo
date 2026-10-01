# simo

simo is a math-driven simulator for logs, metrics and events. It generates realistic data for fleets of virtual devices with stochastic processes, state machines and graph propagation, without machine learning in the generation path. An LLM can write a compact seed spec; the engine expands it deterministically into as much data as you need, and serves it through a CLI and (later) an MCP server.

**Status:** design done, MVP under construction. See the [design document](docs/design.md).

## Ideas in one paragraph

Every random number is a pure function of `(seed, device, stream, tick)` (Philox counter-based RNG), so output is reproducible and can be regenerated instead of stored. Each device has a hidden state machine and a hidden health value; its logs and metrics are noisy observations of that hidden state, which makes the data diagnosable instead of random. Events are stored as fixed-size numeric records, and text is rendered only when someone reads it.
