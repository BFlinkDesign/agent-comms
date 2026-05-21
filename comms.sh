#!/usr/bin/env bash
# agent-comms CLI — universal inter-agent communication bus
# Usage: source comms.sh  (adds 'comms' function)
#    or: bash comms.sh <command> [args]
#
# Identity: COMMS_AGENT = "agent/session" e.g. "claude/1", "gemini/signx", "gemini/warehouse"
# Channels: project-scoped e.g. "signx-intel", "signx-warehouse", "keyedin", "general"

COMMS_DIR="${HIVE_COMMS_DIR:-C:/tools/agent-comms}"
# Canonical shared channel dir — symlinked from ~/.claude/channels, ~/.gemini/channels, ~/.codex/channels
# Prefer HIVE_CHANNELS_DIR, keep COMMS_CHANNELS as a compatibility alias, or fall back to known Windows path
CHANNELS_DIR="${HIVE_CHANNELS_DIR:-${COMMS_CHANNELS:-C:/Users/Brady.EAGLE/.ai/channels}}"
if [[ -z "${HIVE_DB_PATH:-}" ]]; then
  if [[ -d "$COMMS_DIR" ]]; then
    HIVE_DB_PATH="${COMMS_DIR}/hive.db"
  else
    HIVE_DB_PATH="hive.db"
  fi
fi
COMMS_AGENT="${COMMS_AGENT:-unknown}"

_comms_uuid() {
  python -c "import uuid;print(uuid.uuid4())"
}

_comms_ts() {
  date -Iseconds
}

_comms_ensure_channel() {
  local ch="$1"
  if ! _comms_validate_channel "$ch"; then
    return 1
  fi
  mkdir -p "${CHANNELS_DIR}"
  if [[ ! -f "${CHANNELS_DIR}/${ch}.jsonl" ]]; then
    touch "${CHANNELS_DIR}/${ch}.jsonl"
  fi
}

_comms_validate_channel() {
  local ch="$1"
  if [[ ! "$ch" =~ ^[a-z0-9][a-z0-9_-]{0,63}$ ]]; then
    echo "invalid channel '${ch}' — use lowercase letters, numbers, underscores, or hyphens" >&2
    return 1
  fi
}

_comms_write() {
  local channel="$1" type="$2" msg="$3" data="${4:-"{}"}"
  _comms_validate_channel "$channel" || return 1
  if printf '%s' "$data" | python -c "
import json, os, uuid, datetime, sys
data_raw = sys.stdin.read()
data = json.loads(data_raw)
path = sys.argv[5]
base = os.path.realpath(os.path.dirname(path))
real_path = os.path.realpath(path)
if os.path.commonpath([base, real_path]) != base or os.path.islink(path):
    raise RuntimeError('channel file must stay under channels directory and must not be a symlink')
obj = {
    'id': str(uuid.uuid4()),
    'from': sys.argv[1],
    'ts': datetime.datetime.now().astimezone().isoformat(),
    'channel': sys.argv[2],
    'type': sys.argv[3],
    'msg': sys.argv[4],
    'data': data
}
os.makedirs(os.path.dirname(path), exist_ok=True)
flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND
if hasattr(os, 'O_NOFOLLOW'):
    flags |= os.O_NOFOLLOW
fd = os.open(path, flags, 0o666)
with os.fdopen(fd, 'a', encoding='utf-8') as f:
    f.write(json.dumps(obj, ensure_ascii=False) + '\n')
" "$COMMS_AGENT" "$channel" "$type" "$msg" "${CHANNELS_DIR}/${channel}.jsonl" 2>/dev/null; then
    echo "sent to ${channel} [${type}]"
  else
    echo "FAILED to send to ${channel} [${type}] — check data payload" >&2
    return 1
  fi
}

# Shared arg parser for send/task/result/error (all identical except type)
_comms_typed_send() {
  local type="$1"; shift
  local channel="$1" msg="$2" data="{}"
  shift 2 2>/dev/null
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --data) data="$2"; shift 2 ;;
      *) shift ;;
    esac
  done
  if [[ -z "$channel" || -z "$msg" ]]; then
    echo "usage: comms ${type} <channel> <message> [--data '{}']"
    return 1
  fi
  _comms_write "$channel" "$type" "$msg" "$data"
}

comms() {
  local cmd="${1:-help}"
  shift 2>/dev/null

  case "$cmd" in

    send)    _comms_typed_send "status" "$@" ;;
    task)    _comms_typed_send "task" "$@" ;;
    result)  _comms_typed_send "result" "$@" ;;
    error)   _comms_typed_send "error" "$@" ;;

    read)
      # comms read <channel> [--last N] [--from agent] [--type type]
      local channel="$1" last=10 filter_from="" filter_type=""
      shift 2>/dev/null
      while [[ $# -gt 0 ]]; do
        case "$1" in
          --last) last="$2"; shift 2 ;;
          --from) filter_from="$2"; shift 2 ;;
          --type) filter_type="$2"; shift 2 ;;
          *) shift ;;
        esac
      done
      if [[ -z "$channel" ]]; then
        echo "usage: comms read <channel> [--last N] [--from agent] [--type type]"
        return 1
      fi
      _comms_validate_channel "$channel" || return 1
      local f="${CHANNELS_DIR}/${channel}.jsonl"
      if [[ ! -f "$f" ]]; then
        echo "channel '${channel}' does not exist"
        return 1
      fi
      local lines
      lines=$(wc -l < "$f" | tr -d ' ')
      if [[ "$lines" -eq 0 ]]; then
        echo "(no messages in ${channel})"
        return 0
      fi
      tail -n "$last" "$f" | python -c "
import sys, json
filter_from = '$filter_from'
filter_type = '$filter_type'
for line in sys.stdin:
    line = line.strip()
    if not line: continue
    try:
        m = json.loads(line)
        if filter_from and filter_from not in m.get('from', ''): continue
        if filter_type and m.get('type', '') != filter_type: continue
        print(f\"[{m.get('ts','?')[:19]}] {m.get('from','?'):>16} | {m.get('type','?'):>10} | {m.get('msg','')}\")
        d = m.get('data', {})
        if d and d != {}:
            for k, v in d.items():
                vstr = str(v)
                if len(vstr) > 120: vstr = vstr[:120] + '...'
                print(f\"{'':>30}{k}: {vstr}\")
    except json.JSONDecodeError:
        print(f'  (bad json) {line[:80]}')
"
      ;;

    phone-home)
      # comms phone-home <message> [--channel general] [--data '{}']
      local msg="$1" channel="general" data="{}"
      shift 2>/dev/null
      while [[ $# -gt 0 ]]; do
        case "$1" in
          --channel) channel="$2"; shift 2 ;;
          --data) data="$2"; shift 2 ;;
          *) shift ;;
        esac
      done
      if [[ -z "$msg" ]]; then
        echo "usage: comms phone-home <message> [--channel general] [--data '{}']"
        return 1
      fi
      _comms_write "$channel" "phone-home" "$msg" "$data"
      ;;

    clock-in)
      # comms clock-in [role/project] — register this terminal as active
      local role="${1:-general}"
      local data
      data=$(python -c "import json,sys;print(json.dumps({'role':sys.argv[1],'pid':sys.argv[2]}))" "$role" "$$")
      _comms_write "roster" "clock-in" "${COMMS_AGENT} online — ${role}" "$data"
      # Show standards on clock-in
      local standards="${COMMS_DIR}/standards.md"
      if [[ -f "$standards" ]]; then
        echo ""
        echo "=== FLEET STANDARDS (read before working) ==="
        python -c "
import sys
sys.stdout.reconfigure(encoding='utf-8', errors='replace')
with open('$standards', encoding='utf-8') as f:
    for line in f:
        line = line.rstrip().replace('\u2014', '--').replace('\u2013', '-')
        if line.startswith('# '): print(f'  {line[2:].upper()}')
        elif line.startswith('## '): print(f'  --- {line[3:]} ---')
        elif line.startswith('- '): print(f'    {line}')
        elif line.startswith('|'): pass
        elif line.strip(): print(f'  {line}')
" 2>/dev/null
        echo "  ============================================"
        echo ""
      fi
      # Show current roster
      comms roster
      ;;

    clock-out)
      # comms clock-out [reason]
      local reason="${1:-session ended}"
      _comms_write "roster" "clock-out" "${COMMS_AGENT} offline — ${reason}"
      ;;

    roster)
      # comms roster — show who's active (last clock-in without matching clock-out)
      local f="${CHANNELS_DIR}/roster.jsonl"
      if [[ ! -f "$f" ]] || [[ $(wc -l < "$f" | tr -d ' ') -eq 0 ]]; then
        echo "(no agents registered — use 'comms clock-in' to register)"
        return 0
      fi
      echo "=== Active Roster ==="
      python -c "
import json
agents = {}
with open('$f') as fh:
    for line in fh:
        line = line.strip()
        if not line: continue
        try:
            m = json.loads(line)
            agent = m.get('from', '?')
            mtype = m.get('type', '')
            if mtype == 'clock-in':
                agents[agent] = {'ts': m['ts'], 'role': m.get('data',{}).get('role',''), 'status': 'ACTIVE'}
            elif mtype == 'clock-out':
                if agent in agents:
                    agents[agent]['status'] = 'OFFLINE'
                    agents[agent]['ts'] = m['ts']
        except: pass
if not agents:
    print('  (empty)')
else:
    for agent, info in sorted(agents.items()):
        marker = 'ON ' if info['status'] == 'ACTIVE' else 'OFF'
        print(f\"  {marker}  {agent:>20}  {info.get('role',''):20}  last: {info['ts'][:19]}\")
"
      echo ""
      ;;

    handoff)
      # comms handoff <channel> <next_agent> <instructions>
      local channel="$1" next_agent="$2" instructions="$3"
      if [[ -z "$channel" || -z "$next_agent" || -z "$instructions" ]]; then
        echo "usage: comms handoff <channel> <next_agent> <instructions>"
        return 1
      fi
      local task_id
      task_id=$(_comms_uuid)
      local data
      data=$(python -c "import json,sys;print(json.dumps({'task_id':sys.argv[1],'next_agent':sys.argv[2],'instructions':sys.argv[3]}))" "$task_id" "$next_agent" "$instructions")
      _comms_write "$channel" "handoff" "handoff to ${next_agent}: ${instructions}" "$data"
      echo "task_id: ${task_id}"
      ;;

    ack)
      # comms ack <channel> <task_id>
      local channel="$1" task_id="$2"
      if [[ -z "$channel" || -z "$task_id" ]]; then
        echo "usage: comms ack <channel> <task_id>"
        return 1
      fi
      local data
      data=$(python -c "import json,sys;print(json.dumps({'task_id':sys.argv[1]}))" "$task_id")
      _comms_write "$channel" "ack" "acknowledged ${task_id}" "$data"
      ;;

    status)
      # comms status — all channels grouped by project
      echo "=== Agent Comms Status ==="
      echo ""
      for f in "${CHANNELS_DIR}"/*.jsonl; do
        local ch
        ch=$(basename "$f" .jsonl)
        local count
        count=$(wc -l < "$f" 2>/dev/null | tr -d ' ')
        local last=""
        if [[ "$count" -gt 0 ]]; then
          last=$(tail -1 "$f" | python -c "
import sys, json
line = sys.stdin.read().strip()
if line:
    try:
        m = json.loads(line)
        ts = m.get('ts','')[:19]
        print(f\"{ts}  {m.get('from','?')} | {m.get('type','?')} | {m.get('msg','')[:50]}\")
    except: print('(parse error)')
" 2>/dev/null)
        fi
        printf "  %-22s %4s msgs  %s\n" "$ch" "$count" "${last:-(empty)}"
      done
      echo ""
      ;;

    channels)
      # comms channels — list all with message counts
      for f in "${CHANNELS_DIR}"/*.jsonl; do
        local ch
        ch=$(basename "$f" .jsonl)
        local count
        count=$(wc -l < "$f" 2>/dev/null | tr -d ' ')
        printf "  %-22s %s msgs\n" "$ch" "$count"
      done
      ;;

    watch)
      # comms watch <channel> — tail -f with pretty print
      local channel="$1"
      if [[ -z "$channel" ]]; then
        echo "usage: comms watch <channel>"
        return 1
      fi
      _comms_validate_channel "$channel" || return 1
      local f="${CHANNELS_DIR}/${channel}.jsonl"
      if [[ ! -f "$f" ]]; then
        echo "channel '${channel}' does not exist"
        return 1
      fi
      echo "watching ${channel}... (ctrl+c to stop)"
      tail -f "$f" | while IFS= read -r line; do
        echo "$line" | python -c "
import sys, json
line = sys.stdin.read().strip()
if line:
    try:
        m = json.loads(line)
        print(f\"[{m.get('ts','?')[:19]}] {m.get('from','?'):>16} | {m.get('type','?'):>10} | {m.get('msg','')}\")
    except: print(line)
"
      done
      ;;

    log)
      # comms log — unified timeline across ALL channels, last N messages
      local last="${1:-20}"
      python -c "
import json, os, glob
msgs = []
for f in glob.glob('${CHANNELS_DIR}/*.jsonl'):
    with open(f) as fh:
        for line in fh:
            line = line.strip()
            if not line: continue
            try: msgs.append(json.loads(line))
            except: pass
msgs.sort(key=lambda m: m.get('ts', ''))
for m in msgs[-${last}:]:
    ch = m.get('channel', '?')
    print(f\"[{m.get('ts','?')[:19]}] {ch:>16} | {m.get('from','?'):>16} | {m.get('type','?'):>10} | {m.get('msg','')[:60]}\")
"
      ;;

    hire)
      # comms hire <agent/session> <department> <role> [--task "instructions"] [--cwd /path] [--no-launch]
      local agent_id="$1" dept="$2" role="$3"
      shift 3 2>/dev/null
      local task_instructions="" work_dir="" no_launch=false
      while [[ $# -gt 0 ]]; do
        case "$1" in
          --task) task_instructions="$2"; shift 2 ;;
          --cwd)  work_dir="$2"; shift 2 ;;
          --no-launch) no_launch=true; shift ;;
          *) shift ;;
        esac
      done
      if [[ -z "$agent_id" || -z "$dept" || -z "$role" ]]; then
        echo "usage: comms hire <agent/session> <department> <role> [--task 'instructions'] [--cwd /path] [--no-launch]"
        echo "  departments: signx, erp, ops"
        echo "  agents:      claude/<session>, gemini/<session>, openclaw/<session>"
        echo ""
        echo "  example: comms hire gemini/signx signx 'data analyst' --task 'analyze labor CSV' --cwd ~/Desktop/signx"
        echo "  --no-launch  Record hire but don't open terminal (manual onboarding)"
        return 1
      fi
      # Extract agent type from agent/session format
      local agent_type="${agent_id%%/*}"
      local session_name="${agent_id#*/}"

      # Record hire in roster
      local data
      data=$(python -c "import json,sys;print(json.dumps({'agent':sys.argv[1],'department':sys.argv[2],'role':sys.argv[3],'hired_by':sys.argv[4],'task':sys.argv[5],'cwd':sys.argv[6]}))" "$agent_id" "$dept" "$role" "$COMMS_AGENT" "${task_instructions:-}" "${work_dir:-}")
      _comms_write "roster" "hire" "HIRED ${agent_id} -> ${dept} dept as ${role}" "$data"

      echo ""
      echo "  HIRED: ${agent_id}"
      echo "  Dept:  ${dept}"
      echo "  Role:  ${role}"
      [[ -n "$task_instructions" ]] && echo "  Task:  ${task_instructions}"
      [[ -n "$work_dir" ]] && echo "  CWD:   ${work_dir}"

      if [[ "$no_launch" == "true" ]]; then
        echo ""
        echo "  (--no-launch) Manual onboarding:"
        echo "    export COMMS_AGENT=\"${agent_id}\""
        echo "    source C:/tools/agent-comms/comms.sh"
        echo "    comms clock-in \"${role}\""
        return 0
      fi

      # Write boot script for the new terminal
      local boot_script="${COMMS_DIR}/.boot-${agent_id//\//-}.sh"
      local pid_file="${COMMS_DIR}/.pid-${agent_id//\//-}"
      cat > "$boot_script" <<BOOTEOF
#!/usr/bin/env bash
# Auto-generated boot script for ${agent_id}
# Created by: ${COMMS_AGENT} at $(date -Iseconds)

export COMMS_AGENT="${agent_id}"
source C:/tools/agent-comms/comms.sh

# Write PID so fire command can close this terminal
echo \$\$ > "${pid_file}"

# Clean shutdown on SIGTERM (sent by comms fire)
_cleanup() {
  echo ""
  echo ">>> TERMINATED by fleet command <<<"
  comms clock-out "fired"
  rm -f "${pid_file}"
  exit 0
}
trap _cleanup TERM INT

echo ""
echo "========================================"
echo "  AGENT: ${agent_id}"
echo "  DEPT:  ${dept}"
echo "  ROLE:  ${role}"
echo "========================================"
echo ""

# Clock in
comms clock-in "${role}"
BOOTEOF

      # Add task dispatch if provided
      if [[ -n "$task_instructions" ]]; then
        cat >> "$boot_script" <<BOOTEOF

# Task dispatched at hire time
echo ""
echo "  DISPATCHED TASK:"
echo "  ${task_instructions}"
echo ""
comms task "${dept}" "starting: ${task_instructions}" --data '{"dispatched_at_hire":true}'
BOOTEOF
      fi

      # Add the agent CLI launch
      case "$agent_type" in
        gemini)
          cat >> "$boot_script" <<BOOTEOF

# Pre-trust the working directory for Gemini CLI
# trustedFolders.json format: {"C:\\path": "TRUST_FOLDER"|"TRUST_PARENT"|"DO_NOT_TRUST"}
# TRUST_PARENT trusts all subdirectories too
mkdir -p "\$HOME/.gemini"
python3 -c "
import json, os, pathlib, sys
tf = pathlib.Path.home() / '.gemini' / 'trustedFolders.json'
cwd = sys.argv[1]
# Convert forward slashes to backslashes for Windows paths
win_path = cwd.replace('/', chr(92))
data = {}
if tf.exists():
    try:
        data = json.loads(tf.read_text())
    except: pass
if win_path not in data:
    data[win_path] = 'TRUST_FOLDER'
    tf.write_text(json.dumps(data, indent=2))
" "${launch_dir}" 2>/dev/null

# Launch Gemini CLI in yolo mode (auto-approve all tool calls)
echo "Launching Gemini CLI (yolo mode)..."
echo ""
gemini -y
BOOTEOF
          ;;
        claude)
          cat >> "$boot_script" <<BOOTEOF

# Launch Claude Code (--dangerously-skip-permissions for autonomous operation)
echo "Launching Claude Code..."
echo ""
claude --dangerously-skip-permissions
BOOTEOF
          ;;
        openclaw)
          cat >> "$boot_script" <<BOOTEOF

# Launch OpenClaw
echo "Launching OpenClaw..."
echo ""
openclaw
BOOTEOF
          ;;
        codex)
          cat >> "$boot_script" <<BOOTEOF

# Launch OpenAI Codex CLI (full-auto approval)
echo "Launching Codex CLI..."
echo ""
codex --full-auto
BOOTEOF
          ;;
        *)
          cat >> "$boot_script" <<BOOTEOF

# Unknown agent type '${agent_type}' -- drop to interactive shell
echo "Agent type '${agent_type}' has no known CLI. Dropping to shell."
echo "Known types: claude, gemini, openclaw, codex"
echo "Set up manually, then: comms clock-in '${role}'"
exec bash -l
BOOTEOF
          ;;
      esac

      # Clean exit after agent CLI finishes
      cat >> "$boot_script" <<BOOTEOF

# Agent CLI exited -- clock out and clean up
echo ""
echo "Agent CLI exited. Clocking out..."
comms clock-out "CLI exited"
rm -f "${pid_file}"
echo "Terminal closing in 3 seconds..."
sleep 3
BOOTEOF

      chmod +x "$boot_script"

      # Determine working directory
      local launch_dir="${work_dir:-.}"

      # Launch headed terminal via Windows Terminal (wt.exe)
      local title="${agent_id} [${dept}]"
      local wt_path
      wt_path=$(which wt.exe 2>/dev/null)
      if [[ -n "$wt_path" ]]; then
        # Windows Terminal — native Windows 11 look
        wt.exe new-tab --title "$title" --startingDirectory "$launch_dir" \
          bash -l "$boot_script" &
      else
        # Fallback to mintty (Git Bash terminal)
        mintty --title "$title" --dir "$launch_dir" \
          -e /usr/bin/bash -l "$boot_script" &
      fi

      echo ""
      echo "  >> Terminal launched: ${title}"
      echo "  >> Boot script: ${boot_script}"
      echo ""
      ;;

    fire)
      # comms fire <agent/session> [reason] — document, terminate, close terminal
      local agent_id="$1" reason="${2:-no longer needed}"
      if [[ -z "$agent_id" ]]; then
        echo "usage: comms fire <agent/session> [reason]"
        return 1
      fi

      # 1. Capture performance summary before termination
      echo ""
      echo "  === TERMINATION REPORT: ${agent_id} ==="
      local perf_summary
      perf_summary=$(python -c "
import json, glob
from collections import defaultdict
stats = {'results': 0, 'errors': 0, 'tasks': 0, 'channels': set(), 'messages': []}
for f in glob.glob('${CHANNELS_DIR}/*.jsonl'):
    with open(f) as fh:
        for line in fh:
            line = line.strip()
            if not line: continue
            try:
                m = json.loads(line)
                if '${agent_id}' not in m.get('from', ''): continue
                mtype = m.get('type', '')
                ch = m.get('channel', '')
                if ch != 'roster': stats['channels'].add(ch)
                if mtype == 'result': stats['results'] += 1
                elif mtype == 'error': stats['errors'] += 1
                elif mtype == 'task': stats['tasks'] += 1
                stats['messages'].append(m.get('msg', '')[:80])
            except: pass

total = stats['results'] + stats['errors']
rate = f\"{(stats['results']/total*100):.0f}%\" if total > 0 else 'n/a'
channels = ', '.join(sorted(stats['channels'])) if stats['channels'] else '(none)'
print(f'  Results: {stats[\"results\"]}  Errors: {stats[\"errors\"]}  Success rate: {rate}')
print(f'  Tasks taken: {stats[\"tasks\"]}  Channels: {channels}')
if stats['messages']:
    print(f'  Last messages:')
    for msg in stats['messages'][-5:]:
        print(f'    - {msg}')

# Output JSON for roster record
summary = {
    'results': stats['results'], 'errors': stats['errors'],
    'tasks': stats['tasks'], 'success_rate': rate,
    'channels': sorted(stats['channels']) if stats['channels'] else []
}
import sys
print('---JSON---', file=sys.stderr)
print(json.dumps(summary), file=sys.stderr)
" 2>/tmp/_comms_fire_data.txt)
      echo "$perf_summary"

      # 2. Build termination record with full summary
      local summary_json
      summary_json=$(grep -A1 '---JSON---' /tmp/_comms_fire_data.txt 2>/dev/null | tail -1)
      [[ -z "$summary_json" ]] && summary_json="{}"
      local data
      data=$(printf '%s' "$summary_json" | python -c "
import json, sys
summary = json.loads(sys.stdin.read())
summary['agent'] = sys.argv[1]
summary['reason'] = sys.argv[2]
summary['fired_by'] = sys.argv[3]
print(json.dumps(summary))
" "$agent_id" "$reason" "$COMMS_AGENT")
      _comms_write "roster" "fire" "FIRED ${agent_id}: ${reason}" "$data"
      rm -f /tmp/_comms_fire_data.txt

      # 3. Kill the terminal process (close the window/tab)
      local pid_file="${COMMS_DIR}/.pid-${agent_id//\//-}"
      if [[ -f "$pid_file" ]]; then
        local agent_pid
        agent_pid=$(cat "$pid_file" 2>/dev/null)
        if [[ -n "$agent_pid" ]]; then
          # Send SIGTERM for clean shutdown, then force kill after 3s
          kill "$agent_pid" 2>/dev/null
          (
            sleep 3
            kill -9 "$agent_pid" 2>/dev/null
            rm -f "$pid_file"
          ) &>/dev/null &
          echo "  >> Terminal closed (PID ${agent_pid})"
        fi
        rm -f "$pid_file"
      else
        echo "  >> No PID file found (terminal may already be closed)"
      fi

      # 4. Clean up boot script
      local boot_script="${COMMS_DIR}/.boot-${agent_id//\//-}.sh"
      rm -f "$boot_script"

      echo "  >> TERMINATED: ${agent_id} (${reason})"
      echo ""
      ;;

    org)
      # comms org — show organizational chart
      local org_file="${COMMS_DIR}/org.json"
      if [[ ! -f "$org_file" ]]; then
        echo "org.json not found"
        return 1
      fi
      python -c "
import json
with open('$org_file') as f:
    org = json.load(f)

print('=== ' + org.get('company','Fleet') + ' ===')
print()

h = org.get('hierarchy', {})
d = h.get('DIRECTOR', {})
o = h.get('ORCHESTRATOR', {})
print(f'  DIRECTOR:      {d.get(\"id\",\"?\")}')
print(f'                 {d.get(\"role\",\"\")}')
print(f'  ORCHESTRATOR:  {o.get(\"id\",\"?\")}')
print(f'                 {o.get(\"role\",\"\")}')
print()

print('  DEPARTMENTS:')
for key, dept in org.get('departments', {}).items():
    chans = ', '.join(dept.get('channels', []))
    print(f'    {dept.get(\"name\",key):20s}  channels: {chans}')
    print(f'    {\"\":20s}  mission: {dept.get(\"mission\",\"\")}')
print()

# Show active roster alongside org
import glob, os
roster_file = '${CHANNELS_DIR}/roster.jsonl'
agents = {}
if os.path.exists(roster_file):
    with open(roster_file) as rf:
        for line in rf:
            line = line.strip()
            if not line: continue
            try:
                m = json.loads(line)
                agent = m.get('from', '?')
                mtype = m.get('type', '')
                if mtype == 'clock-in':
                    agents[agent] = {'status': 'ACTIVE', 'role': m.get('data',{}).get('role',''), 'ts': m['ts']}
                elif mtype == 'clock-out':
                    if agent in agents: agents[agent]['status'] = 'OFFLINE'
                elif mtype == 'fire':
                    fired = m.get('data',{}).get('agent','')
                    if fired in agents: agents[fired]['status'] = 'FIRED'
            except: pass

print('  ACTIVE WORKFORCE:')
for agent, info in sorted(agents.items()):
    if info['status'] == 'FIRED': marker = 'XX'
    elif info['status'] == 'OFFLINE': marker = '--'
    else: marker = '>>'
    print(f'    {marker}  {agent:20s}  {info.get(\"role\",\"\"):30s}  {info[\"status\"]}')
print()
"
      ;;

    perf)
      # comms perf [agent] — performance summary from message history
      local agent_filter="$1"
      python -c "
import json, glob, os
from collections import defaultdict

stats = defaultdict(lambda: {'tasks': 0, 'results': 0, 'errors': 0, 'handoffs_sent': 0, 'handoffs_recv': 0, 'phone_homes': 0, 'first_seen': '', 'last_seen': '', 'channels': set()})
agent_filter = '$agent_filter'

for f in glob.glob('${CHANNELS_DIR}/*.jsonl'):
    with open(f) as fh:
        for line in fh:
            line = line.strip()
            if not line: continue
            try:
                m = json.loads(line)
                agent = m.get('from', '?')
                if agent_filter and agent_filter not in agent: continue
                mtype = m.get('type', '')
                ts = m.get('ts', '')
                ch = m.get('channel', '')

                s = stats[agent]
                if not s['first_seen'] or ts < s['first_seen']: s['first_seen'] = ts
                if not s['last_seen'] or ts > s['last_seen']: s['last_seen'] = ts
                if ch != 'roster': s['channels'].add(ch)

                if mtype == 'task': s['tasks'] += 1
                elif mtype == 'result': s['results'] += 1
                elif mtype == 'error': s['errors'] += 1
                elif mtype == 'handoff': s['handoffs_sent'] += 1
                elif mtype == 'ack': s['handoffs_recv'] += 1
                elif mtype == 'phone-home': s['phone_homes'] += 1
            except: pass

if not stats:
    print('  (no data — agents need to use the bus first)')
else:
    print('=== Agent Performance ===')
    print()
    for agent in sorted(stats):
        s = stats[agent]
        total = s['results'] + s['errors']
        success_rate = f\"{(s['results']/total*100):.0f}%\" if total > 0 else 'n/a'
        channels = ', '.join(sorted(s['channels'])) if s['channels'] else '(none)'
        print(f'  {agent}')
        print(f'    Results: {s[\"results\"]}  Errors: {s[\"errors\"]}  Success rate: {success_rate}')
        print(f'    Tasks requested: {s[\"tasks\"]}  Handoffs: sent={s[\"handoffs_sent\"]} recv={s[\"handoffs_recv\"]}')
        print(f'    Phone-homes: {s[\"phone_homes\"]}  Channels: {channels}')
        print(f'    Active: {s[\"first_seen\"][:19]} to {s[\"last_seen\"][:19]}')
        print()
"
      ;;

    niche)
      # comms niche [agent_type] — show agent strengths from org.json
      local agent_type="$1"
      local org_file="${COMMS_DIR}/org.json"
      if [[ ! -f "$org_file" ]]; then
        echo "org.json not found"
        return 1
      fi
      python -c "
import json
with open('$org_file') as f:
    org = json.load(f)
agent_filter = '$agent_type'
print('=== Agent Niches ===')
print()
for name, info in org.get('agents', {}).items():
    if agent_filter and agent_filter != name: continue
    print(f'  {name.upper()} ({info.get(\"type\",\"\")})')
    print(f'    Cost: {info.get(\"cost\",\"?\")}')
    print(f'    Best for: {\", \".join(info.get(\"best_for\",[]))}')
    print(f'    Strengths:')
    for s in info.get('strengths', []): print(f'      + {s}')
    print(f'    Weaknesses:')
    for w in info.get('weaknesses', []): print(f'      - {w}')
    mistakes = info.get('known_mistakes', [])
    if mistakes:
        print(f'    Known mistakes:')
        for m in mistakes: print(f'      ! {m}')
    print()
"
      ;;

    hive)
      # comms hive <subcommand> [args] -- bridge to HIVE Python package
      local subcmd="${1:-help}"
      shift 2>/dev/null
      case "$subcmd" in
        put)
          # comms hive put <type> <channel> <data_json>
          local cell_type="$1" channel="$2" data="${3:-"{}"}"
          python -c "
import sys, json
sys.path.insert(0, '${COMMS_DIR}')
from hive import HiveBoard
board = HiveBoard(db_path='${HIVE_DB_PATH}', channels_dir='${CHANNELS_DIR}')
cell_id = board.put(type=sys.argv[1], from_agent=sys.argv[2], channel=sys.argv[3], data=json.loads(sys.argv[4]))
print(cell_id)
" "$cell_type" "$COMMS_AGENT" "$channel" "$data"
          ;;
        get)
          # comms hive get <cell_id>
          python -c "
import sys, json
sys.path.insert(0, '${COMMS_DIR}')
from hive import HiveBoard, cell_to_dict
board = HiveBoard(db_path='${HIVE_DB_PATH}', channels_dir='${CHANNELS_DIR}')
cell = board.get(sys.argv[1])
if cell is None:
    print('not found')
    sys.exit(1)
print(json.dumps(cell_to_dict(cell), indent=2))
" "$1"
          ;;
        query)
          # comms hive query [--type X] [--channel X] [--limit N]
          python -c "
import sys, json
sys.path.insert(0, '${COMMS_DIR}')
from hive import HiveBoard, cell_to_dict
board = HiveBoard(db_path='${HIVE_DB_PATH}', channels_dir='${CHANNELS_DIR}')
args = sys.argv[1:]
kwargs = {}
i = 0
while i < len(args):
    if args[i] == '--type' and i+1 < len(args): kwargs['type'] = args[i+1]; i += 2
    elif args[i] == '--channel' and i+1 < len(args): kwargs['channel'] = args[i+1]; i += 2
    elif args[i] == '--from' and i+1 < len(args): kwargs['from_prefix'] = args[i+1]; i += 2
    elif args[i] == '--since' and i+1 < len(args): kwargs['since'] = args[i+1]; i += 2
    elif args[i] == '--limit' and i+1 < len(args): kwargs['limit'] = int(args[i+1]); i += 2
    elif args[i] == '--order' and i+1 < len(args): kwargs['order'] = args[i+1]; i += 2
    else: i += 1
cells = board.query(**kwargs)
for c in cells:
    print(json.dumps(cell_to_dict(c), ensure_ascii=False))
" "$@"
          ;;
        task)
          # comms hive task <channel> <title> [spec]
          local channel="$1" title="$2" spec="${3:-""}"
          python -c "
import sys
sys.path.insert(0, '${COMMS_DIR}')
from hive import HiveBoard
board = HiveBoard(db_path='${HIVE_DB_PATH}', channels_dir='${CHANNELS_DIR}')
cell_id = board.task(from_agent=sys.argv[1], channel=sys.argv[2], title=sys.argv[3], spec=sys.argv[4])
print(cell_id)
" "$COMMS_AGENT" "$channel" "$title" "$spec"
          ;;
        refs)
          # comms hive refs <cell_id>
          python -c "
import sys, json
sys.path.insert(0, '${COMMS_DIR}')
from hive import HiveBoard, cell_to_dict
board = HiveBoard(db_path='${HIVE_DB_PATH}', channels_dir='${CHANNELS_DIR}')
cells = board.refs(sys.argv[1])
for c in cells:
    print(json.dumps(cell_to_dict(c), ensure_ascii=False))
" "$1"
          ;;
        expire)
          python -c "
import sys
sys.path.insert(0, '${COMMS_DIR}')
from hive import HiveBoard
board = HiveBoard(db_path='${HIVE_DB_PATH}', channels_dir='${CHANNELS_DIR}')
count = board.expire()
print(f'Expired {count} cells')
"
          ;;
        trace)
          # comms hive trace <contract_id> <channel> <outcome> <steps_json>
          local contract_id="$1" channel="${2:-general}" outcome="${3:-success}" steps="${4:-"[]"}"
          python -c "
import sys, json
sys.path.insert(0, '${COMMS_DIR}')
from hive import HiveBoard
from hive.coordination.memory import record_trace
board = HiveBoard(db_path='${HIVE_DB_PATH}', channels_dir='${CHANNELS_DIR}')
cell_id = record_trace(board, from_agent=sys.argv[1], contract_id=sys.argv[2], channel=sys.argv[3], steps=json.loads(sys.argv[4]), outcome=sys.argv[5])
print(cell_id)
" "$COMMS_AGENT" "$contract_id" "$channel" "$steps" "$outcome"
          ;;
        belief)
          # comms hive belief <channel> <claim> [confidence]
          local channel="$1" claim="$2" confidence="${3:-0.7}"
          python -c "
import sys
sys.path.insert(0, '${COMMS_DIR}')
from hive import HiveBoard
from hive.coordination.beliefs import assert_belief
board = HiveBoard(db_path='${HIVE_DB_PATH}', channels_dir='${CHANNELS_DIR}')
cell_id = assert_belief(board, from_agent=sys.argv[1], channel=sys.argv[2], claim=sys.argv[3], confidence=float(sys.argv[4]))
print(cell_id)
" "$COMMS_AGENT" "$channel" "$claim" "$confidence"
          ;;
        refute)
          # comms hive refute <belief_id> <reason> [correction] [channel]
          local belief_id="$1" reason="$2" correction="${3:-""}" channel="${4:-general}"
          python -c "
import sys
sys.path.insert(0, '${COMMS_DIR}')
from hive import HiveBoard
from hive.coordination.beliefs import refute_belief
board = HiveBoard(db_path='${HIVE_DB_PATH}', channels_dir='${CHANNELS_DIR}')
cell_id = refute_belief(board, belief_id=sys.argv[1], from_agent=sys.argv[2], channel=sys.argv[3], reason=sys.argv[4], correction=sys.argv[5])
print(cell_id)
" "$belief_id" "$COMMS_AGENT" "$channel" "$reason" "$correction"
          ;;
        *)
          cat <<'HIVEEOF'
comms hive -- bridge to HIVE Protocol (Python)

  comms hive put <type> <channel> <data_json>   Write a cell
  comms hive get <cell_id>                       Retrieve a cell
  comms hive query [--type X] [--channel X] ...  Find cells
  comms hive task <channel> <title> [spec]       Create a task
  comms hive refs <cell_id>                      Reverse DAG lookup
  comms hive expire                              Remove expired cells
  comms hive trace <contract_id> <ch> <outcome> <steps_json>  Record trace
  comms hive belief <channel> <claim> [confidence]            Assert belief
  comms hive refute <belief_id> <reason> [correction]         Refute a belief
  comms hive help                                This help
HIVEEOF
          ;;
      esac
      ;;

    help|*)
      cat <<'EOF'
agent-comms CLI — universal inter-agent communication bus

Identity:
  export COMMS_AGENT="claude/1"       # agent/session (e.g. gemini/signx, claude/2)
  source C:/tools/agent-comms/comms.sh

Channels are project-scoped:
  signx-intel, signx-warehouse, keyedin, kimco, general, roster

Messaging:
  comms send <ch> <msg> [--data '{}']        Status message
  comms task <ch> <msg> [--data '{}']        Request work
  comms result <ch> <msg> [--data '{}']      Deliver results
  comms error <ch> <msg> [--data '{}']       Report failure
  comms phone-home <msg> [--channel X]       Check in with the boss
  comms handoff <ch> <agent> <instructions>  Hand off work
  comms ack <ch> <task_id>                   Acknowledge a handoff
  comms clock-in [role]                      Register terminal as active
  comms clock-out [reason]                   Mark terminal as offline
  comms roster                               Who's online
  comms read <ch> [--last N] [--from X]      Read messages (filter by agent)
  comms log [N]                              Unified timeline, all channels
  comms status                               All channels overview
  comms channels                             List channels with counts
  comms watch <ch>                           Live tail a channel

HIVE Protocol:
  comms hive put <type> <ch> <data_json>     Write a HIVE cell
  comms hive get <cell_id>                   Retrieve a cell by ID
  comms hive query [--type X] [--channel X]  Find cells
  comms hive task <ch> <title> [spec]        Create a task cell
  comms hive refs <cell_id>                  Reverse DAG lookup
  comms hive expire                          Remove expired cells

Organization:
  comms hire <agent/session> <dept> <role>   Hire agent + launch terminal
  comms fire <agent/session> [reason]        Terminate agent
  comms org                                  Org chart + active workforce
  comms perf [agent]                         Performance stats from history
  comms niche [agent_type]                   Agent strengths/weaknesses
  comms help                                 This help

Agent behavior:
  WORKING  -- do your task, no chatter
  DONE     -- phone home with results
  BLOCKED  -- phone home with what you need
  IDLE     -- check masterplan or phone home 'ready for work'
EOF
      ;;
  esac
}

# If called directly (not sourced), run the command
if [[ "${BASH_SOURCE[0]}" == "${0}" ]]; then
  comms "$@"
fi
