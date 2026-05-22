"""Canonical task lifecycle reducer.

Reduces append-only HIVE and legacy JSONL events into a single TaskState using
the A2A-inspired lifecycle from PROTOCOL.md:

    SUBMITTED -> WORKING -> BLOCKED -> COMPLETE -> FAILED -> CANCELED -> VERIFIED
"""
from __future__ import annotations

from dataclasses import dataclass, field

from hive.board import HiveBoard
from hive.cell import Cell

TERMINAL_STATUSES = frozenset({"COMPLETE", "FAILED", "CANCELED", "VERIFIED"})
SATISFIED_DEP_STATUSES = frozenset({"COMPLETE", "VERIFIED"})
_LEGACY_EVENT_TYPES = ("claim", "result", "error", "cancel", "blocked", "verified", "status")


@dataclass(frozen=True)
class TaskState:
    task_id: str
    status: str
    claimed_by: str | None = None
    contract_id: str | None = None
    result_id: str | None = None
    verified_by: str | None = None
    unmet_dependencies: tuple[str, ...] = field(default_factory=tuple)


def _resolve_task(board: HiveBoard, task_or_id: Cell | str) -> Cell:
    if isinstance(task_or_id, Cell):
        if task_or_id.type != "task":
            raise ValueError(f"expected task cell, got type={task_or_id.type!r}")
        return task_or_id
    cell = board.get(task_or_id)
    if cell is None:
        raise ValueError(f"task not found: {task_or_id}")
    if cell.type != "task":
        raise ValueError(f"expected task cell, got type={cell.type!r}")
    return cell


def _collect_dep_ids(task: Cell) -> list[str]:
    dep_ids: list[str] = []
    seen: set[str] = set()

    for ref_id in task.refs:
        if ref_id not in seen:
            dep_ids.append(ref_id)
            seen.add(ref_id)

    depends_on = task.data.get("depends_on")
    if isinstance(depends_on, list):
        for dep_id in depends_on:
            if isinstance(dep_id, str) and dep_id not in seen:
                dep_ids.append(dep_id)
                seen.add(dep_id)

    return dep_ids


def _legacy_events_for_task(board: HiveBoard, task_id: str) -> list[Cell]:
    events: list[Cell] = []
    for ctype in _LEGACY_EVENT_TYPES:
        for cell in board.query(type=ctype, unlimited=True):
            if cell.data.get("task_id") == task_id:
                events.append(cell)
    return events


def _contract_has_result(board: HiveBoard, contract_id: str) -> bool:
    for cell in board.refs(contract_id):
        if cell.type == "result":
            return True
    return False


def _result_completes_task(board: HiveBoard, cell: Cell, task_id: str) -> bool:
    if cell.data.get("task_id") == task_id:
        return True
    for ref_id in cell.refs:
        ref = board.get(ref_id)
        if ref is not None and ref.type == "contract" and task_id in ref.refs:
            return True
        if ref_id in board.refs(task_id) or ref_id == task_id:
            continue
    # Direct contract refs on the result
    for ref_id in cell.refs:
        ref = board.get(ref_id)
        if ref is not None and ref.type == "contract" and task_id in ref.refs:
            return True
    return False


def _apply_event(
    board: HiveBoard,
    *,
    task_id: str,
    cell: Cell,
    status: str,
    claimed_by: str | None,
    contract_id: str | None,
    result_id: str | None,
    verified_by: str | None,
) -> tuple[str, str | None, str | None, str | None, str | None]:
    if status in TERMINAL_STATUSES and not (status == "COMPLETE" and cell.type == "verified"):
        return status, claimed_by, contract_id, result_id, verified_by

    if cell.type == "contract" and task_id in cell.refs:
        return "WORKING", cell.data.get("agent") or cell.from_agent, cell.id, result_id, verified_by

    if cell.type == "claim" and cell.data.get("task_id") == task_id:
        return "WORKING", cell.from_agent, contract_id, result_id, verified_by

    if cell.type == "blocked" and cell.data.get("task_id") == task_id:
        return "BLOCKED", claimed_by, contract_id, result_id, verified_by

    if cell.type == "result" and _result_completes_task(board, cell, task_id):
        matched_contract = contract_id
        for ref_id in cell.refs:
            ref = board.get(ref_id)
            if ref is not None and ref.type == "contract" and task_id in ref.refs:
                matched_contract = ref_id
                break
        return "COMPLETE", claimed_by, matched_contract, cell.id, verified_by

    if cell.type == "error" and cell.data.get("task_id") == task_id:
        return "FAILED", claimed_by, contract_id, result_id, verified_by

    if cell.type == "cancel" and cell.data.get("task_id") == task_id:
        return "CANCELED", claimed_by, contract_id, result_id, verified_by

    if cell.type == "verified" and (
        cell.data.get("task_id") == task_id or task_id in cell.refs or result_id in cell.refs
    ):
        return "VERIFIED", claimed_by, contract_id, result_id, cell.from_agent

    return status, claimed_by, contract_id, result_id, verified_by


def get_task_state(
    board: HiveBoard,
    *,
    task_or_id: Cell | str,
    visited_tasks: set[str] | None = None,
) -> TaskState:
    """Reduce all known events for a task into a canonical TaskState."""
    task = _resolve_task(board, task_or_id)
    task_id = task.id

    related = list(board.refs(task_id))
    legacy = _legacy_events_for_task(board, task_id)
    seen_ids: set[str] = set()
    events: list[Cell] = []
    for cell in related + legacy:
        if cell.id not in seen_ids:
            seen_ids.add(cell.id)
            events.append(cell)
    events.sort(key=lambda c: c.ts)

    status = "SUBMITTED"
    claimed_by: str | None = None
    contract_id: str | None = None
    result_id: str | None = None
    verified_by: str | None = None

    for cell in events:
        status, claimed_by, contract_id, result_id, verified_by = _apply_event(
            board,
            task_id=task_id,
            cell=cell,
            status=status,
            claimed_by=claimed_by,
            contract_id=contract_id,
            result_id=result_id,
            verified_by=verified_by,
        )

    # If multiple contracts exist but no explicit result event matched yet, check contracts.
    if status == "WORKING":
        for cell in events:
            if cell.type == "contract" and task_id in cell.refs and _contract_has_result(board, cell.id):
                status = "COMPLETE"
                contract_id = cell.id
                for result in board.refs(cell.id):
                    if result.type == "result":
                        result_id = result.id
                        break
                break

    dep_ids = _collect_dep_ids(task)
    unmet: list[str] = []
    visit_chain = (visited_tasks or set()) | {task_id}
    for dep_id in dep_ids:
        if dep_id in visit_chain:
            unmet.append(dep_id)
            continue
        dep_cell = board.get(dep_id)
        if dep_cell is None:
            unmet.append(dep_id)
            continue
        if dep_cell.type == "task":
            dep_state = get_task_state(board, task_or_id=dep_cell, visited_tasks=visit_chain)
            if dep_state.status not in SATISFIED_DEP_STATUSES:
                unmet.append(dep_id)
        else:
            unmet.append(dep_id)

    return TaskState(
        task_id=task_id,
        status=status,
        claimed_by=claimed_by,
        contract_id=contract_id,
        result_id=result_id,
        verified_by=verified_by,
        unmet_dependencies=tuple(unmet),
    )


def get_unsatisfied_deps(board: HiveBoard, task: Cell) -> list[str]:
    """Return dependency IDs that are not yet COMPLETE or VERIFIED."""
    return list(get_task_state(board, task_or_id=task).unmet_dependencies)


def is_task_ready(board: HiveBoard, *, task_or_id: Cell | str) -> bool:
    """True when a task is SUBMITTED and all dependencies are satisfied."""
    state = get_task_state(board, task_or_id=task_or_id)
    return state.status == "SUBMITTED" and not state.unmet_dependencies
