# agent-comms

Universal inter-agent communication bus for CNC-1. File-based JSONL. Zero dependencies.

Multiple AI terminals (Claude Code, Gemini CLI, OpenClaw) coordinate through append-only JSONL channels. Any process that can `echo >> file` can participate.

## Quick Start

```bash
export COMMS_AGENT="claude"
source C:/tools/agent-comms/comms.sh

comms send general "hello from claude"
comms read general --last 5
comms status
```

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
| `comms task-ref <ch> <msg> <task_id>` | Result cell referencing a task |
| `comms phone-home <msg>` | Check in with the boss |
| `comms handoff <ch> <agent> <task>` | Hand work to another agent |
| `comms ack <ch> <task_id>` | Acknowledge a handoff |
| `comms clock-in [role]` / `clock-out` | Register / deregister this terminal |
| `comms roster` | Who's online |
| `comms read <ch> [--last N]` | Read messages |
| `comms log [N]` | Unified timeline across all channels |
| `comms status` | Overview of all channels |
| `comms channels` | List channel names |
| `comms watch <ch>` | Live tail |
| `comms trace <contract_id> <ch> <outcome> <steps_json>` | Record reasoning trace |
| `comms belief <ch> <claim> [confidence]` | Assert a prior belief |
| `comms refute <belief_id> <reason> [correction] [ch]` | Refute a belief |
| `comms hire <agent/session> <dept> <role>` | Hire agent + launch terminal |
| `comms fire <agent/session> [reason]` | Terminate agent with report |
| `comms perf [agent]` | Performance stats from history |

Messaging commands accept `--data '{}'` for structured payloads.
`PROTOCOL.md` is the canonical command reference; a CI-tested guarantee
keeps it in sync with the CLI.

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

## Development

Four CI gates run on every PR (`.github/workflows/test.yml`); run them locally before pushing:

```bash
python -m pytest tests/ --timeout=30 -q   # test suite
mypy                                      # strict type check (hive/)
ruff check .                              # lint
bash -n comms.sh agent-runner.sh          # shell syntax
```

See `CLAUDE.md` and `AGENTS.md` for architecture and agent-facing rules.
