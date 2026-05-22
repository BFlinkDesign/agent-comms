# agent-comms

Universal inter-agent communication bus for CNC-1. File-based JSONL. Zero dependencies.

Multiple AI terminals (Claude Code, Gemini CLI, OpenClaw) coordinate through append-only JSONL channels. Any process that can `echo >> file` can participate.

## Project state

Current branch status, shipped hardening work, test count, and ranked backlog:
[`docs/PROJECT_STATE.md`](docs/PROJECT_STATE.md).

## Quick Start

```bash
export COMMS_AGENT="claude/operator"
export HIVE_CHANNELS_DIR="C:/Users/Brady.EAGLE/.ai/channels"
source ./comms.sh

comms send general "hello from claude"
comms read general --last 5
comms status
```

## Runtime Configuration

Use the `HIVE_*` variables for portable multi-terminal setups. Legacy
`COMMS_CHANNELS` still works as an alias for existing shell workflows.

| Variable | Purpose | Default |
| --- | --- | --- |
| `HIVE_CHANNELS_DIR` | Shared JSONL channel directory for CLI, runners, MCP, and dashboard | `C:/Users/Brady.EAGLE/.ai/channels` |
| `HIVE_DB_PATH` | SQLite database path for `HiveBoard` and MCP | `C:/tools/agent-comms/hive.db` |
| `HIVE_DASHBOARD_HOST` | Dashboard bind host | `127.0.0.1` |
| `HIVE_DASHBOARD_PORT` | Dashboard port | `7842` |
| `HIVE_DASHBOARD_TOKEN` | Required before binding dashboard to a non-loopback host | unset |
| `HIVE_DASHBOARD_ALLOW_ORIGINS` | Comma-separated CORS origins | localhost origins for dashboard port |

Channel names are intentionally restricted to lowercase letters, numbers,
underscores, and hyphens. This prevents channel names from escaping the shared
channel directory when they become JSONL filenames.

## Multi-Terminal Fleet Setup

1. Pick one shared channel directory:

   ```bash
   export HIVE_CHANNELS_DIR="C:/Users/Brady.EAGLE/.ai/channels"
   mkdir -p "$HIVE_CHANNELS_DIR"
   ```

2. In each terminal or IDE agent session, set a unique identity:

   ```bash
   export COMMS_AGENT="claude/architect"
   source ./comms.sh
   comms clock-in "architect"
   ```

3. Start runners with the same channel directory:

   ```bash
   HIVE_CHANNELS_DIR="$HIVE_CHANNELS_DIR" \
   COMMS_AGENT="gemini/researcher" \
   AGENT_CMD="gemini" \
   ./agent-runner.sh general
   ```

4. Start the dashboard locally:

   ```bash
   HIVE_CHANNELS_DIR="$HIVE_CHANNELS_DIR" \
   HIVE_DB_PATH="./hive.db" \
   python -m dashboard.server
   ```

The dashboard is localhost-only by default. Set `HIVE_DASHBOARD_TOKEN` before
binding to a non-loopback host or exposing it through a tunnel.

## Development

```bash
pip3 install pytest pytest-timeout -r dashboard/requirements.txt
python3 -m pytest tests/ --timeout=30 -q   # 145 tests
```

Task readiness and lifecycle state are computed by `hive.coordination.lifecycle`
(HIVE + legacy JSONL events → A2A-aligned states).

## Agent Behavior

Run this like a business. Agents are employees.

| State | Action |
|-------|--------|
| **WORKING** | Do your task. No chatter until you have something to report. |
| **DONE** | Phone home immediately with results. |
| **BLOCKED** | Phone home immediately with what you need. |
| **IDLE** | Check the masterplan or phone home "ready for work". |

```bash
comms phone-home "finished backfill - 847 records extracted"
comms phone-home "blocked - need Kimco API credentials" --data '{"blocked_on":"kimco_oauth"}'
comms phone-home "idle - ready for work"
```

## Commands

| Command | Description |
|---------|-------------|
| `comms send <ch> <msg>` | Status message |
| `comms task <ch> <msg>` | Request work |
| `comms result <ch> <msg>` | Deliver results |
| `comms error <ch> <msg>` | Report failure |
| `comms phone-home <msg>` | Check in with the boss |
| `comms handoff <ch> <agent> <task>` | Hand work to another agent |
| `comms ack <ch> <task_id>` | Acknowledge a handoff |
| `comms read <ch> [--last N]` | Read messages |
| `comms status` | Overview of all channels |
| `comms channels` | List channel names |
| `comms watch <ch>` | Live tail |

All commands accept `--data '{}'` for structured payloads.

## Channels

| Channel | Purpose |
|---------|---------|
| general | Catch-all coordination |
| backfill | Data extraction / migration |
| deploy | Deployments / releases |
| audit | Verification requests / results |
| ingest | Ingestion pipeline tasks |
| handoff | Agent-to-agent task transfers |

New channels auto-created by writing to them.

## Handoff Protocol

1. Agent A: `comms handoff backfill gemini "re-extract lordfed"`
2. Agent B: `comms ack backfill <task_id>`
3. Agent B works autonomously
4. Agent B: `comms result backfill "done - 200 records" --data '{"task_id":"..."}'`

## Raw Usage (no comms.sh)

```bash
# Send
echo '{"id":"'$(python -c 'import uuid;print(uuid.uuid4())')'",...}' >> channels/general.jsonl

# Read
tail -5 channels/general.jsonl
```

## Message Schema

```json
{"id":"uuid","from":"agent","ts":"ISO-8601","channel":"name","type":"task|status|result|error|handoff|ack|phone-home","msg":"summary","data":{}}
```
