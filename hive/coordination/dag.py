"""Task DAG resolution.

Tasks depend on other tasks via refs or the legacy data.depends_on list.
Readiness is decided by the canonical lifecycle reducer: a task is ready when
it is SUBMITTED with every dependency COMPLETE or VERIFIED.
"""
from hive.board import HiveBoard
from hive.cell import Cell
from hive.coordination.lifecycle import _collect_dep_ids, is_task_ready


def get_task_deps(board: HiveBoard, task: Cell) -> list[Cell]:
    """Get all task cells that this task depends on (refs + data.depends_on)."""
    deps: list[Cell] = []
    for ref_id in _collect_dep_ids(task):
        ref_cell = board.get(ref_id)
        if ref_cell and ref_cell.type == "task":
            deps.append(ref_cell)
    return deps


def get_ready_tasks(board: HiveBoard, channel: str | None = None) -> list[Cell]:
    """Get all tasks that are ready to be worked on.

    Ready = lifecycle status SUBMITTED with no unmet dependencies (so it has
    no claim/contract/result yet and every dependency is COMPLETE/VERIFIED).
    """
    # Unbounded: a truncated scan would silently hide ready tasks.
    kwargs: dict[str, object] = {"type": "task", "limit": None}
    if channel:
        kwargs["channel"] = channel
    tasks = board.query(**kwargs)
    return [task for task in tasks if is_task_ready(board, task_or_id=task)]
