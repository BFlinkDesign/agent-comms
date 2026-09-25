<!-- Generated: 2026-03-02 | Updated: 2026-03-02 -->

# agent-comms — HIVE Fleet Protocol

## Purpose

Universal inter-agent communication bus for CNC-1. File-based JSONL. Zero
dependencies for the core bus. Multiple AI terminals (Claude Code, Gemini CLI,
OpenClaw) coordinate through append-only JSONL channels. Any process that can
append to a file can participate.

The Python `hive` package adds structured coordination on top: cell schema
validation, task lifecycle enforcement (A2A-aligned 7 states), lease-based
claim locking, DAG dependency tracking, stall detection, reputation scoring,
and a FastAPI dashboard. The `comms.sh` CLI wraps the raw bus for shell
convenience.

## Key Files

| File | Description |
|------|-------------|
| `CLAUDE.md` | Claude Code guidance: commands, architecture, concurrency invariants |
| `comms.sh` | Primary CLI entry point — source this, do not execute directly |
| `agent-runner.sh` | Persistent dispatch loop for non-interactive agents; validates cells before posting |
| `PROTOCOL.md` | Canonical A2A-aligned task lifecycle schema and cell format specification |
| `FLEET-OPS.md` | Post-mortem from 2026-03-02 first live run — 7 mistakes and their fixes |
| `ROLE-PROMPTS.md` | Role-scoped agent prompts (Claude/architect, Gemini/researcher, Codex/deployer) |
| `standards.md` | Fleet operating standards — clock-in/out protocol, mandatory reads on session start |
| `manifest.json` | Machine-readable protocol definition: identity format, channel registry, agent roster |
| `codex-wrap.py` | Wrapper for codex CLI: strips ANSI/progress noise, extracts test counts |
| `pyproject.toml` | Python package config; pytest timeout 30s; mypy strict + ruff config |
| `org.json` | Organizational config (agent and channel ownership metadata) |
| `README.md` | Quick-start guide: commands, channel list, handoff protocol, raw usage |
| `RESEARCH.md` | Research notes on paradigm-shifting multi-agent tools compiled 2026-03-02 |
| `.mcp.json` | MCP server configuration for this project |

## Subdirectories

| Directory | Purpose |
|-----------|---------|
| `hive/` | HIVE Python protocol library (see `hive/AGENTS.md`) |
| `cmd/fleetd/` | `fleetd` — native host-attributed journal writer (see below) |
| `internal/` | Go packages backing `fleetd`: `cell`, `hostid`, `journal` |
| `tests/` | Full pytest suite (see `tests/AGENTS.md`) |
| `channels/` | JSONL channel files — shared message bus (see `channels/AGENTS.md`) |
| `dashboard/` | Factory floor web UI + FastAPI server (see `dashboard/AGENTS.md`) |
| `docs/` | Design documents and implementation plans (see `docs/AGENTS.md`) |

## For AI Agents

### Working In This Directory

- NEVER hardcode credentials
- CHANNELS_DIR = `C:/Users/Brady.EAGLE/.ai/channels` (canonical — NOT `channels/` in this repo)
- Set `COMMS_AGENT` before any comms command: `export COMMS_AGENT="agent/role"`
- Run tests: `cd C:/tools/agent-comms && python -m pytest tests/ --timeout=30 -q`
- Quality gates (all enforced in CI, all must pass before pushing):
  `python -m pytest tests/ -q` && `mypy` && `ruff check .`
- `comms.sh` is the CLI entry point — source it, don't execute directly

### Task Protocol (A2A-Aligned — 7 States)

```
submitted -> working -> blocked -> complete -> failed -> canceled -> verified
```

See `PROTOCOL.md` for full schema. Never post empty cells (msg < 20 chars = violation).

### Never-Again Rules (from FLEET-OPS.md)

- Never use commands that aren't in `comms.sh` or this document — Gemini hallucinated `comms join`, `comms broadcast`, and `/hive-clock-in`; none exist
- Never give an agent a single-task prompt — task-scoped prompts cause agents to go idle after one task; always use role-scoped prompts with a "never stop" directive
- Never post a cell with msg shorter than 20 characters — empty status cells (e.g., just "TASK-3" or "deployer") are protocol violations; `agent-runner.sh` rejects them
- Never start a task before checking its `depends_on` — Codex claimed TASK-3 before TASK-2 was even posted; always verify dependency state is COMPLETE first
- Never write to any channel path other than `C:/Users/Brady.EAGLE/.ai/channels` — the repo's `channels/` directory is not the live bus; split writes corrupt the fleet's shared state
- Never post a result more than once for the same task_id — Gemini posted TASK-2 results twice with different content; one result per task, tracked in agent-runner state file
- Never run comms commands with `COMMS_AGENT` unset or set to "unknown" — agent identity must be set in format `name/role` before any comms operation; `agent-runner.sh` exits immediately otherwise

## fleetd — the native writer

`fleetd` answers one question the rest of this bus cannot: **which machine did that
happen on.** It is a single static binary with no runtime, built from `cmd/fleetd`
and `internal/`, so it runs on a host that has no Python and participates in the bus
through the documented extension point — any process that can append a file.

```
fleetd host                     what this machine is, and how much that is worth
fleetd record --type T --note N append one host-attributed record
fleetd sync                     publish this machine's records, receive the others'
fleetd where                    per machine, what it was last doing
```

`fleetd sync` is what makes the answer cross-machine. The journal directory is
the root of a clone of one journal repository that fleetd owns. Sync never
rebases, merges or stashes, and never writes this machine's own file, which
`fleetd record` may be appending to at that moment. Instead it:

- snapshots the file up to its last complete record;
- builds a commit on top of the remote tip with git plumbing (`hash-object`,
  `mktree`, `commit-tree`) and pushes exactly that commit, retrying a push that
  lost a race to another machine at most three times;
- then brings in every file the remote changed or deleted. A file with changes
  the remote does not have (another identity's unpublished records on this
  machine, or an edit made by hand) is never overwritten; sync names it.

Two machines can collide only by deriving the same host id. That is detected by
content: the remote copy of this machine's file must be a prefix of the local one,
ignoring the CRLF line endings git for Windows checks files out with; records are
always published with LF. A clone with commits fleetd did not make is refused,
never pushed and never discarded; the error names
`git reset --soft '@{upstream}'` as the way back, which keeps unpublished records.
Every git call is bounded by `--timeout`, and when it runs out git and every
process it started (ssh, a remote helper) are killed: a process group on Unix, a
job object on Windows. On Windows the job also ends anything git leaves running
when each git command returns, not only on timeout, so an ssh ControlPersist
master or a credential-helper daemon does not survive from one git command to
the next, and if fleetd itself dies mid-command git dies with it. The job is
closed only once git's output has been read to the end, so a leftover that still
holds that output open still costs a two-second wait, as on Unix, before it is
ended, and that git command counts as failed. On Windows git cannot move automatic
`gc`/`maintenance` into the background, so when a fetch starts one it runs inside
the job and is ended with it on timeout. That can leave a lock file in the clone
(`packed-refs.lock`, or a `.lock` under `refs/` or `objects/`); later syncs then
fail with `Unable to create '...lock': File exists` until you delete the file the
error names, once no git is running in that clone. If Windows will
not give git a job, or will not let fleetd resume git inside one, git runs outside
any job, git alone is killed on timeout, and the sync still returns within two
seconds of the deadline. Prompts, hooks, commit signing and core.fsmonitor are
all disabled, so a sync never waits for input and starts no daemon. ssh runs in batch mode unless you
chose your own ssh command (`GIT_SSH`, `GIT_SSH_COMMAND` or `core.sshCommand`),
which is used as is.

Every command takes `--json`, so one surface serves a person and a program. The
journal lives at `$COMMS_CHANNELS/journal/<host>.jsonl`, one file per host.
`PROTOCOL.md` carries the cell schema and the full command reference.

Two defaults are deliberate and worth knowing before you use it:

- **The OS account is not recorded unless you pass `--include-user`.** These
  records are meant to be committed, and on a domain-joined Windows host
  `user.Current().Username` is `DOMAIN\account` — publishing it by default would
  put the AD domain and the operator's account name into the repository on every
  record, which would undo the care taken not to publish the machine identifier.
- **`fleetd where` fails on a directory that does not exist** rather than
  answering "no records". An empty answer that is indistinguishable from a
  mistyped path is the worst available output for a tool whose only job is
  saying which machine did something.

Three things about it are load-bearing, and each exists because the naive version
was wrong:

- **Host identity is derived, not assumed.** `internal/hostid` reads
  `/etc/machine-id`, the Windows `MachineGuid`, or the macOS `IOPlatformUUID`, and
  publishes only a salted digest of it — a hardware fingerprint committed to a
  repository has left the machine. Set `FLEET_SALT` to the *same* value on every
  host or one machine will appear as several. When no stable source is readable it
  degrades to the hostname and says so: `stable: false` means the attribution will
  change if the machine is renamed and may collide with another machine of that
  name. A weak identity is labelled weak rather than presented as a strong one.

- **One file per host is the whole concurrency design.** Several machines publish
  into one repository; a shared file would make every push a conflict on the same
  trailing lines. Within a host, concurrent appends from several CLIs are safe
  because each record is a single write to a file opened `O_APPEND` — this is
  verified against six real concurrent processes, not asserted.

- **Order within a host is file order, and there is no order across hosts.**
  `Record.Line` is the position the append actually committed at, which is the same
  reason the board arbitrates races by `rowid` and never by `ts`: writer clocks
  cannot be trusted. Nothing in the journal proves machine A's entry happened before
  machine B's, and `fleetd where` says so in its own output. Where machines *are*
  ranked for display, they are compared as instants rather than as strings —
  lexical comparison of RFC3339 is only correct when every timestamp is UTC `Z`,
  and `hive/cell.py` writes local time with an offset.

- **A line this parser cannot read never hides the records after it.** The bus is
  open to any process that can append, including the raw uuid4 plane whose
  records this parser rejects, so an unreadable line is an expected input rather
  than proof the file is ruined. Every defect is reported, and every intact
  record is still returned.

**Which plane `fleetd` writes to, and why it matters.** This bus has two documented
planes over the same files: the raw plane is plain JSONL appends with uuid4 ids,
and the board plane (`hive/cell.py`) is content-addressed. `fleetd` writes
content-addressed cells, and `internal/cell` proves its ids byte-identical against
the live `hive.cell` module across eight payload shapes.

That choice is forced by the multi-host case rather than being a preference. A
random id makes a duplicate undetectable: if the same observation is published
twice — a retry, a re-run, or two machines seeing the same fact — nothing lets a
reader tell it is the same record. A content-derived id makes them collide, so the
reader can collapse them. Anything appended to a journal through the raw plane's
uuid4 path therefore cannot be deduplicated across hosts; that is a property of
that plane, not a defect in it, and it is the reason `fleetd` does not use it.

## Dependencies

### Internal

- `comms.sh` sources from this directory
- `hive/` Python package installed via `pyproject.toml`

### External

- Python 3.11+ (stdlib only for `hive` core)
- `fastapi` + `uvicorn` (dashboard only)
- `pytest` + `pytest-timeout` (tests)

<!-- MANUAL: -->
