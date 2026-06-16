"""MCP tool definitions for HIVE Board operations.

Each Board operation becomes an MCP tool. Any MCP-capable agent
can participate in the HIVE protocol natively.
"""
from typing import Any

from hive.board import HiveBoard
from hive.cell import cell_to_dict
from hive.coordination.dag import get_ready_tasks
from hive.coordination.leases import acquire_lease, is_leased, release_lease
from hive.coordination.lifecycle import get_task_state


def get_tool_definitions() -> list[dict[str, Any]]:
    """Return MCP tool definitions for all HIVE operations."""
    return [
        {
            "name": "hive_put",
            "description": "Write a cell to the HIVE board. Returns the content-addressable cell ID.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "type": {
                        "type": "string",
                        "description": "Cell type (task, card, bid, contract, result, "
                                       "feedback, lease, release, heartbeat, signal)",
                    },
                    "from_agent": {"type": "string", "description": "Agent identity (e.g. claude/1, gemini/signx)"},
                    "channel": {"type": "string", "description": "Channel name (e.g. general, signx-intel)"},
                    "data": {"type": "object", "description": "Type-specific payload"},
                    "refs": {"type": "array", "items": {"type": "string"}, "description": "IDs of related cells"},
                    "ttl": {"type": "integer", "description": "Seconds until expiry (0 = permanent)"},
                    "tags": {"type": "array", "items": {"type": "string"}, "description": "Freeform key:value tags"},
                },
                "required": ["type", "from_agent", "channel", "data"],
            },
        },
        {
            "name": "hive_get",
            "description": "Retrieve a cell by its ID.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "id": {"type": "string", "description": "Cell ID (hive:...)"},
                },
                "required": ["id"],
            },
        },
        {
            "name": "hive_query",
            "description": "Find cells matching criteria. Returns up to `limit` cells.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "type": {"type": "string"},
                    "channel": {"type": "string"},
                    "from_prefix": {
                        "type": "string",
                        "description": "Agent prefix match (e.g. 'claude' matches 'claude/1')",
                    },
                    "since": {"type": "string", "description": "ISO-8601 timestamp -- cells after this time"},
                    "tags": {"type": "array", "items": {"type": "string"}},
                    "refs": {"type": "string", "description": "Cell ID -- find cells referencing this"},
                    "limit": {"type": "integer", "default": 100},
                    "order": {"type": "string", "enum": ["asc", "desc"], "default": "asc"},
                },
            },
        },
        {
            "name": "hive_refs",
            "description": "Return all cells that reference a given cell ID (reverse DAG traversal).",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "id": {"type": "string", "description": "Cell ID to find references for"},
                },
                "required": ["id"],
            },
        },
        {
            "name": "hive_expire",
            "description": "Remove all cells past their TTL. Returns count removed.",
            "inputSchema": {"type": "object", "properties": {}},
        },
        {
            "name": "hive_task",
            "description": "Create a task cell (convenience).",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "channel": {"type": "string"},
                    "title": {"type": "string"},
                    "spec": {"type": "string", "default": ""},
                    "bounty": {"type": "integer", "default": 5},
                    "race": {"type": "boolean", "default": False},
                    "auto_assign": {"type": "boolean", "default": False},
                    "refs": {"type": "array", "items": {"type": "string"}},
                    "tags": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["from_agent", "channel", "title"],
            },
        },
        {
            "name": "hive_card",
            "description": "Publish an agent capability card.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "capabilities": {"type": "array", "items": {"type": "string"}},
                    "cost_profile": {"type": "object"},
                    "models": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["from_agent", "capabilities"],
            },
        },
        {
            "name": "hive_heartbeat",
            "description": "Send a heartbeat (alive signal while working on a contract).",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "contract_id": {"type": "string"},
                    "progress": {"type": "integer", "minimum": 0, "maximum": 100},
                    "status": {"type": "string", "default": "working"},
                },
                "required": ["from_agent", "contract_id"],
            },
        },
        {
            "name": "hive_feedback",
            "description": "Score a result (1-10). Feeds into reputation.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "channel": {"type": "string"},
                    "result_id": {"type": "string"},
                    "contract_id": {"type": "string"},
                    "score": {"type": "integer", "minimum": 1, "maximum": 10},
                    "notes": {"type": "string", "default": ""},
                    "tags": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["from_agent", "channel", "result_id", "contract_id", "score"],
            },
        },
        {
            "name": "hive_trace",
            "description": "Record a reasoning trace for a contract. "
                           "Stores HOW a problem was solved, not just the result.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "contract_id": {"type": "string", "description": "Contract this trace belongs to"},
                    "channel": {"type": "string", "default": "general"},
                    "steps": {
                        "type": "array",
                        "items": {"type": "object"},
                        "description": "Reasoning steps: [{attempt, action, outcome}]",
                    },
                    "outcome": {"type": "string", "enum": ["success", "failure", "partial"], "default": "success"},
                    "tags": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["from_agent", "contract_id"],
            },
        },
        {
            "name": "hive_belief",
            "description": "Record an explicit prior belief before acting. Enables surgical correction if wrong.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "channel": {"type": "string", "default": "general"},
                    "claim": {"type": "string", "description": "What the agent believes to be true"},
                    "confidence": {"type": "number", "minimum": 0.0, "maximum": 1.0, "default": 0.7},
                    "evidence": {"type": "array", "items": {"type": "string"}},
                    "refs": {"type": "array", "items": {"type": "string"}},
                    "tags": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["from_agent", "claim"],
            },
        },
        {
            "name": "hive_refute",
            "description": "Mark a belief as refuted. Triggers evolution signals for system improvement.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "belief_id": {"type": "string"},
                    "from_agent": {"type": "string"},
                    "channel": {"type": "string", "default": "general"},
                    "reason": {"type": "string"},
                    "correction": {"type": "string"},
                },
                "required": ["belief_id", "from_agent", "reason"],
            },
        },
        {
            "name": "hive_bid",
            "description": (
                "Post a bid for a task. Signals willingness to work on it. "
                "A bid refs the task and carries optional cost and capability metadata."
            ),
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "channel": {"type": "string"},
                    "task_id": {"type": "string", "description": "Task cell ID this bid is for"},
                    "cost": {"type": "integer", "description": "Offered cost in credits"},
                    "eta": {"type": "string", "description": "ISO-8601 estimated completion time"},
                    "tags": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["from_agent", "channel", "task_id"],
            },
        },
        {
            "name": "hive_contract",
            "description": (
                "Create a contract cell claiming a task for an agent. "
                "A contract refs the task and sets the agent to WORKING state. "
                "Use hive_task_state first to confirm the task is SUBMITTED."
            ),
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "channel": {"type": "string"},
                    "task_id": {"type": "string", "description": "Task cell ID to claim"},
                    "agent": {"type": "string", "description": "Agent that will perform the work"},
                    "race": {
                        "type": "boolean",
                        "default": False,
                        "description": "True if this is a race contract (multiple agents compete)",
                    },
                    "tags": {"type": "array", "items": {"type": "string"}},
                },
                "required": ["from_agent", "channel", "task_id", "agent"],
            },
        },
        {
            "name": "hive_lease",
            "description": (
                "Acquire an advisory lease on a named resource. Returns the lease ID if acquired, "
                "or null if the resource is already leased. Uses claim-then-verify to be race-safe."
            ),
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "resource": {"type": "string", "description": "Unique resource name to lock"},
                    "ttl": {
                        "type": "integer",
                        "default": 300,
                        "description": "Seconds until lease expires (default 300 = 5 min)",
                    },
                    "channel": {
                        "type": "string",
                        "default": "roster",
                        "description": "Channel to record the lease on",
                    },
                },
                "required": ["from_agent", "resource"],
            },
        },
        {
            "name": "hive_release",
            "description": "Release a previously acquired lease.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "lease_id": {"type": "string", "description": "Lease cell ID to release"},
                    "channel": {"type": "string", "default": "roster"},
                },
                "required": ["from_agent", "lease_id"],
            },
        },
        {
            "name": "hive_is_leased",
            "description": "Check whether a named resource currently has an active, unexpired lease.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "resource": {"type": "string", "description": "Resource name to check"},
                },
                "required": ["resource"],
            },
        },
        {
            "name": "hive_task_state",
            "description": (
                "Return the canonical lifecycle state of a task: "
                "SUBMITTED | WORKING | BLOCKED | COMPLETE | FAILED | CANCELED | VERIFIED. "
                "Also returns claimed_by, contract_id, result_id, verified_by, and "
                "unmet_dependencies. Use this before claiming a task to confirm it is "
                "still SUBMITTED with no unmet deps."
            ),
            "inputSchema": {
                "type": "object",
                "properties": {
                    "task_id": {"type": "string", "description": "Cell ID of the task (hive:...)"},
                },
                "required": ["task_id"],
            },
        },
        {
            "name": "hive_ready_tasks",
            "description": (
                "List all tasks that are ready to be claimed: SUBMITTED status with every "
                "dependency COMPLETE or VERIFIED. Optionally filter by channel."
            ),
            "inputSchema": {
                "type": "object",
                "properties": {
                    "channel": {
                        "type": "string",
                        "description": "Restrict to tasks on this channel (omit for all channels)",
                    },
                },
            },
        },
        {
            "name": "hive_result",
            "description": "Post a result cell for a contract. Marks the corresponding task COMPLETE.",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "from_agent": {"type": "string"},
                    "channel": {"type": "string"},
                    "contract_id": {"type": "string", "description": "ID of the contract being fulfilled"},
                    "output": {"type": "string", "description": "Result text / summary"},
                    "artifacts": {
                        "type": "array",
                        "items": {"type": "string"},
                        "description": "Optional list of artifact paths or URLs",
                    },
                    "metrics": {"type": "object", "description": "Optional performance metrics"},
                },
                "required": ["from_agent", "channel", "contract_id", "output"],
            },
        },
    ]


def execute_tool(board: HiveBoard, tool_name: str, args: dict[str, Any]) -> dict[str, Any]:
    """Execute an MCP tool call against the board."""
    if tool_name == "hive_put":
        cell_id = board.put(
            type=args["type"],
            from_agent=args["from_agent"],
            channel=args["channel"],
            data=args.get("data", {}),
            refs=args.get("refs"),
            ttl=args.get("ttl", 0),
            tags=args.get("tags"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_get":
        cell = board.get(args["id"])
        if cell is None:
            return {"cell": None}
        return {"cell": cell_to_dict(cell)}

    elif tool_name == "hive_query":
        query_args = {k: v for k, v in args.items() if v is not None}
        cells = board.query(**query_args)
        return {"cells": [cell_to_dict(c) for c in cells]}

    elif tool_name == "hive_refs":
        cells = board.refs(args["id"])
        return {"cells": [cell_to_dict(c) for c in cells]}

    elif tool_name == "hive_expire":
        count = board.expire()
        return {"removed": count}

    elif tool_name == "hive_task":
        cell_id = board.task(
            from_agent=args["from_agent"],
            channel=args["channel"],
            title=args["title"],
            spec=args.get("spec", ""),
            bounty=args.get("bounty", 5),
            race=args.get("race", False),
            auto_assign=args.get("auto_assign", False),
            refs=args.get("refs"),
            tags=args.get("tags"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_card":
        cell_id = board.card(
            from_agent=args["from_agent"],
            capabilities=args["capabilities"],
            cost_profile=args.get("cost_profile"),
            models=args.get("models"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_heartbeat":
        cell_id = board.heartbeat(
            from_agent=args["from_agent"],
            contract_id=args["contract_id"],
            progress=args.get("progress", 0),
            status=args.get("status", "working"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_feedback":
        cell_id = board.feedback(
            from_agent=args["from_agent"],
            channel=args["channel"],
            result_id=args["result_id"],
            contract_id=args["contract_id"],
            score=args["score"],
            notes=args.get("notes", ""),
            tags=args.get("tags"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_trace":
        from hive.coordination.memory import record_trace
        cell_id = record_trace(
            board,
            from_agent=args["from_agent"],
            contract_id=args["contract_id"],
            channel=args.get("channel", "general"),
            steps=args.get("steps", []),
            outcome=args.get("outcome", "success"),
            tags=args.get("tags"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_belief":
        from hive.coordination.beliefs import assert_belief
        cell_id = assert_belief(
            board,
            from_agent=args["from_agent"],
            channel=args.get("channel", "general"),
            claim=args["claim"],
            confidence=args.get("confidence", 0.7),
            evidence=args.get("evidence"),
            refs=args.get("refs"),
            tags=args.get("tags"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_refute":
        from hive.coordination.beliefs import refute_belief
        cell_id = refute_belief(
            board,
            belief_id=args["belief_id"],
            from_agent=args["from_agent"],
            channel=args.get("channel", "general"),
            reason=args["reason"],
            correction=args.get("correction"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_bid":
        cell_id = board.put(
            type="bid",
            from_agent=args["from_agent"],
            channel=args["channel"],
            data={
                "cost": args.get("cost"),
                "eta": args.get("eta"),
            },
            refs=[args["task_id"]],
            tags=args.get("tags"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_contract":
        cell_id = board.contract(
            from_agent=args["from_agent"],
            channel=args["channel"],
            task_id=args["task_id"],
            agent=args["agent"],
            race=args.get("race", False),
            tags=args.get("tags"),
        )
        return {"id": cell_id}

    elif tool_name == "hive_lease":
        lease_id = acquire_lease(
            board,
            resource=args["resource"],
            holder=args["from_agent"],
            ttl=args.get("ttl", 300),
            channel=args.get("channel", "roster"),
        )
        return {"id": lease_id}

    elif tool_name == "hive_release":
        release_id = release_lease(
            board,
            lease_id=args["lease_id"],
            holder=args["from_agent"],
            channel=args.get("channel", "roster"),
        )
        return {"id": release_id}

    elif tool_name == "hive_is_leased":
        return {"leased": is_leased(board, resource=args["resource"])}

    elif tool_name == "hive_task_state":
        try:
            state = get_task_state(board, task_or_id=args["task_id"])
        except ValueError as e:
            return {"error": str(e)}
        return {
            "task_id": state.task_id,
            "status": state.status,
            "claimed_by": state.claimed_by,
            "contract_id": state.contract_id,
            "result_id": state.result_id,
            "verified_by": state.verified_by,
            "unmet_dependencies": list(state.unmet_dependencies),
        }

    elif tool_name == "hive_ready_tasks":
        channel = args.get("channel")
        cells = get_ready_tasks(board, channel=channel)
        return {
            "tasks": [
                {
                    "id": c.id,
                    "title": c.data.get("title", ""),
                    "channel": c.channel,
                    "from_agent": c.from_agent,
                    "ts": c.ts,
                }
                for c in cells
            ]
        }

    elif tool_name == "hive_result":
        cell_id = board.result(
            from_agent=args["from_agent"],
            channel=args["channel"],
            contract_id=args["contract_id"],
            output=args["output"],
            artifacts=args.get("artifacts"),
            metrics=args.get("metrics"),
        )
        return {"id": cell_id}

    else:
        return {"error": f"Unknown tool: {tool_name}"}
