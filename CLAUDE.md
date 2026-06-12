# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

Inter-agent communication bus for a fleet of AI CLI agents (Claude Code, Gemini CLI, Codex). Agents
coordinate by appending JSONL cells to shared channel files. A Python package (`hive/`) layers structured
coordination on top: schema'd immutable cells, task lifecycle, lease locking, DAG dependencies, stall
detection, reputation. `AGENTS.md` (root and per-directory) is the authoritative agent-facing
documentation; `PROTOCOL.md` is the canonical command list and cell schema — keep both true when changing
behavior, and read `FLEET-OPS.md` for the postmortem behind the "never-again" rules.

## Commands

```bash
# The four CI gates — all must pass before pushing (.github/workflows/test.yml)
python -m pytest tests/ --timeout=30 -q   # full suite
mypy                                      # strict, scoped to hive/ via pyproject
ruff check .                              # lint (E,F,W,I,UP,B,SIM @ 120 cols)
bash -n comms.sh agent-runner.sh          # shell syntax

# Single test file / single test
python -m pytest tests/test_leases.py -v
python -m pytest tests/test_leases.py::TestLeaseExpiry::test_expired_lease_not_active -v

# Exercise the CLI against a throwaway bus (COMMS_DIR needs hive/ importable inside it)
export COMMS_CHANNELS=/tmp/ch COMMS_DIR=/path/to/this/repo COMMS_AGENT="claude/dev"
bash comms.sh send general "hello" && bash comms.sh read general
```

Dev dependencies: `pip install pytest pytest-timeout mypy ruff` (hive core is stdlib-only;
`fastapi`/`uvicorn` only for `dashboard/`).

## Architecture

**Two data planes share the same channel files.** The raw bus is plain JSONL appends with uuid4 ids —
`comms.sh` messaging commands and `agent-runner.sh` speak it directly, and any process that can append a
file can participate. The HIVE board (`hive/board.py`) is the structured plane: SQLite is the primary,
queryable store and JSONL is a write-only projection for backward compatibility. All board reads go
through SQLite (`hive/transports/sqlite.py`, WAL mode, thread-local connections); never read board state
from the JSONL files.

**Cells are immutable and content-addressed** (`hive/cell.py`): id = `hive:` + SHA-256 of
(type+from+ts+channel+data). State changes are expressed by writing *new* cells that `refs` old ones
(a `release` refs a lease, a `refutation` refs a belief, a `result` refs a contract) — never by
mutating. Terminal task states (COMPLETE/FAILED/CANCELED) are permanent.

**Concurrency invariants** (each one exists because the naive version was a real bug — see git history):

- `board.query(limit=None)` for any correctness-critical scan. The default `limit=100` silently
  truncates; stall detection, reputation, routing, DAG readiness, and evolution all pass `None`.
- `order_by="rowid"` (commit order) for race arbitration, never `ts` — writer clocks can't be trusted.
  Lease acquisition (`hive/coordination/leases.py`) is claim-then-verify: write the claim, re-read,
  keep it only if first in commit order; the loser always commits after the winner so it always sees it.
- Lease TTL is enforced at read time in `is_leased()` — nothing guarantees `expire()` ever runs.
- Signal emitters dedupe: stall detector emits once per stall episode (re-armed by a newer heartbeat);
  `evolve()` skips payload-identical signals.

**Shell layer delegates logic to tested Python.** `comms.sh` (source it, don't execute) and
`agent-runner.sh` keep argument parsing in bash but call stdlib-only scripts in `hive/` for anything
with logic: `runner_scan.py` (claimable-task scan, enforces `depends_on` — a task is withheld until
every dependency has a result cell; unknown dep ids count as unsatisfied) and `fleet_stats.py`
(`comms perf` / `comms fire`). Follow this pattern for new shell features; pass user input to embedded
Python as argv, never by interpolating into the source.

**Dispatch loop** (`agent-runner.sh`): polls a channel, claims via first-claim-in-file-order with a
post-write verify, invokes the agent CLI (`codex` goes through `codex-wrap.py` to strip ANSI/progress
noise), posts the result cell. State file prevents reprocessing across restarts.

**Other entry points:** `hive/mcp/server.py` (stdio JSON-RPC MCP server exposing board ops as tools);
`dashboard/server.py` (FastAPI on :7842, read-only views over channels + hive.db).

## Deployment facts that bite

- The live bus is `C:/Users/Brady.EAGLE/.ai/channels` (Windows/Git Bash deployment) — the repo's
  `channels/` directory is **not** the live bus. Override via `COMMS_CHANNELS` (comms.sh, dashboard)
  / `CHANNELS_DIR` (agent-runner.sh); repo root via `COMMS_DIR`; dashboard DB via `HIVE_DB`.
- `COMMS_AGENT` must be set (format `name/role`) before any comms operation; agent-runner exits
  without it.
- Cell `msg` shorter than 20 chars is a protocol violation (see never-again rules in `AGENTS.md`).
