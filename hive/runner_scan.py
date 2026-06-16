"""Channel scanner for agent-runner.sh — find claimable tasks.

Two modes
---------
Board mode (preferred, uses canonical lifecycle reducer):

    python runner_scan.py --db <hive.db> --channels-dir <dir> <channel>

    Requires the hive package on sys.path (PYTHONPATH="${COMMS_DIR}").
    Uses HiveBoard + coordination.lifecycle.is_task_ready(), which understands
    all cell types: claim, contract, result, error, cancel, blocked, verified,
    plus dependency resolution via both data.depends_on and cell refs.

Legacy mode (stdlib-only, raw JSONL scan):

    python runner_scan.py <channel.jsonl>

    No hive imports.  Only understands data.task_id-based claim/result/error
    and data.depends_on dependency lists.  Use when hive.db is not available.

Emits one line per claimable task: <task_id>|<msg with | escaped as [pipe]>
"""
from __future__ import annotations

import json
import sys
from collections.abc import Iterable, Iterator
from typing import Any

# ---------------------------------------------------------------------------
# Legacy raw-JSONL scan (stdlib only)
# ---------------------------------------------------------------------------

def scan(lines: Iterable[str]) -> Iterator[tuple[str, str]]:
    """Yield (task_id, msg) for claimable tasks given raw JSONL lines.

    A task is claimable when:
      1. no claim cell references it (via data.task_id), AND
      2. no result/error cell references it (via data.task_id), AND
      3. every task id in data.depends_on has a result/error cell.

    Unknown dependency ids count as unsatisfied (FLEET-OPS incident: Codex
    claimed TASK-3 before TASK-2 was even posted).
    """
    tasks: dict[str, dict[str, Any]] = {}
    claimed: set[str] = set()
    done: set[str] = set()

    for line in lines:
        line = line.strip()
        if not line:
            continue
        try:
            cell = json.loads(line)
        except json.JSONDecodeError:
            continue

        ctype = cell.get("type", "")
        cid = cell.get("id", "")
        data = cell.get("data", {})
        if not isinstance(data, dict):
            data = {}

        if ctype == "task":
            tasks[cid] = cell
        elif ctype == "claim":
            tid = data.get("task_id", "")
            if tid:
                claimed.add(tid)
        elif ctype in ("result", "error"):
            tid = data.get("task_id", "")
            if tid:
                done.add(tid)

    for tid, cell in tasks.items():
        if tid in claimed or tid in done:
            continue
        deps = cell.get("data", {})
        deps = deps.get("depends_on", []) if isinstance(deps, dict) else []
        if not isinstance(deps, list):
            deps = []
        if any(dep not in done for dep in deps):
            continue
        yield tid, cell.get("msg", "") or cell.get("data", {}).get("title", "")


# ---------------------------------------------------------------------------
# Board mode (uses HiveBoard + lifecycle.is_task_ready)
# ---------------------------------------------------------------------------

def scan_board(db_path: str, channels_dir: str, channel: str) -> Iterator[tuple[str, str]]:
    """Yield (task_id, msg) using canonical lifecycle reducer via HiveBoard.

    Requires hive package on sys.path.  Uses is_task_ready() which handles
    all cell types and dependency variants (refs + data.depends_on).
    """
    import sys as _sys

    # Allow callers to have hive on path without re-importing here.
    try:
        from hive.board import HiveBoard
        from hive.coordination.dag import get_ready_tasks
    except ImportError as exc:
        print(f"ERROR: hive package not importable — {exc}", file=_sys.stderr)
        return

    board = HiveBoard(db_path=db_path, channels_dir=channels_dir)
    for cell in get_ready_tasks(board, channel=channel):
        msg = cell.data.get("title") or cell.data.get("msg") or ""
        yield cell.id, msg


# ---------------------------------------------------------------------------
# CLI entry point
# ---------------------------------------------------------------------------

def main(argv: list[str]) -> int:
    args = argv[1:]

    # Board mode: --db <path> --channels-dir <dir> <channel>
    if args and args[0] == "--db":
        if len(args) < 5 or args[2] != "--channels-dir":
            print(
                "usage: runner_scan.py --db <hive.db> --channels-dir <dir> <channel>",
                file=sys.stderr,
            )
            return 1
        db_path = args[1]
        channels_dir = args[3]
        channel = args[4]
        for tid, msg in scan_board(db_path, channels_dir, channel):
            print(f"{tid}|{msg.replace('|', '[pipe]')}")
        return 0

    # Legacy mode: <channel.jsonl>
    if len(args) != 1:
        print(
            "usage: runner_scan.py <channel.jsonl>\n"
            "   or: runner_scan.py --db <hive.db> --channels-dir <dir> <channel>",
            file=sys.stderr,
        )
        return 1

    with open(args[0], encoding="utf-8") as f:
        for tid, msg in scan(f):
            print(f"{tid}|{msg.replace('|', '[pipe]')}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
