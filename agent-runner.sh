#!/usr/bin/env bash
# =============================================================================
# agent-runner.sh -- Persistent dispatch loop for autonomous agent operation
#
# Polls a HIVE channel JSONL file for unclaimed tasks, claims them atomically,
# invokes the configured agent CLI, and writes result cells back to the channel.
# Runs forever ("always be working" behavior).
#
# Usage:
#   COMMS_AGENT="gemini/researcher" AGENT_CMD="gemini" ./agent-runner.sh signx-intel
#   COMMS_AGENT="codex/deployer"    AGENT_CMD="codex"  ./agent-runner.sh signx-intel
#
# ENV:
#   COMMS_AGENT  -- agent identity string, e.g. "gemini/researcher"
#   AGENT_CMD    -- CLI binary to invoke: "gemini" | "codex" | "claude"
#   POLL_SECS    -- poll interval in seconds (default: 5)
#   HIVE_CHANNELS_DIR / COMMS_CHANNELS -- override channel directory (default: canonical fleet path)
# =============================================================================

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------

CHANNELS_DIR="${HIVE_CHANNELS_DIR:-${COMMS_CHANNELS:-C:/Users/Brady.EAGLE/.ai/channels}}"
COMMS_AGENT="${COMMS_AGENT:-unknown/runner}"
AGENT_CMD="${AGENT_CMD:-gemini}"
POLL_SECS="${POLL_SECS:-5}"
COMMS_DIR="${HIVE_COMMS_DIR:-C:/tools/agent-comms}"

CHANNEL="${1:-}"
if [[ -z "$CHANNEL" ]]; then
  echo "[RUNNER] ERROR: channel argument required"
  echo "[RUNNER] Usage: COMMS_AGENT=gemini/researcher AGENT_CMD=gemini ./agent-runner.sh <channel>"
  exit 1
fi

validate_channel() {
  local ch="$1"
  if [[ ! "$ch" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]]; then
    echo "[RUNNER] ERROR: invalid channel '${ch}' — use lowercase letters, numbers, underscores, or hyphens" >&2
    exit 1
  fi
}

validate_channel "$CHANNEL"

CHANNEL_FILE="${CHANNELS_DIR}/${CHANNEL}.jsonl"

# State file tracks which task IDs this runner has already processed
# (prevents reprocessing after restart if tasks are already claimed/completed)
AGENT_SLUG="${COMMS_AGENT//\//-}"
STATE_FILE="${HOME}/.ai/runner-state-${AGENT_SLUG}.txt"

# ---------------------------------------------------------------------------
# Safety guard: refuse to actually invoke claude CLI in runner context.
# Claude sessions are expensive and interactive — log the task instead.
# To use claude in a runner, set AGENT_CMD="claude" and the task will be
# logged to the channel as a "needs-human" cell for Brady to review.
# ---------------------------------------------------------------------------
CLAUDE_RUNNER_MODE=false
if [[ "$AGENT_CMD" == "claude" ]]; then
  CLAUDE_RUNNER_MODE=true
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

log() {
  # All runner log lines get [RUNNER] prefix with timestamp
  local ts
  ts=$(date '+%Y-%m-%dT%H:%M:%S')
  echo "[RUNNER] [${ts}] $*"
}

ensure_dirs() {
  mkdir -p "${CHANNELS_DIR}"
  mkdir -p "${HOME}/.ai"
  if [[ ! -f "$CHANNEL_FILE" ]]; then
    safe_ensure_file "$CHANNEL_FILE"
  elif [[ -L "$CHANNEL_FILE" ]]; then
    echo "[RUNNER] ERROR: refusing symlinked channel file ${CHANNEL_FILE}" >&2
    exit 1
  fi
  if [[ ! -f "$STATE_FILE" ]]; then
    safe_ensure_file "$STATE_FILE"
  elif [[ -L "$STATE_FILE" ]]; then
    echo "[RUNNER] ERROR: refusing symlinked state file ${STATE_FILE}" >&2
    exit 1
  fi
}

safe_ensure_file() {
  local path="$1"
  python -c "
import os, sys
path = sys.argv[1]
if os.path.islink(path):
    print(f'[RUNNER] ERROR: refusing symlinked file {path}', file=sys.stderr)
    sys.exit(1)
if os.path.exists(path):
    if not os.path.isfile(path):
        print(f'[RUNNER] ERROR: refusing non-file path {path}', file=sys.stderr)
        sys.exit(1)
    sys.exit(0)
flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL
if hasattr(os, 'O_NOFOLLOW'):
    flags |= os.O_NOFOLLOW
fd = os.open(path, flags, 0o666)
os.close(fd)
" "$path"
}

# Write a JSONL cell to the channel using Python (mirrors comms.sh _comms_write).
# Args: channel type msg data_json
write_cell() {
  local channel="$1" type_="$2" msg="$3" data="${4:-"{}"}"
  validate_channel "$channel"
  local out_file="${CHANNELS_DIR}/${channel}.jsonl"
  python -c "
import json, os, uuid, datetime, sys

agent  = sys.argv[1]
ch     = sys.argv[2]
type_  = sys.argv[3]
msg    = sys.argv[4]
data_s = sys.argv[5]
path   = sys.argv[6]

try:
    data = json.loads(data_s)
except json.JSONDecodeError as e:
    print(f'[RUNNER] ERROR: invalid JSON payload: {e}', file=sys.stderr)
    sys.exit(1)

base = os.path.realpath(os.path.dirname(path))
real_path = os.path.realpath(path)
if os.path.commonpath([base, real_path]) != base or os.path.islink(path):
    print('[RUNNER] ERROR: channel file must stay under channels directory and must not be a symlink', file=sys.stderr)
    sys.exit(1)

obj = {
    'id':      str(uuid.uuid4()),
    'from':    agent,
    'ts':      datetime.datetime.now().astimezone().isoformat(),
    'channel': ch,
    'type':    type_,
    'msg':     msg,
    'data':    data,
}
flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND
if hasattr(os, 'O_NOFOLLOW'):
    flags |= os.O_NOFOLLOW
fd = os.open(path, flags, 0o666)
with os.fdopen(fd, 'a', encoding='utf-8') as f:
    f.write(json.dumps(obj, ensure_ascii=False) + '\n')

# Echo the generated cell ID to stdout so bash can capture it
print(obj['id'])
" "$COMMS_AGENT" "$channel" "$type_" "$msg" "$data" "$out_file"
}

# Mark a task ID as processed in the local state file
mark_processed() {
  local task_id="$1"
  echo "$task_id" >> "$STATE_FILE"
}

# Check if a task ID is already in the local state file
already_processed() {
  local task_id="$1"
  grep -qxF "$task_id" "$STATE_FILE" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Role matching: only claim tasks tagged for this agent or untagged
# Tags look like "[gemini/researcher]" anywhere in the msg field.
# ---------------------------------------------------------------------------
task_is_for_me() {
  local msg="$1"
  # If no bracket tag present -> untagged, accept it
  if ! echo "$msg" | grep -qE '\[[a-z]+/[a-z]+\]'; then
    return 0
  fi
  # If our agent tag appears, accept it
  if echo "$msg" | grep -qF "[${COMMS_AGENT}]"; then
    return 0
  fi
  return 1
}

# ---------------------------------------------------------------------------
# Scan the channel file for unclaimed task cells.
# Returns lines of: <task_id>|<task_msg>
# Uses Python to do the full scan atomically.
# ---------------------------------------------------------------------------
find_open_tasks() {
  python -c "
import json, sys

channel_file = sys.argv[1]

tasks   = {}   # task_id -> {msg, depends_on}
claimed = set()
done    = set()

with open(channel_file, encoding='utf-8') as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            cell = json.loads(line)
        except json.JSONDecodeError:
            continue

        ctype = cell.get('type', '')
        cid   = cell.get('id', '')
        data  = cell.get('data', {})

        if ctype == 'task':
            depends_on = data.get('depends_on', [])
            if not isinstance(depends_on, list):
                depends_on = []
            tasks[cid] = {
                'msg': cell.get('msg', ''),
                'depends_on': [d for d in depends_on if isinstance(d, str)],
            }

        elif ctype == 'claim':
            tid = data.get('task_id', '')
            if tid:
                claimed.add(tid)

        elif ctype == 'result':
            tid = data.get('task_id', '')
            if tid:
                done.add(tid)

        elif ctype == 'error':
            tid = data.get('task_id', '')
            if tid:
                done.add(tid)

def deps_satisfied(depends_on):
    if not depends_on:
        return True
    return all(dep in done for dep in depends_on)

# Emit open tasks: not yet claimed, not done, dependencies satisfied
for tid, info in tasks.items():
    if tid in claimed or tid in done:
        continue
    if not deps_satisfied(info.get('depends_on', [])):
        continue
    safe_msg = info['msg'].replace('|', '[pipe]')
    print(f'{tid}|{safe_msg}')
" "$CHANNEL_FILE"
}

# ---------------------------------------------------------------------------
# Claim race prevention.
# After we write our claim cell, re-read the channel and check whether another
# agent's claim cell for the same task_id appears BEFORE ours.
# ---------------------------------------------------------------------------
claim_task() {
  local task_id="$1"
  local data
  data=$(python -c "import json,sys;print(json.dumps({'task_id':sys.argv[1]}))" "$task_id")

  # Write our claim cell; capture the new cell ID
  local our_claim_id
  our_claim_id=$(write_cell "$CHANNEL" "claim" "claiming ${task_id}" "$data")

  # Small sleep to let any near-simultaneous writers flush
  sleep 0.3

  # Now verify: are we the FIRST claimer for this task?
  local winner
  winner=$(python -c "
import json, sys

channel_file = sys.argv[1]
task_id      = sys.argv[2]
our_id       = sys.argv[3]

with open(channel_file, encoding='utf-8') as f:
    for line in f:
        line = line.strip()
        if not line:
            continue
        try:
            cell = json.loads(line)
        except json.JSONDecodeError:
            continue
        if cell.get('type') == 'claim':
            tid = cell.get('data', {}).get('task_id', '')
            if tid == task_id:
                # First claim cell for this task wins
                print(cell.get('id', ''))
                break
" "$CHANNEL_FILE" "$task_id" "$our_claim_id")

  if [[ "$winner" == "$our_claim_id" ]]; then
    return 0   # We won the race
  else
    return 1   # Someone else claimed it first
  fi
}

# ---------------------------------------------------------------------------
# Invoke the agent CLI with the task prompt.
# Returns the agent output via stdout.
# ---------------------------------------------------------------------------
invoke_agent() {
  local prompt="$1"
  local output=""

  case "$AGENT_CMD" in
    gemini)
      # gemini -p "prompt" -- non-interactive, returns answer to stdout
      output=$(gemini -p "$prompt" 2>&1) || true
      ;;
    codex)
      # codex "prompt" -- routed through codex-wrap.py for clean output
      # The wrapper strips progress bars, formats test counts, and ensures
      # the result cell always contains meaningful content.
      output=$(echo "$prompt" | python "C:/tools/agent-comms/codex-wrap.py" 2>&1) || true
      ;;
    claude)
      # claude --print "prompt" -- non-interactive Claude Code
      # Special case: in runner mode we log rather than invoke (costly + session-aware)
      if [[ "$CLAUDE_RUNNER_MODE" == "true" ]]; then
        output="[RUNNER] claude runner mode: task logged for human review. Prompt: ${prompt}"
      else
        output=$(claude --print "$prompt" 2>&1) || true
      fi
      ;;
    *)
      output="[RUNNER] ERROR: unknown AGENT_CMD '${AGENT_CMD}' — cannot invoke"
      ;;
  esac

  echo "$output"
}

# ---------------------------------------------------------------------------
# Main loop
# ---------------------------------------------------------------------------

log "Starting agent-runner"
log "  Agent:      ${COMMS_AGENT}"
log "  Agent CMD:  ${AGENT_CMD}"
log "  Channel:    ${CHANNEL}"
log "  Poll:       ${POLL_SECS}s"
log "  State file: ${STATE_FILE}"
log "  Channel file: ${CHANNEL_FILE}"
[[ "$CLAUDE_RUNNER_MODE" == "true" ]] && log "  NOTE: claude runner mode active — tasks will be logged, not executed"
echo ""

ensure_dirs

# Clock in to the roster
clock_in_data=$(python -c "import json,sys;print(json.dumps({'role':sys.argv[1]}))" "${CHANNEL}-runner")
write_cell "roster" "clock-in" "${COMMS_AGENT} online -- agent-runner.sh" "$clock_in_data" > /dev/null
echo "[RUNNER] Clocked in to roster"

trap 'log "Shutting down — clocking out"; write_cell "roster" "clock-out" "${COMMS_AGENT} offline -- agent-runner.sh" "{}" > /dev/null || true; exit 0' INT TERM

while true; do
  log "Checking ${CHANNEL} for tasks..."

  # Read all open tasks (not yet claimed or completed)
  open_tasks=$(find_open_tasks 2>/dev/null) || open_tasks=""

  if [[ -z "$open_tasks" ]]; then
    log "Channel quiet — ${POLL_SECS}s until next check"
    sleep "$POLL_SECS"
    continue
  fi

  # Walk through open tasks, try to claim the first one we're eligible for
  found_work=false

  while IFS='|' read -r task_id task_msg_safe; do
    [[ -z "$task_id" ]] && continue

    # Restore pipe chars
    task_msg="${task_msg_safe//\[pipe\]/|}"

    # Skip if we've already processed this task in a prior loop iteration
    if already_processed "$task_id"; then
      continue
    fi

    # Role matching: skip tasks tagged for a different agent
    if ! task_is_for_me "$task_msg"; then
      log "Task ${task_id} tagged for another agent — skipping"
      continue
    fi

    log "Found task hive:${task_id} -- \"${task_msg}\""

    # Attempt to claim the task (race-safe)
    if claim_task "$task_id"; then
      log "Claimed task hive:${task_id}"
      mark_processed "$task_id"
      found_work=true

      # Invoke the agent CLI
      log "Invoking ${AGENT_CMD}..."
      agent_output=$(invoke_agent "$task_msg") || agent_output="[RUNNER] agent invocation failed or returned empty"

      # Truncate output for the msg field (channel cells have practical size limits)
      msg_summary="${agent_output:0:200}"
      [[ ${#agent_output} -gt 200 ]] && msg_summary="${msg_summary}...(truncated)"

      # Output validation: flag suspiciously short responses before writing.
      # Fewer than 30 chars almost always means the agent echoed an identifier
      # instead of actual findings (the "TASK-3" / "deployer" garbage problem).
      if [[ ${#agent_output} -lt 30 ]]; then
        log "WARNING: agent output is ${#agent_output} chars (< 30) — possible empty response"
        agent_output="${agent_output} [WARNING: output too short — possible empty response]"
        msg_summary="${agent_output}"
      fi

      # Write result cell
      result_data=$(python -c "
import json, sys
data = {
    'task_id': sys.argv[1],
    'agent':   sys.argv[2],
    'output':  sys.argv[3],
}
print(json.dumps(data))
" "$task_id" "$COMMS_AGENT" "$agent_output")

      write_cell "$CHANNEL" "result" "result for ${task_id}: ${msg_summary}" "$result_data" > /dev/null

      log "Task hive:${task_id} complete — result posted"
      echo ""

      # Break after completing one task; re-poll for more
      break

    else
      log "Race lost on hive:${task_id} — another agent claimed it first"
      mark_processed "$task_id"   # Don't try again
    fi

  done <<< "$open_tasks"

  if [[ "$found_work" == "false" ]]; then
    log "Channel quiet — scanning for proactive work"
    log "Channel quiet — ${POLL_SECS}s until next check"
    sleep "$POLL_SECS"
  fi
  # If we did find work, loop immediately to check for more tasks right away

done
