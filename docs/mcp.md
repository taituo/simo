# simo over MCP

`simo serve` exposes worlds and seed authoring to agents over the Model Context Protocol. It speaks JSON-RPC over stdio by default, or streamable HTTP at `/mcp` with `--http ADDR`.

```bash
simo serve --role observer --world worlds/retail                   # an agent under test: one world, read only
simo serve --role author,observer --worlds worlds                  # an LLM that writes seeds and builds worlds
simo serve --role observer --worlds worlds --http 127.0.0.1:8765   # HTTP at http://127.0.0.1:8765/mcp
```

Most MCP clients accept a server entry like this:

```json
{
  "mcpServers": {
    "simo": { "command": "simo", "args": ["serve", "--role", "observer", "--world", "worlds/retail"] }
  }
}
```

## Roles and tools

An agent under test gets only `observer`. It sees what an on-call engineer would see: never hidden state, health, fault labels or the answer key.

| Role | Tool | Returns |
| --- | --- | --- |
| observer | `describe_world` | Time range, devices per class, event catalog with message templates, metrics. Start here. |
| observer | `summarize_logs` | Event counts grouped by event, level, device, site or class, optionally per time step. |
| observer | `search_logs` | Log lines in time order, filtered by device, level, event and text; up to 200 per call, with `next_cursor`. |
| observer | `get_metrics` | Min, avg and max of a device's metrics per step. |
| observer | `list_changes` | Operator and manual actions, such as restarts. |
| observer | `list_devices` | Devices with class and site, paged. |
| observer, author | `list_worlds` | Worlds the server can read. |
| author | `get_seed_schema` | JSON Schema of the seed format, with descriptions. |
| author | `list_examples`, `get_example` | Commented example seeds. |
| author | `validate_seed` | Every error and warning with its YAML path, or a summary when valid. |
| author | `preview_seed` | Counts, time in state and sample lines for a short window. |
| author | `create_world` | Builds a world that observer tools can read. |
| admin | `get_truth` | The answer key: faults with cause, window and devices. |

Resources: `simo://schema/seed`, `simo://examples/{name}` and `simo://worlds/{name}/summary`.

Time arguments accept RFC 3339 timestamps copied from log lines, `day2 09:15` (day 1 is the first), `09:15`, or offsets like `26h`. Arguments are strict: a misspelled argument is reported back so the model can correct it. `preview_seed` and `create_world` refuse windows above `--max-device-ticks` (default 2e9 devices × ticks).

## The author loop, as run

The phase 3 exit gate is "an LLM turns a brief into a valid seed". Claude ran it against the built binary over stdio with this brief (the counts below are from that run, before the responder fix it led to):

> A web API on Kubernetes: checkout and catalog services, 6 pods each. Request logs with latencies and status codes, an afternoon traffic peak. Pods leak memory and get OOM-killed; once a leak is severe they crash-loop with back-off until on-call does a rollout restart. A bad checkout deploy on day 1 at 14:00 causes 30 minutes of 503s.

1. `list_examples` and `get_seed_schema` for the format.
2. `validate_seed` on the first draft: valid.
3. `preview_seed` for day 1, 13:00 to 16:00, and for day 2. The deploy showed (about 1,050 503s against a baseline of about 37 per hour) and crash loops emerged as health fell. The preview also showed two realism problems, which the author fixed: "Started container" was logged on entering back-off instead of on running again, and `POST` returned 304.
4. `create_world` built two simulated days (3.1 million records) in about 16 s.
5. As an observer, `summarize_logs` per 30 minutes showed the 14:00 bucket with 971 errors on checkout pods against 13 to 21 in the other half hours, `search_logs` returned the 503 lines, and `list_changes` showed the rollout restarts.

The loop also found an engine flaw. The simulated responder acted only if a pod was still in `CrashLoopBackOff` when its delay ended, so it kept missing pods that were briefly running between crashes. One pod was OOM-killed 61 times in a day. The responder now runs its command whenever the command is allowed in the device's current state (engine 0.2.0). The seed is [examples/k8s-web-api.yaml](../examples/k8s-web-api.yaml).

Two gaps it showed, for phase 4: a deploy cannot write its own log line (the cause shows only through its effects), and metrics cannot follow hidden health, so a leak does not show in memory before the OOM kill.

## Implementation note

The protocol layer (`internal/mcp`) is a small implementation of initialize, ping, tools and resources over stdio and streamable HTTP (JSON responses, no event streams; the Origin header is checked to stop DNS rebinding). It answers protocol versions 2025-11-25, 2025-06-18, 2025-03-26 and 2024-11-05. The official Go SDK was the plan. Its current releases need Go 1.25 and modules hosted at golang.org, which the build environment could not fetch. Tool handlers are plain functions from JSON arguments to text, so moving to the SDK later only touches `internal/mcp`.
