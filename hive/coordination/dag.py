"""Task DAG resolution.

Tasks can reference other tasks via refs. A task is "ready" when all
referenced tasks have result cells.
"""
from hive.board import HiveBoard
from hive.cell import Cell


def get_task_deps(board: HiveBoard, task: Cell) -> list[Cell]:
    """Get all task cells that this task depends on (via refs)."""
    deps = []
    for ref_id in task.refs:
        ref_cell = board.get(ref_id)
        if ref_cell and ref_cell.type == "task":
            deps.append(ref_cell)
    return deps


def _task_has_result(board: HiveBoard, task_id: str) -> bool:
    """Check if a task has been completed (has a contract with a result)."""
    contracts = board.refs(task_id)
    for contract in contracts:
        if contract.type == "contract":
            results = board.refs(contract.id)
            if any(r.type == "result" for r in results):
                return True
    return False


def get_ready_tasks(board: HiveBoard, channel: str | None = None) -> list[Cell]:
    """Get all tasks that are ready to be worked on.

    A task is ready if:
    1. It has no task refs (no dependencies), OR
    2. All referenced tasks have results (dependencies satisfied)
    AND it does not already have a contract.
    """
    # Unbounded: a truncated scan would silently hide ready tasks.
    kwargs = {"type": "task", "limit": None}
    if channel:
        kwargs["channel"] = channel
    tasks = board.query(**kwargs)

    ready = []
    for task in tasks:
        # Skip tasks that already have contracts
        task_refs = board.refs(task.id)
        if any(r.type == "contract" for r in task_refs):
            continue

        # Check dependencies
        deps = get_task_deps(board, task)
        if not deps:
            ready.append(task)
            continue

        # All deps must have results
        if all(_task_has_result(board, dep.id) for dep in deps):
            ready.append(task)

    return ready
