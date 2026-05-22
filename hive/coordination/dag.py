"""Task DAG resolution.

Tasks can reference other tasks via refs or legacy data.depends_on. A task is
"ready" when the lifecycle reducer reports SUBMITTED with satisfied deps.
"""
from hive.board import HiveBoard
from hive.cell import Cell
from hive.coordination.lifecycle import _collect_dep_ids, is_task_ready


def get_task_deps(board: HiveBoard, task: Cell) -> list[Cell]:
    """Get all task cells that this task depends on."""
    deps: list[Cell] = []
    for ref_id in _collect_dep_ids(task):
        ref_cell = board.get(ref_id)
        if ref_cell and ref_cell.type == "task":
            deps.append(ref_cell)
    return deps


def get_ready_tasks(board: HiveBoard, channel: str | None = None) -> list[Cell]:
    """Get all tasks that are ready to be worked on."""
    kwargs = {"type": "task"}
    if channel:
        kwargs["channel"] = channel
    tasks = board.query(**kwargs)
    return [task for task in tasks if is_task_ready(board, task_or_id=task)]
