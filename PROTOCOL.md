# HIVE Fleet Protocol v1.2
# A2A-Aligned Task Lifecycle for File-Based Multi-Agent Systems

Built from real-world mistakes made 2026-03-02. Incorporates Google A2A standards.
Every rule here exists because something broke without it.

---

## The 7-Step Task Lifecycle

Based on Google A2A state machine. Every task MUST pass through these states in order.
No skipping. No free-form status messages without a state field.

```
1. SUBMITTED    → task posted to channel, not yet claimed
2. WORKING      → agent claimed it, actively executing
3. BLOCKED      → agent needs input from another agent (was: input-required in A2A)
4. COMPLETE     → terminal success, artifact posted
5. FAILED       → terminal failure, reason posted
6. CANCELED     → architect killed it
7. VERIFIED     → architect reviewed and approved result (HIVE addition)
```

### State Transition Rules

```
SUBMITTED  → WORKING    (agent claims it)
WORKING    → BLOCKED    (waiting on another task's result)
BLOCKED    → WORKING    (dependency resolved)
WORKING    → COMPLETE   (success)
WORKING    → FAILED     (unrecoverable error)
WORKING    → CANCELED   (architect cancels)
COMPLETE   → VERIFIED   (architect approves)
COMPLETE   → SUBMITTED  (architect rejects — new task, same context)
```

**Terminal states: COMPLETE, FAILED, CANCELED**
Once terminal, never modify. Create a new task instead.

---

## Cell Schema (A2A-Aligned)

Every cell written to a channel MUST include all required fields.
Empty msg field = protocol violation. Agents that post empty cells are broken.

### Task Cell (type="task")
```json
{
  "id": "uuid",
  "from": "claude/architect",
  "ts": "ISO8601",
  "channel": "signx-intel",
  "type": "task",
  "status": "submitted",
  "context_id": "uuid (groups related tasks)",
  "msg": "Human-readable description — MUST be non-empty",
  "data": {
    "for_agent": "gemini/researcher",
    "depends_on": [],
    "parts": [
      { "kind": "text", "text": "Full task description with acceptance criteria" }
    ],
    "skills_required": ["code-analysis", "regression-tracing"]
  }
}
```

### Claim Cell (type="claim")
```json
{
  "type": "claim",
  "status": "working",
  "msg": "Claiming task <id> — starting <brief description of approach>",
  "data": {
    "task_id": "uuid of task being claimed",
    "approach": "1-2 sentence plan before starting"
  }
}
```

### Status Cell (type="status")
```json
{
  "type": "status",
  "msg": "MUST contain actual progress — not just the task name",
  "data": {
    "task_id": "uuid",
    "state": "working",
    "progress": "What I found / what I'm doing / what's blocking me"
  }
}
```

### Result Cell (type="result")
```json
{
  "type": "result",
  "status": "complete",
  "msg": "TASK-N COMPLETE: <summary of what was done and what changed>",
  "data": {
    "task_id": "uuid",
    "artifacts": [
      { "kind": "text", "text": "Full findings / output" }
    ],
    "tests_passed": 248,
    "tests_failed": 0
  }
}
```

### Block Cell (type="blocked")
```json
{
  "type": "blocked",
  "status": "blocked",
  "msg": "Blocked on TASK-2 (gemini/researcher) — cannot run calibrate.py until regression root-cause is known",
  "data": {
    "task_id": "my task uuid",
    "waiting_on": ["task-2-uuid"],
    "will_proceed_when": "gemini/researcher posts result for TASK-2"
  }
}
```

### Observation Cell (type="observation" | "handoff" | "note" | "session" | "turn" | "hook", channel="journal")

Written by `fleetd` (see the Fleet Command Reference below) to record **which
machine** did a piece of work. Git records no such thing: a commit carries an
author, a committer and a UTC offset, none of which identify a host, so this is
recorded at the time or not at all.

These cells use the content-addressed id from `hive/cell.py`, not the raw
plane's uuid4, because two hosts recording the same observation must derive the
same id for a reader to collapse the duplicate.

```json
{
  "id": "hive:3e5ac3f5428c3abf",
  "v": 1,
  "type": "observation",
  "from": "claude/cloud",
  "ts": "2026-09-14T13:55:00.123456789Z",
  "channel": "journal",
  "data": {
    "host.id": "host:21f5d68f266ed466",
    "host.name": "CNC-1",
    "host.os": "windows",
    "host.arch": "amd64",
    "host.source": "windows:MachineGuid",
    "host.stable": true,
    "note": "fleetd packages green, cross-compiled to 5 targets",
    "repo": "agent-comms",
    "branch": "main"
  },
  "refs": [],
  "ttl": 0,
  "tags": []
}
```

| Field | Meaning |
|---|---|
| `host.id` | Salted digest of the platform's stable machine identifier. The raw value is never published. |
| `host.name` | Hostname, in clear — distinguishing CNC-1 from BRADY-HPREMOTE by eye is the point. |
| `host.source` | Where `host.id` came from: `linux:machine-id`, `windows:MachineGuid`, `darwin:IOPlatformUUID`, or `hostname-only`. |
| `host.stable` | `false` means no stable identifier was readable and the id is hostname-derived: it changes if the machine is renamed and may collide with another machine of that name. |
| `note` / `repo` / `branch` | Optional, present only when supplied. |
| `tool` / `event` / `session` | Written by `fleetd hook`: the AI tool, its event name as the tool spells it, and its session id. The record's type is `session` for a session end, `turn` for a turn end, `hook` for any other event. |
| `user` | Optional. Present **only** with `--include-user`: on a domain-joined host the OS account name carries the domain with it, and these records are committed. |

`ts` carries nanosecond precision. At whole-second resolution two distinct
records written in the same second would derive the same id and one would be
collapsed away as a duplicate.

Records live in one append-only file per host at
`$COMMS_CHANNELS/journal/<host-id>.jsonl`. One file per writer is the
concurrency design: several machines publishing into one repository would
otherwise conflict on the same trailing lines on every push.

**Ordering.** Within a host, file order is authoritative — it is the order the
appends committed, and unlike a timestamp it cannot be wrong because a writer's
clock was. Across hosts there is deliberately **no total order**: nothing in the
journal proves machine A's entry happened before machine B's.

---

## Agent Card (A2A Standard)

Every agent MUST publish an agent-card.json in their config directory.
This is what the router reads — not guesses.

Location:
- Claude:  ~/.claude/agent-card.json
- Gemini:  ~/.gemini/agent-card.json
- Codex:   ~/.codex/agent-card.json

```json
{
  "name": "gemini/researcher",
  "description": "Root-cause investigator. Traces regressions, audits data, reads commits. Reports findings before code is touched.",
  "version": "1.0.0",
  "capabilities": {
    "streaming": false,
    "pushNotifications": false,
    "stateTransitionHistory": true
  },
  "skills": [
    {
      "id": "regression-trace",
      "name": "Regression Tracing",
      "description": "Find which commit caused a test delta and why",
      "tags": ["git", "analysis", "regression"]
    },
    {
      "id": "variance-analysis",
      "name": "Variance Analysis",
      "description": "Compare actual vs estimated values, find root causes",
      "tags": ["data", "analysis", "calibration"]
    }
  ],
  "will_not_do": ["modify code", "run deployments", "approve results"]
}
```

---

## Fleet Command Reference (REAL COMMANDS ONLY)

These exist. Use them. Do not invent others.

```bash
# Identity (set before every session)
export COMMS_AGENT="gemini/researcher"
source C:/tools/agent-comms/comms.sh

# Read a channel
comms read <channel>

# Post cells
comms send <channel> "<msg>"         # type=status
comms task <channel> "<msg>"         # type=task
comms result <channel> "<msg>"       # type=result
comms error <channel> "<msg>"        # type=error

# Fleet management
comms clock-in "<role>"              # register to roster
comms clock-out                      # deregister
comms roster                         # show who's online
comms hire <agent/session> <dept> <role> [--task "..."] [--cwd /path]
                                     # hire agent + launch terminal

# Coordination
comms task-ref <channel> <msg> <task_id>    # result that refs a task
comms hive expire                           # remove all TTL-expired cells

# Cognitive layer
comms trace <contract_id> <channel> <outcome> <steps_json>
comms belief <channel> <claim> [confidence]
comms refute <belief_id> <reason> [correction] [channel]

# Host attribution (fleetd — a separate single static binary, no runtime)
fleetd init   [--json] [--dir D] [--salt S] [--reclaim] [--timeout 2m] URL
fleetd host   [--json] [--dir D] [--salt S]
fleetd record [--json] [--dir D] [--salt S] --type T [--note N] [--repo R] [--branch B] [--agent A] [--at RFC3339] [--include-user]
fleetd sync   [--json] [--dir D] [--salt S] [--timeout 60s]
fleetd where  [--json] [--dir D] [--limit N]
fleetd hook   <claude|cursor|codex|grok> [--dir D] [--salt S] [--timeout 40s] [--no-sync] [--json] [event-json]
```

`fleetd hook` is what an AI tool's own hook configuration runs; AGENTS.md has
the configuration for each tool. It reads the tool's event (JSON on stdin, or the
last argument for Codex's `notify`) and records it as above. A session's turns
are recorded at most once every 30 minutes. A session end, and a turn Codex's
`notify` records, then syncs. It prints nothing, always exits 0, and logs any
problem to `fleetd-hook.log` beside the journal directory, which `fleetd where`
mentions for a week.

`fleetd init URL` makes the journal directory a clone of the journal repository,
once per machine, without moving or deleting anything in it, and then syncs. An
empty repository gets a first commit holding `fleetd.json` (`{"salt": ...}`, plus
an `about` line), on the branch it names as its default, else `main`; a repository
that already holds records without one needs the salt its machines use (`--salt` or
`$FLEET_SALT`), or a new one if they ran without a salt (their earlier records keep
the ids they had). When the repository's default branch does not exist, the journal is
on its only branch; with several, init is refused. A plain clone of the repository
made while it was empty, which has no commit, is refused with the advice to delete
its `.git` directory and run init again. A `fleetd.json`
in the journal directory with another salt is replaced by the journal's. A repository whose top level
holds anything but `host-*.jsonl`, `fleetd.json`, README, LICENSE, `.gitignore` or
`.gitattributes`, or a directory, is refused; so is a journal directory holding
anything but journal files and `fleetd.json`, or a clone of another repository.
When this machine's file no longer starts with the records the journal holds for
it, nothing is published and init fails: its journal directory was set up again or
restored from an older copy, or another machine has its id, and only a person can
tell which. `--reclaim` says it is the first: under the sync lock it moves the file
into `.git/fleetd-pre-init/`, puts the journal's copy back, and files the moved
records the journal lacks after it. init exits non-zero when its sync failed with
`--reclaim`, or for a reason no later sync gets past: this machine's file not
starting with the journal's copy, commits fleetd did not make, or a push the
remote declines (a hook, branch protection, a ruleset). Any other failure, a
credential that cannot push included, is reported and retried by the next sync.

`fleetd sync` requires the journal directory to be the root of a clone of the
journal repository, with an upstream branch. It notes each outcome in the clone's
`.git/fleetd-sync.json`, which `fleetd where` reports. It publishes only this host's
`<host-id>.jsonl`, up to its last complete line, in a commit built directly on
the remote tip. It never rebases and never rewrites that file. A push rejected
because another machine pushed first is retried, up to three attempts; a push
the remote declines (a hook, branch protection, a ruleset) fails at once,
saying it was refused. It then
brings in every other file the remote changed or deleted, except a file with
local changes the remote does not have, which it leaves alone and reports
(`kept` in `--json`). Anything staged with `git add` and not committed is the
exception: sync resets a staged edit to the remote's version and deletes a
staged new file (`git fsck --lost-found` recovers either until the gc a sync runs
prunes it, which it may as soon as the content is two weeks old). A git lock file older
than ten minutes is removed, as left by a git command a timeout killed
(`cleared` in `--json`, and on stderr when the sync then fails). Once the clone
holds a thousand loose objects, a sync that did its work runs `git gc` (`packed`
in `--json`). Journal commits are by `fleetd <fleetd@fleetd.invalid>`, dated when
the sync runs. git's repository variables (`GIT_DIR` and the rest of
`git rev-parse --local-env-vars`, except `GIT_CONFIG_COUNT`) and the commit
dates a git hook exports are ignored, so a sync run from a git hook still syncs
the journal clone.

`fleetd` resolves its journal directory from `--dir`, else
`$COMMS_CHANNELS/journal`, else `~/.ai/channels/journal` (never the current
directory). `fleetd where` reports an
error rather than "no records" when that directory does not exist, so a mistyped
path is distinguishable from a machine that genuinely recorded nothing. For each
host, `where --json` adds `last_published` (the ts of the newest record the
remote has from that host, leaving out records re-filed after it, as of this
clone's last sync) and `unpublished` (the
host's complete records here that the remote lacks), when the journal directory is
the top of a clone and git could say.

The salt is the one in the journal's `fleetd.json`. `--salt` may repeat it but not
contradict it; a `$FLEET_SALT` that contradicts it is overridden with a warning.
Without `fleetd.json` they supply it. It must be identical on every machine in the
fleet, or one machine will appear as several. It is not a credential. A sync files
under this machine's fleet id the records it wrote under another salt (none or
`$FLEET_SALT` before it knew the fleet's, a replaced `fleetd.json`'s, or the
journal's previous one) that the old id never published, appending them to this
machine's file with `refiled.from` set to the old id (left out when it would take
the record past 4096 bytes; `where` and `last_published` may then take that record
for the latest). It moves their file into
`.git/fleetd-pre-init/`, putting back a file git tracks as published, and files a
record appended to the moved copy later at the next sync. A `--salt` or
`$FLEET_SALT` a record was written under for want of `fleetd.json` is noted in
`<journal directory>.salts`, beside it, so that sync need not have it set; so is a
salt the journal's `fleetd.json` held before, when the clone's git directory cannot
take its note.

DOES NOT EXIST (never use):
- comms join
- comms broadcast
- comms subscribe
- /hive-clock-in
- gemini clock-in
- codex clock-in

---

## Dependency Enforcement

If your task has a `depends_on` list, you MUST:
1. Check that all dependency tasks have `status=complete` before starting
2. If not complete: post a `type=blocked` cell and STOP
3. Check again every 30 seconds (or when agent-runner notifies you)

Do NOT start work on a blocked task. Post empty status cells while waiting = protocol violation.

---

## Agent Dispatch Loop (agent-runner.sh)

Replaces manual prompting. Run once per agent terminal.

```bash
COMMS_AGENT="gemini/researcher" AGENT_CMD="gemini" \
  bash /c/tools/agent-comms/agent-runner.sh signx-intel
```

What it does:
1. Polls channel every 5 seconds
2. Finds SUBMITTED tasks tagged for this agent (or untagged)
3. Checks dependencies — skips BLOCKED tasks
4. Claims via race-safe lease
5. Invokes CLI, captures output
6. Posts COMPLETE result cell
7. Loops forever

POLL_SECS env var overrides the 5-second default.

---

## Context ID Usage

Group related tasks with a shared context_id.
This is how the architect links TASK-1 → TASK-2 → TASK-3 as one sprint.

```bash
CTX=$(python -c "import uuid; print(uuid.uuid4())")
comms task signx-intel "TASK-1: Fix bug" --context $CTX
comms task signx-intel "TASK-2: Root-cause regression" --context $CTX
comms task signx-intel "TASK-3: Run calibration" --context $CTX --depends TASK-1-ID TASK-2-ID
```

The dashboard groups and visualizes tasks by context_id as a sprint/batch.
