# HIVE Project State

Last updated: 2026-05-22

Branch: `cursor/gap-fit-analysis-2e94`  
Pull request: [#2 — Production hardening](https://github.com/EAGLE605/agent-comms/pull/2)  
Base: `main` @ `d215262`

## Snapshot

| Area | Status |
| --- | --- |
| Tests | **145 passing** (`python3 -m pytest tests/ --timeout=30 -q`) |
| Core package | `hive-protocol` v1.0.0, Python 3.11+ |
| Write paths | **Split** — shell/runner still append legacy UUID JSONL; `HiveBoard` dual-writes SQLite + JSONL |
| Config | **Unified** via `hive.config` and `HIVE_*` env vars |
| Lifecycle | **Reducer shipped** — `hive.coordination.lifecycle` |
| Runner deps | **Partial** — JSONL `depends_on` gate in `agent-runner.sh` |
| Dashboard | Localhost default + token auth for non-loopback bind |
| CI / lockfile | Not yet configured |
| Identity / signing | Not yet implemented |

## Shipped on this branch

### P0a — Runtime config and channel safety (`492d2cf`)

- `hive/config.py` — shared defaults for channels dir, DB path, channel name validation, path traversal and symlink guards
- Aligned `comms.sh`, `agent-runner.sh`, `dashboard/server.py`, `HiveBoard`, JSONL transport, MCP entrypoint
- Dashboard: `127.0.0.1` default bind, explicit CORS, bearer token enforcement when exposed
- `tests/test_runtime_hardening.py` — 28 tests for config, validation, dashboard guards, shell safety

### P0b — Lifecycle reducer and dependency gates (`6e7db9a`)

- `hive/coordination/lifecycle.py` — canonical task state from HIVE + legacy events
- A2A-aligned states: `SUBMITTED`, `WORKING`, `BLOCKED`, `COMPLETE`, `FAILED`, `CANCELED`, `VERIFIED`
- `hive/coordination/dag.py` — readiness via `is_task_ready()`
- `SQLiteTransport.query(unlimited=True)` — uncapped legacy event scans for reducer
- `agent-runner.sh` — skips tasks with unsatisfied `data.depends_on`
- `tests/test_lifecycle_reducer.py` — 19 tests

### Analysis artifact (`c11d0d0`)

- `docs/2026-05-21-hive-gap-fit-analysis.md` — gap matrix, architecture decisions, implementation sequence

## Architecture (current)

```
┌─────────────────┐     ┌──────────────────┐     ┌─────────────────┐
│ comms.sh        │     │ agent-runner.sh  │     │ hive.mcp.server │
│ (legacy JSONL)  │     │ (legacy JSONL)   │     │ (HiveBoard)     │
└────────┬────────┘     └────────┬─────────┘     └────────┬────────┘
         │                       │                          │
         └───────────────────────┼──────────────────────────┘
                                 ▼
                    HIVE_CHANNELS_DIR/*.jsonl
                                 │
         ┌───────────────────────┴───────────────────────┐
         ▼                                               ▼
  HiveBoard (SQLite primary)                    dashboard/server.py
  + JSONL projection                          (reads JSONL channels)
```

**Single source of truth for task readiness (Python path):** `get_task_state()` / `is_task_ready()` in `hive.coordination.lifecycle`.

**Known split-brain risk:** JSONL written by shell may not appear in SQLite until import/repair exists.

## Environment

| Variable | Purpose |
| --- | --- |
| `HIVE_CHANNELS_DIR` | Shared JSONL channel directory (canonical) |
| `COMMS_CHANNELS` | Legacy alias for `HIVE_CHANNELS_DIR` |
| `HIVE_DB_PATH` | SQLite path for HiveBoard / MCP |
| `COMMS_AGENT` | Agent identity (`name/role`) — required for comms |
| `HIVE_DASHBOARD_HOST` | Dashboard bind host (default `127.0.0.1`) |
| `HIVE_DASHBOARD_PORT` | Dashboard port (default `7842`) |
| `HIVE_DASHBOARD_TOKEN` | Required for non-loopback dashboard bind |

## Development

```bash
# Install test + dashboard deps
pip3 install pytest pytest-timeout -r dashboard/requirements.txt

# Run full suite
python3 -m pytest tests/ --timeout=30 -q

# Start dashboard (localhost)
HIVE_CHANNELS_DIR=/tmp/hive-channels HIVE_DB_PATH=/tmp/hive.db python3 -m dashboard.server
```

## Ranked backlog

| Priority | Item | Notes |
| --- | --- | --- |
| **P0b** | Legacy JSONL → SQLite import/repair | Unblocks unified writer; fixes split-brain |
| **P0b** | Unified Python writer for shell/runner | Route all writes through HiveBoard after import |
| **P0b** | Task Protocol Profile + layered validation | Separate core Cell vs task/message profile rules |
| **P0c** | MCP write gate + identity/signing | Prevent forged claims on live bus |
| **P0c** | Trusted runner execution policy | Sandbox, tool allowlists, audit logs |
| **P1** | Dashboard structured tasks | Replace TASK-N heuristics with lifecycle reducer |
| **P1** | MCP contract modernization | outputSchema, structured content, annotations |
| **P1** | CI + lockfile | GitHub Actions, pinned deps, integration smoke tests |
| **P2** | OpenAPI / AsyncAPI / Arazzo / llms.txt | Agent-readable contracts |

## Recommended next slice

**Legacy JSONL import/repair** — lowest-risk path to converging SQLite and JSONL without identity/signing yet.

## References

- Gap-fit analysis: `docs/2026-05-21-hive-gap-fit-analysis.md`
- Protocol spec: `PROTOCOL.md`
- Fleet ops post-mortem: `FLEET-OPS.md`
- Coordination modules: `hive/coordination/AGENTS.md`
