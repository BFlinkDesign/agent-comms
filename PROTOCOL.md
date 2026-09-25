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

### Observation Cell (type="observation" | "handoff" | "note", channel="journal")

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
fleetd host   [--json] [--salt S]
fleetd record [--json] [--dir D] [--salt S] --type T [--note N] [--repo R] [--branch B] [--agent A] [--at RFC3339] [--include-user]
fleetd sync   [--json] [--dir D] [--salt S] [--timeout 60s]
fleetd where  [--json] [--dir D] [--limit N]
```

`fleetd sync` requires the journal directory to be the root of a clone of the
journal repository, with an upstream branch. It publishes only this host's
`<host-id>.jsonl`, up to its last complete line, in a commit built directly on
the remote tip. It never rebases and never writes that file. A push rejected
because another machine pushed first is retried, up to three attempts. It then
brings in every other file the remote changed or deleted, except a file with
local changes the remote does not have, which it leaves alone and reports
(`kept` in `--json`). Anything staged with `git add` and not committed is the
exception: sync resets a staged edit to the remote's version and deletes a
staged new file (`git fsck --lost-found` recovers either).

`fleetd` resolves its journal directory from `--dir`, else
`$COMMS_CHANNELS/journal`, else `./channels/journal`. `fleetd where` reports an
error rather than "no records" when that directory does not exist, so a mistyped
path is distinguishable from a machine that genuinely recorded nothing.

`--salt` (or `$FLEET_SALT`) must be identical on every machine in the fleet, or
one machine will appear as several. It is not a credential.

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
