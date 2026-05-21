"""
HIVE Dashboard Server — port 7842
Reads from ~/.ai/channels/*.jsonl and hive.db (if present)
"""

import json
import os
import sqlite3
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from fastapi import FastAPI, Query, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import JSONResponse

from hive.config import (
    ConfigError,
    channel_file_path,
    default_channels_dir,
    default_db_path,
    validate_channel_name,
)

# ---------------------------------------------------------------------------
# Config
# ---------------------------------------------------------------------------
CHANNELS_DIR = Path(default_channels_dir())
DB_PATH = Path(default_db_path())
DASHBOARD_HOST = os.environ.get("HIVE_DASHBOARD_HOST", "127.0.0.1")


def _dashboard_port() -> int:
    raw = os.environ.get("HIVE_DASHBOARD_PORT", "7842")
    try:
        port = int(raw)
    except ValueError as exc:
        raise ConfigError("HIVE_DASHBOARD_PORT must be an integer") from exc
    if not 1 <= port <= 65535:
        raise ConfigError("HIVE_DASHBOARD_PORT must be between 1 and 65535")
    return port


DASHBOARD_PORT = _dashboard_port()
DASHBOARD_TOKEN = os.environ.get("HIVE_DASHBOARD_TOKEN")
DEFAULT_ORIGINS = [f"http://127.0.0.1:{DASHBOARD_PORT}", f"http://localhost:{DASHBOARD_PORT}"]


def _configured_origins() -> list[str]:
    raw = os.environ.get("HIVE_DASHBOARD_ALLOW_ORIGINS")
    if not raw:
        return DEFAULT_ORIGINS
    return [origin.strip() for origin in raw.split(",") if origin.strip()]


if DASHBOARD_HOST not in {"127.0.0.1", "localhost", "::1"} and not DASHBOARD_TOKEN:
    raise ConfigError("HIVE_DASHBOARD_TOKEN is required when binding dashboard to a non-loopback host")

app = FastAPI(title="HIVE Dashboard", version="1.0.0")

app.add_middleware(
    CORSMiddleware,
    allow_origins=_configured_origins(),
    allow_credentials=False,
    allow_methods=["*"],
    allow_headers=["*"],
)


@app.middleware("http")
async def _require_dashboard_token(request: Request, call_next):
    client_host = request.client.host if request.client else "127.0.0.1"
    client_is_loopback = client_host in {"127.0.0.1", "localhost", "::1"}
    if not DASHBOARD_TOKEN and not client_is_loopback:
        return JSONResponse(status_code=401, content={"error": "dashboard token required"})
    if DASHBOARD_TOKEN:
        auth_header = request.headers.get("authorization", "")
        bearer = f"Bearer {DASHBOARD_TOKEN}"
        header_token = request.headers.get("x-hive-dashboard-token")
        if auth_header != bearer and header_token != DASHBOARD_TOKEN:
            return JSONResponse(status_code=401, content={"error": "dashboard token required"})
    return await call_next(request)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _read_jsonl(path: Path) -> list[dict]:
    """Read a .jsonl file, skip malformed lines, return list of dicts."""
    cells = []
    if not path.exists():
        return cells
    try:
        with open(path, encoding="utf-8") as f:
            for line in f:
                line = line.strip()
                if not line:
                    continue
                try:
                    cells.append(json.loads(line))
                except json.JSONDecodeError:
                    pass
    except OSError:
        pass
    return cells


def _channel_path(channel: str) -> Path:
    return Path(channel_file_path(str(CHANNELS_DIR), channel))


def _channel_files() -> list[Path]:
    if not CHANNELS_DIR.exists():
        return []
    files = []
    for path in CHANNELS_DIR.glob("*.jsonl"):
        if path.is_symlink():
            continue
        try:
            validate_channel_name(path.stem)
            channel_file_path(str(CHANNELS_DIR), path.stem)
        except ConfigError:
            continue
        files.append(path)
    return files


def _parse_ts(ts_str: str | None) -> datetime | None:
    if not ts_str:
        return None
    try:
        # Handle timezone offset like -06:00
        return datetime.fromisoformat(ts_str)
    except (ValueError, TypeError):
        return None


def _seconds_ago(ts_str: str | None) -> float | None:
    dt = _parse_ts(ts_str)
    if dt is None:
        return None
    now = datetime.now(timezone.utc)
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return (now - dt).total_seconds()


def _relative_time(ts_str: str | None) -> str:
    secs = _seconds_ago(ts_str)
    if secs is None:
        return "unknown"
    secs = int(secs)
    if secs < 60:
        return f"{secs}s ago"
    if secs < 3600:
        return f"{secs // 60}m ago"
    if secs < 86400:
        return f"{secs // 3600}h ago"
    return f"{secs // 86400}d ago"


def _online_threshold_secs() -> float:
    """Agent is considered online if last clock-in within 10 minutes."""
    return 600.0


def _get_db() -> sqlite3.Connection | None:
    if not DB_PATH.exists():
        return None
    try:
        conn = sqlite3.connect(str(DB_PATH))
        conn.row_factory = sqlite3.Row
        return conn
    except sqlite3.Error:
        return None


# ---------------------------------------------------------------------------
# Core data builders
# ---------------------------------------------------------------------------

def _build_roster() -> dict[str, dict]:
    """
    Returns dict keyed by agent name. Values:
    {
        name, role, department, last_seen_ts, last_seen_rel,
        online, current_task, tasks_completed, cells_sent
    }
    """
    roster_cells = _read_jsonl(_channel_path("roster"))

    agents: dict[str, dict] = {}

    # Process in chronological order
    for cell in roster_cells:
        agent_id = cell.get("from", "unknown")
        ts = cell.get("ts")
        cell_type = cell.get("type", "")
        data = cell.get("data", {}) or {}

        if agent_id not in agents:
            agents[agent_id] = {
                "name": agent_id,
                "role": "worker",
                "department": "general",
                "last_seen_ts": None,
                "online": False,
                "current_task": None,
                "tasks_completed": 0,
                "cells_sent": 0,
                "fired": False,
            }

        a = agents[agent_id]

        if cell_type == "hire":
            # hire targets another agent — update that agent
            target = data.get("agent", agent_id)
            if target not in agents:
                agents[target] = {
                    "name": target,
                    "role": "worker",
                    "department": "general",
                    "last_seen_ts": None,
                    "online": False,
                    "current_task": data.get("task"),
                    "tasks_completed": 0,
                    "cells_sent": 0,
                    "fired": False,
                }
            agents[target]["role"] = data.get("role", "worker")
            agents[target]["department"] = data.get("department", "general")
            if data.get("task"):
                agents[target]["current_task"] = data["task"]
            agents[target]["fired"] = False

        elif cell_type == "fire":
            target = data.get("agent", agent_id)
            if target in agents:
                agents[target]["fired"] = True
                agents[target]["online"] = False

        elif cell_type == "clock-in":
            a["last_seen_ts"] = ts
            a["online"] = True
            a["fired"] = False
            if data.get("role"):
                a["role"] = data["role"]

        elif cell_type == "clock-out":
            a["last_seen_ts"] = ts
            a["online"] = False

    # Second pass: scan all channel files for per-agent activity
    all_channels = _channel_files()

    agent_channel_last: dict[str, str | None] = {}  # agent -> most recent ts across channels
    agent_cell_count: dict[str, int] = {}
    agent_tasks_completed: dict[str, int] = {}

    for ch_path in all_channels:
        if ch_path.name == "roster.jsonl":
            continue
        cells = _read_jsonl(ch_path)
        for cell in cells:
            sender = cell.get("from", "")
            ts = cell.get("ts")
            cell_type = cell.get("type", "")

            agent_cell_count[sender] = agent_cell_count.get(sender, 0) + 1

            # Track most recent ts per agent
            prev = agent_channel_last.get(sender)
            if prev is None or (ts and ts > prev):
                agent_channel_last[sender] = ts

            if cell_type == "result":
                agent_tasks_completed[sender] = agent_tasks_completed.get(sender, 0) + 1

    # Merge channel activity into roster
    for agent_id, ts in agent_channel_last.items():
        if agent_id not in agents:
            agents[agent_id] = {
                "name": agent_id,
                "role": "worker",
                "department": "general",
                "last_seen_ts": ts,
                "online": False,
                "current_task": None,
                "tasks_completed": 0,
                "cells_sent": 0,
                "fired": False,
            }
        a = agents[agent_id]
        # Update last_seen if channel activity is more recent
        if ts and (a["last_seen_ts"] is None or ts > a["last_seen_ts"]):
            a["last_seen_ts"] = ts

    for agent_id, count in agent_cell_count.items():
        if agent_id in agents:
            agents[agent_id]["cells_sent"] = count

    for agent_id, count in agent_tasks_completed.items():
        if agent_id in agents:
            agents[agent_id]["tasks_completed"] = count

    # Determine online status: clock-in within threshold and not clocked out / fired
    threshold = _online_threshold_secs()
    for a in agents.values():
        if not a["fired"]:
            secs = _seconds_ago(a["last_seen_ts"])
            if secs is not None and secs <= threshold:
                a["online"] = True

    # Add computed fields
    for a in agents.values():
        a["last_seen_rel"] = _relative_time(a["last_seen_ts"])

    return agents


def _build_tasks(channel: str = "signx-intel") -> list[dict]:
    """
    Parse task cells and determine open/claimed/complete status.
    A task cell has type == 'task'.
    'claim' type references a task id (in refs or msg).
    'result'/'error' type means complete.
    Uses simple heuristic: parse TASK-N from msg text to correlate.
    """
    ch_path = _channel_path(channel)
    cells = _read_jsonl(ch_path)

    # Collect task cells
    tasks: dict[str, dict] = {}
    # key: task short id (TASK-1, etc.) or full uuid

    for cell in cells:
        cell_type = cell.get("type", "")
        msg = cell.get("msg", "") or ""
        cell_id = cell.get("id", "")
        ts = cell.get("ts", "")
        sender = cell.get("from", "")
        refs = cell.get("refs") or []
        data = cell.get("data") or {}

        if cell_type == "task":
            # Extract task key from msg prefix like "TASK-1 [agent]"
            task_key = _extract_task_key(msg, cell_id)
            tasks[task_key] = {
                "id": task_key,
                "cell_id": cell_id,
                "msg": msg,
                "assigned_to": _extract_assigned_agent(msg),
                "created_by": sender,
                "ts": ts,
                "status": "open",
                "claimed_by": None,
                "result_msg": None,
                "result_ts": None,
            }

    # Now scan for claims and results
    for cell in cells:
        cell_type = cell.get("type", "")
        msg = cell.get("msg", "") or ""
        ts = cell.get("ts", "")
        sender = cell.get("from", "")
        refs = cell.get("refs") or []

        if cell_type == "claim":
            # Find which task is being claimed
            task_key = _find_referenced_task(msg, refs, tasks)
            if task_key and task_key in tasks:
                if tasks[task_key]["status"] == "open":
                    tasks[task_key]["status"] = "claimed"
                    tasks[task_key]["claimed_by"] = sender

        elif cell_type in ("result", "error"):
            task_key = _find_referenced_task(msg, refs, tasks)
            if task_key and task_key in tasks:
                tasks[task_key]["status"] = "complete"
                tasks[task_key]["result_msg"] = msg
                tasks[task_key]["result_ts"] = ts
            else:
                # Heuristic: if msg starts with "TASK-N COMPLETE" or similar
                for tk, t in tasks.items():
                    if tk in msg and t["status"] in ("open", "claimed"):
                        t["status"] = "complete"
                        t["result_msg"] = msg
                        t["result_ts"] = ts

        elif cell_type == "status":
            # Status messages like "TASK-1 starting: ..."
            # Mark as claimed if status references a task
            task_key = _find_referenced_task(msg, refs, tasks)
            if task_key and task_key in tasks:
                if tasks[task_key]["status"] == "open":
                    tasks[task_key]["status"] = "claimed"
                    tasks[task_key]["claimed_by"] = sender

    # Add computed fields
    result = []
    for t in tasks.values():
        t["age"] = _relative_time(t["ts"])
        t["msg_preview"] = (t["msg"] or "")[:120]
        result.append(t)

    # Sort: open first, then claimed, then complete; within each by ts desc
    status_order = {"open": 0, "claimed": 1, "complete": 2}
    result.sort(key=lambda x: (status_order.get(x["status"], 9), x.get("ts", "")))
    return result


def _extract_task_key(msg: str, fallback_id: str) -> str:
    """Extract 'TASK-N' from message, else use short cell id."""
    import re
    m = re.match(r"(TASK-\d+)", msg.strip())
    if m:
        return m.group(1)
    return fallback_id[:8]


def _extract_assigned_agent(msg: str) -> str | None:
    """Extract [agent/name] from task message."""
    import re
    m = re.search(r"\[([^\]]+)\]", msg)
    if m:
        return m.group(1)
    return None


def _find_referenced_task(msg: str, refs: list, tasks: dict) -> str | None:
    """Find a task key referenced by this cell."""
    import re
    # Direct match in msg
    m = re.search(r"(TASK-\d+)", msg)
    if m:
        key = m.group(1)
        if key in tasks:
            return key
    # Check refs list (could be full cell ids)
    for ref in refs:
        if ref in tasks:
            return ref
        # Check if ref matches a cell_id in tasks
        for tk, t in tasks.items():
            if t.get("cell_id", "").startswith(ref) or ref.startswith(t.get("cell_id", "")[:8]):
                return tk
    return None


# ---------------------------------------------------------------------------
# API endpoints
# ---------------------------------------------------------------------------

@app.get("/api/agents")
def get_agents() -> Any:
    roster = _build_roster()
    # Filter out boring system/test agents, show only real ones
    real_agents = {
        k: v for k, v in roster.items()
        if not k.startswith("test/") and k != "unknown"
    }
    return JSONResponse(content={
        "agents": list(real_agents.values()),
        "all_agents": list(roster.values()),
        "ts": datetime.now(timezone.utc).isoformat(),
    })


@app.get("/api/tasks")
def get_tasks(channel: str = Query(default="signx-intel")) -> Any:
    try:
        tasks = _build_tasks(channel)
    except ConfigError as exc:
        return JSONResponse(status_code=400, content={"error": str(exc)})
    return JSONResponse(content={
        "channel": channel,
        "tasks": tasks,
        "counts": {
            "open": sum(1 for t in tasks if t["status"] == "open"),
            "claimed": sum(1 for t in tasks if t["status"] == "claimed"),
            "complete": sum(1 for t in tasks if t["status"] == "complete"),
        },
        "ts": datetime.now(timezone.utc).isoformat(),
    })


@app.get("/api/feed")
def get_feed(
    channel: str = Query(default="signx-intel"),
    limit: int = Query(default=50, le=200),
) -> Any:
    try:
        ch_path = _channel_path(channel)
    except ConfigError as exc:
        return JSONResponse(status_code=400, content={"error": str(exc)})
    cells = _read_jsonl(ch_path)

    # Newest first
    cells_desc = list(reversed(cells))[:limit]

    feed = []
    for cell in cells_desc:
        feed.append({
            "id": cell.get("id", ""),
            "from": cell.get("from", ""),
            "ts": cell.get("ts", ""),
            "ts_rel": _relative_time(cell.get("ts")),
            "type": cell.get("type", ""),
            "msg": (cell.get("msg", "") or "")[:200],
            "channel": cell.get("channel", channel),
        })

    return JSONResponse(content={
        "channel": channel,
        "cells": feed,
        "ts": datetime.now(timezone.utc).isoformat(),
    })


@app.get("/api/stats")
def get_stats() -> Any:
    if not CHANNELS_DIR.exists():
        return JSONResponse(content={"error": "channels dir not found"})

    all_channels = _channel_files()
    agent_counts: dict[str, int] = {}
    agent_results: dict[str, int] = {}
    channel_counts: dict[str, int] = {}
    total_cells = 0

    for ch_path in all_channels:
        cells = _read_jsonl(ch_path)
        channel_counts[ch_path.stem] = len(cells)
        total_cells += len(cells)
        for cell in cells:
            sender = cell.get("from", "unknown")
            agent_counts[sender] = agent_counts.get(sender, 0) + 1
            if cell.get("type") == "result":
                agent_results[sender] = agent_results.get(sender, 0) + 1

    # Pass rates per agent
    pass_rates = {}
    for agent in agent_results:
        total = agent_counts.get(agent, 1)
        pass_rates[agent] = round(agent_results[agent] / total * 100, 1)

    # DB stats
    db_stats = {}
    conn = _get_db()
    if conn:
        try:
            cur = conn.execute("SELECT name FROM sqlite_master WHERE type='table'")
            tables = [r[0] for r in cur.fetchall()]
            for t in tables:
                try:
                    count = conn.execute(f"SELECT COUNT(*) FROM {t}").fetchone()[0]  # noqa: S608
                    db_stats[t] = count
                except sqlite3.Error:
                    db_stats[t] = "?"
        except sqlite3.Error:
            pass
        finally:
            conn.close()

    return JSONResponse(content={
        "total_cells": total_cells,
        "total_channels": len(all_channels),
        "agent_cell_counts": agent_counts,
        "agent_result_counts": agent_results,
        "agent_pass_rates": pass_rates,
        "active_channels": [k for k, v in channel_counts.items() if v > 0],
        "db_tables": db_stats,
        "ts": datetime.now(timezone.utc).isoformat(),
    })


@app.get("/api/channels")
def get_channels() -> Any:
    if not CHANNELS_DIR.exists():
        return JSONResponse(content={"channels": [], "error": "channels dir not found"})

    channels = []
    for ch_path in sorted(_channel_files()):
        cells = _read_jsonl(ch_path)
        last_ts = None
        if cells:
            # Most recent ts
            tss = [c.get("ts") for c in cells if c.get("ts")]
            last_ts = max(tss) if tss else None

        channels.append({
            "name": ch_path.stem,
            "file": ch_path.name,
            "cell_count": len(cells),
            "last_activity": last_ts,
            "last_activity_rel": _relative_time(last_ts),
        })

    # Sort by cell count desc
    channels.sort(key=lambda x: x["cell_count"], reverse=True)

    return JSONResponse(content={
        "channels": channels,
        "ts": datetime.now(timezone.utc).isoformat(),
    })


@app.get("/")
def root() -> Any:
    return {"status": "HIVE Dashboard API online", "port": 7842}


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

if __name__ == "__main__":
    import uvicorn
    uvicorn.run(app, host=DASHBOARD_HOST, port=DASHBOARD_PORT, log_level="info")
