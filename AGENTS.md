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
| `internal/` | Go packages backing `fleetd`: `cell`, `gitsync`, `hostid`, `journal` |
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
  `python -m pytest tests/ -q` && `mypy` && `ruff check .`, and for fleetd
  `gofmt -l ./cmd ./internal` (must print nothing) && `go vet ./...` && `go test ./... -count=1`
  (CI adds `-race` on Linux, which needs cgo; a Windows runner; five cross-compile targets; and the
  Windows-only `scripts/install-fleetd.test.ps1`). `go` downloads and runs at least the Go
  named by go.mod's `toolchain` line; where that download is blocked, `GOTOOLCHAIN=local` uses
  the installed Go, and only CI then shows the pinned toolchain passing.
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
fleetd init URL                 set up this machine's journal, once
fleetd host                     what this machine is, and how much that is worth
fleetd record --type T --note N append one host-attributed record
fleetd sync                     publish this machine's records, receive the others'
fleetd where                    per machine, what it was last doing, and how fresh that is
fleetd hook <tool>              record a Claude Code, Cursor, Codex or Grok event
```

`fleetd init URL` sets a machine up, once, and then syncs. It makes the journal
directory a clone of the journal repository without moving, rewriting or deleting
anything in it, because a hook may be appending a record at any moment: it clones
without a work tree beside the directory, makes sure the repository has
`fleetd.json`, writes that file into the directory so every record from then on
uses its salt, and only then moves the clone's git directory in and checks out the
files the directory lacks. When the repository is empty, its first commit holds
`fleetd.json` with a new salt for the fleet, on the branch the repository names as
its default, else `main`. One that already holds records needs the salt its
machines use (`--salt` or `FLEET_SALT`); init never invents one for it. When the
repository's default branch does not exist but it has exactly one branch, the
journal is on that branch; a server that does not advertise an empty repository's
default leaves the first machine starting `main` whatever the repository's HEAD
names. With several branches and no default among them, init is refused before
anything is pushed. A repository
with anything but host journal files, `fleetd.json`, a README, a LICENSE,
`.gitignore` or `.gitattributes`, or with a directory, is refused, and so is a
journal directory holding anything but journal files and `fleetd.json`. A
directory that is already a clone of another repository is refused too. A
`fleetd.json` already in the journal directory with another salt is replaced by the
journal's, and the next sync files the records written under its salt under the
journal's. When two machines start the same empty repository at once, the one
whose push loses takes the other's commit and salt. Running init again finishes
what an interrupted run began; one killed outright leaves the sync lock behind,
which init and every sync wait out for ten minutes. When this machine's journal
directory was set up again, or restored from an older copy, its file no longer
starts with the records the journal holds for it, and neither init nor any sync
publishes anything: a second machine with this machine's id produces the same
mismatch, and only a person can tell the two apart. `fleetd init --reclaim URL`
says it is this machine: holding the sync lock, it moves the file into the clone's
git directory, `fleetd-pre-init/`, puts the journal's copy back, and files the
moved records the journal lacks after it.

`fleetd sync` is what makes the answer cross-machine. The journal directory is
the root of a clone of one journal repository that fleetd owns. Sync never
rebases, merges or stashes, and never rewrites this machine's own file, which
`fleetd record` may be appending to at that moment: it only appends the records
it re-files (see host identity below), as `fleetd record` appends, and puts the
file back as the remote has it when it is missing. Only `fleetd init --reclaim`
moves it aside. Instead it:

- snapshots the file up to its last complete record;
- builds a commit on top of the remote tip with git plumbing (`hash-object`,
  `mktree`, `commit-tree`) and pushes exactly that commit, retrying a push that
  lost a race to another machine, for at most three attempts in all;
- then brings in every file the remote changed or deleted. A file with changes
  the remote does not have (an edit made by hand, say) is never overwritten;
  sync names it. A file that holds only the start of git's copy, such as one
  restored from an older backup, has nothing of its own and is brought up to
  date, whether or not the remote changed it. The
  exception is anything staged with `git add` and not committed: the clone is
  fleetd's, so sync resets a staged edit to the remote's version and deletes a
  staged new file, even one the remote never had. `git fsck --lost-found`
  recovers such content until git's garbage collection prunes it.

Two machines can collide only by deriving the same host id. That is detected by
content: the remote copy of this machine's file must be a prefix of the local one,
ignoring the CRLF line endings git for Windows checks files out with; records are
always published with LF. The error also names the other cause: this machine's
journal directory set up again over newer records, which `fleetd init --reclaim` repairs. A clone with commits fleetd did not make is refused,
never pushed and never discarded; the error names
`git reset --soft '@{upstream}'` as the way back, which keeps unpublished records.
Every git call is bounded by `--timeout`, and when it runs out git and the
processes it started (ssh, a remote helper) are killed: on Unix every process
still in git's process group (one that starts a session of its own, as `setsid`
does, leaves it), on Windows every process in git's job object. On Windows the job also ends anything git leaves running
when each git command returns, not only on timeout, so an ssh ControlPersist
master or a credential-helper daemon does not survive from one git command to
the next, and if fleetd itself dies mid-command git dies with it. The job is
closed only once git's output has been read to the end, so a leftover that still
holds that output open still costs a two-second wait, as on Unix, before it is
ended, and that git command counts as failed. Automatic `gc` and `maintenance`
are off for every command a sync runs, so a sync never starts them inside its
deadline. Instead, once a sync has done its work and the clone holds a thousand
loose objects, it runs `git gc` with what is left of its deadline (`packed` in
`--json`), so the clone does not grow without end. That gc packs objects and does
nothing else, so one the deadline kills leaves no lock behind. A git command killed
on timeout can still leave a lock file (an `index.lock`, or a `.lock` under
`refs/`); once fleetd holds its own sync lock, it removes any git lock file older
than ten minutes outside the object store, where fleetd takes none, and says so (`cleared` in `--json`, and on stderr when the sync then
fails), and leaves a newer one alone. A git command a person leaves running in the
clone for over ten minutes, such as a commit with its editor open, loses its lock
the same way: the clone is fleetd's. If Windows will
not give git a job, or will not let fleetd resume git inside one, git runs outside
any job, git alone is killed on timeout, and the sync still returns within two
seconds of the deadline. Prompts, hooks, commit signing and core.fsmonitor are
all disabled, so a sync never waits for input and starts no daemon. ssh runs in batch mode unless you
chose your own ssh command (`GIT_SSH`, `GIT_SSH_COMMAND` or `core.sshCommand`),
which is used as is. Variables that point git at a repository or carry a git
command's own `-c` settings (`GIT_DIR`, `GIT_WORK_TREE`, `GIT_INDEX_FILE`,
`GIT_CONFIG_PARAMETERS` and the rest of `git rev-parse --local-env-vars`) are
dropped, and so are the commit dates a git hook exports, so fleetd run from a git
hook still syncs its own clone and dates its commits when it runs.
`GIT_CONFIG_COUNT` is kept, as git itself keeps it: configuration the environment
sets on purpose that way (a URL rewrite, a trusted directory, an ssh command) still
applies. Journal commits are by `fleetd <fleetd@fleetd.invalid>`, never the PC's
git identity, which may be missing or a private address GitHub refuses to publish.

Every command takes `--json`, so one surface serves a person and a program. The
journal lives at `$COMMS_CHANNELS/journal/<host>.jsonl`, one file per host; with no
`COMMS_CHANNELS` and no `--dir`, at `~/.ai/channels/journal`, never in the current
directory. On Windows `~` is `%USERPROFILE%`, which is not always Git Bash's `~`:
Git Bash sets `HOME` to `%HOMEDRIVE%%HOMEPATH%` when that exists, as it can on a
domain PC with a network home folder, so clone the journal into
`%USERPROFILE%\.ai\channels\journal`, not `$HOME/.ai/channels/journal`. A record
appended after a torn one (a crash partway through a write) starts on a line of its
own, so the fragment stays one malformed line. `PROTOCOL.md` carries the cell schema
and the full command reference.

**Upgrading from fleetd-v0.1.0: run `fleetd sync` with the old version first.**
Two changes in how fleetd finds things could otherwise leave records behind that
the old version wrote and never synced:
- The default journal directory is now the home directory, where v0.1.0 used the
  current directory.
- On a Windows PC where `reg.exe` could not be read, the host id is now derived
  from MachineGuid, where v0.1.0 fell back to the hostname.

That sync publishes what the old version finds on that run: its journal directory,
under the id it derives then. It cannot publish records v0.1.0 wrote into a
project's `./channels/journal`, which lies inside that project's repository, or
records filed under the hostname id on an earlier run when `reg.exe` failed and this
run's does not. Those stay where they are.

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
- **`fleetd where` says how fresh its answer is.** Every sync notes its outcome in
  the clone's git directory, and `where` reports it: when this machine last
  synced, and whether its last sync failed and why, even when there are no records
  to show. For each machine it gives the time of the newest record the remote has
  from it (`last_published` in `--json`) and how many of its records this machine
  holds that the remote lacks (`unpublished`), read from the remote's copy of each
  file rather than its history. A machine whose syncs keep failing shows up there
  rather than looking idle. A journal directory that is not the top of a clone
  gets no such lines, and a machine git cannot answer for is left out of them, not
  reported as unpublished.

Three things about it are load-bearing, and each exists because the naive version
was wrong:

- **Host identity is derived, not assumed.** `internal/hostid` reads
  `/etc/machine-id`, the Windows `MachineGuid`, or the macOS `IOPlatformUUID`, and
  publishes only a salted digest of it — a hardware fingerprint committed to a
  repository has left the machine. The salt is the one in the journal's
  `fleetd.json`, so every machine that clones the journal uses the same one, with
  nothing to set in its environment. `--salt` may repeat it but not contradict it.
  A `FLEET_SALT` that contradicts it is overridden with a warning, since refusing
  would drop every record of a hook started with a stale one. An invalid
  `fleetd.json` is not trusted: the salt it last held, kept in the clone's git
  directory, is used until a sync brings in a fixed one. Records this machine
  wrote under another salt (none or `FLEET_SALT` before it knew the fleet's, the
  salt of a `fleetd.json` init replaced, or the journal's salt before it changed)
  are filed under the fleet's id by the next sync, while it holds its lock. It
  moves their file into the clone's git directory, `fleetd-pre-init/`, and files
  the records the old id never published; a file git tracks is put back as
  published, and one a killed sync left missing is put back too. A file git
  cannot answer for (it timed out, say) waits for the next sync. A process that
  still had the file open appends to the moved copy, and the next sync files that
  record too. A re-filed record carries
  `refiled.from`, the id it was written under, and `where` does not take it for
  the machine's latest activity. When no stable source is readable it
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

### `fleetd hook`: recording AI CLI sessions without anyone remembering to

`fleetd hook <tool>` is what a tool's own hook configuration runs. The tool can be
`claude`, `cursor`, `codex` or `grok`. Each of their hook systems, payloads and
exit-code contracts was checked against the tool's own documentation on
2026-09-25. The hook reads the event the way the tool documents it: JSON on stdin,
or for Codex's `notify` program, JSON as the last argument. It then appends one
record through the same code path as `fleetd record`, with:

- `from` set to the tool;
- type `session` when a session ended, `turn` when a turn did, and `hook` for any
  other event;
- note `<tool> <event>`, and `tool`, `event` and `session` (the tool's session id)
  in the record's data;
- `repo` and `branch` taken from the git repository at the event's working
  directory: the basename of `git -C <cwd> rev-parse --show-toplevel`, and
  `git branch --show-current`. The working directory is `cwd`, or
  `workspace_roots[0]` for Cursor. In a linked worktree the repo is the
  worktree's directory name.

An event outside a repository, or with no working directory, is recorded without
repo and branch. The working directory itself is never recorded, because it
usually contains the account name. Neither are transcripts, prompts or replies.
Unknown payload fields are ignored.

**A session's turns are recorded at most once every 30 minutes.** A turn ends
after every reply. One record per half hour says where the work happened as well
as one per reply would, and keeps each machine's file, which every machine
reads, small. The check reads only the end of this machine's file and runs
before git is asked anything, so a skipped turn costs almost nothing. A session
end is always recorded.

**A session end syncs; a turn from a tool's own hooks does not.** A tool runs its
per-turn hook (Stop, `stop`) while the person waits, so a sync there would add a
fetch and a push to an answer. A sync publishes this machine's whole file, so a
turn's record goes out with the next sync. That sync is bounded by `--timeout`
(default 40s). `--no-sync` records only.

- **Codex's hooks never sync.** Its SessionEnd hook may run for at most 3
  seconds, which is too short for a fetch and a push, and a sync killed partway
  leaves its lock behind for ten minutes.
- **Codex's `notify` program does sync every turn it records.** Codex starts it
  and neither waits for it nor limits it, so a machine that runs only Codex
  still publishes.
- **Another sync already running is not a problem.** If one is running when a
  hook syncs, the next sync publishes the record.

**A hook never disturbs the session it runs in.** Every one of these tools gives
a hook's exit code and output a meaning:

- exit 2 from a Stop hook keeps Claude Code or Grok working;
- Codex fails a Stop hook that prints plain text;
- Cursor submits a `followup_message` that a stop hook prints as the next user
  message;
- a non-zero exit shows an error in the session.

So `fleetd hook` prints nothing and always exits 0, even for a bad flag, `-h` or a
panic. It appends any problem to **`fleetd-hook.log` beside the journal
directory**: with the default journal, `~/.ai/channels/fleetd-hook.log`. Problems
include a payload that isn't JSON, a `FLEET_SALT` the journal's salt overrides,
or a sync that failed or timed out. `fleetd where` names the log and quotes its last
line while a problem is less than a week old. Once the log reaches 256 KiB it is
moved to `fleetd-hook.log.1`, replacing the previous copy, so the log never
grows past two files. `--json` prints what the hook did, for trying it by hand.
Never put `--json` in a tool's configuration.

**The journal directory must be absolute.** It is `--dir`, else
`$COMMS_CHANNELS/journal`, else `~/.ai/channels/journal`, and a relative `--dir`
or `COMMS_CHANNELS` is refused. A hook runs in whatever directory the tool
chose, usually the project, so a relative journal would scatter records across
every project worked in.

**One tool's hook fires inside the others' sessions.** By default, Cursor and
Grok both run the hooks in `~/.claude/settings.json`, and Grok also runs
`~/.cursor/hooks.json`. The hook tells the sender from its payload and
environment:

- Grok's events carry `hookEventName`, and Grok sets `GROK_HOOK_EVENT`;
- Cursor's events carry `cursor_version`, and Cursor sets `CURSOR_VERSION`.

It records nothing when the sender isn't the tool it was configured for, since
recording the event would either misattribute it or duplicate what that tool's
own hook records. So configure `fleetd hook <tool>` in every tool you use. With
only Claude Code's hook configured, Cursor and Grok sessions are not recorded.

#### Setting it up on a Windows PC

1. `install-fleetd.ps1` puts `fleetd.exe` in `%USERPROFILE%\bin` and on the
   user PATH. That is how the configurations below find it.
2. Run `fleetd init <journal repository URL>` once, from any directory. It
   sets the journal up at `%USERPROFILE%\.ai\channels\journal`, with the fleet's
   salt in it, so no environment variable needs setting. The first PC to run it
   on an empty repository starts the journal. On a journal its machines already
   record into without `fleetd.json`, give the salt they use: `--salt`. If this
   PC's journal directory was set up again or restored from a backup, init says
   that nothing can be published; run `fleetd init --reclaim <URL>` to put the
   journal's records for this PC back in front of its newer ones.
3. Add the configurations below, then restart each tool so it reads them.

Each tool's session-end entry allows 60 seconds, so that fleetd's own 40-second
limit on the sync is always the one that stops it.

**Claude Code**: `%USERPROFILE%\.claude\settings.json`. Exec form (`args` set)
resolves `fleetd.exe` on PATH with no shell in between.

```json
{
  "hooks": {
    "Stop": [
      { "hooks": [ { "type": "command", "command": "fleetd.exe", "args": ["hook", "claude"], "timeout": 30 } ] }
    ],
    "StopFailure": [
      { "hooks": [ { "type": "command", "command": "fleetd.exe", "args": ["hook", "claude"], "timeout": 30 } ] }
    ],
    "SessionEnd": [
      { "hooks": [ { "type": "command", "command": "fleetd.exe", "args": ["hook", "claude"], "timeout": 60 } ] }
    ]
  }
}
```

SessionEnd hooks share a 1.5-second budget unless a hook sets a longer
`timeout`, which raises it to at most 60 seconds, so keep the 60.

**Cursor**: `%USERPROFILE%\.cursor\hooks.json`. Cursor documents
`~/.cursor/hooks.json`; that this is the Windows location is inferred.

```json
{
  "version": 1,
  "hooks": {
    "stop": [ { "command": "fleetd.exe hook cursor", "timeout": 30 } ],
    "sessionEnd": [ { "command": "fleetd.exe hook cursor", "timeout": 60 } ]
  }
}
```

**Codex**: its `notify` program, at the top level of
`%USERPROFILE%\.codex\config.toml`, before any table. It needs no trust. Codex
adds the event as the last argument, and a turn it records is synced at once:

```toml
notify = ["fleetd.exe", "hook", "codex"]
```

Codex's hooks work too, alone or as well as `notify`. A turn its Stop hook
records is not recorded again by `notify`, which still publishes it, since Codex's
hooks never sync. Codex skips a hook until you trust it, and trust is tied to the
hook's exact definition, so run `/hooks` in Codex after adding or editing these.
Codex gives a SessionEnd hook 1 second unless configured otherwise, and at most 3,
so its hooks ask git for the repository and branch for at most half a second.

```toml
[[hooks.Stop]]
[[hooks.Stop.hooks]]
type = "command"
command = "fleetd hook codex"
command_windows = "fleetd.exe hook codex"
timeout = 30

[[hooks.SessionEnd]]
[[hooks.SessionEnd.hooks]]
type = "command"
command = "fleetd hook codex"
command_windows = "fleetd.exe hook codex"
timeout = 3
```

**Grok Build**: `%USERPROFILE%\.grok\hooks\fleetd.json`. Grok documents
`~/.grok/hooks/*.json`; the Windows location is inferred from its
documented `%USERPROFILE%\.grok\config.toml`. Grok also loads Claude Code's
hooks, but an open Grok bug (xai-org/plugin-marketplace#236) reports that those
never run, so give Grok its own:

```json
{
  "hooks": {
    "Stop": [ { "hooks": [ { "type": "command", "command": "fleetd.exe hook grok", "timeout": 30 } ] } ],
    "StopFailure": [ { "hooks": [ { "type": "command", "command": "fleetd.exe hook grok", "timeout": 30 } ] } ],
    "SessionEnd": [ { "hooks": [ { "type": "command", "command": "fleetd.exe hook grok", "timeout": 60 } ] } ]
  }
}
```

Grok fires one more Stop when a session ends (reason `channel_closed` or
`shutdown`), and fleetd leaves that to SessionEnd. It also records nothing for a
subagent's events (`subagentType`).

None of these configurations has yet been run by the tool on a Windows PC. The
tests feed each tool's own documented example event through `fleetd hook`
against real repositories and a real remote. They cannot show a tool starting
the hook.

## Dependencies

### Internal

- `comms.sh` sources from this directory
- `hive/` Python package installed via `pyproject.toml`

### External

- Python 3.11+ (stdlib only for `hive` core)
- `fastapi` + `uvicorn` (dashboard only)
- `pytest` + `pytest-timeout` (tests)

<!-- MANUAL: -->
