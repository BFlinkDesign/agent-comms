"""Multi-agent racing.

When a task has race=True, multiple agents get contracts for the same task.
All results are collected and compared.
"""
from hive.board import HiveBoard
from hive.cell import Cell


def start_race(
    board: HiveBoard,
    *,
    task_id: str,
    agents: list[str],
    channel: str = "general",
    from_agent: str = "hive/racing",
) -> list[str]:
    """Create contracts for multiple agents on the same task (racing).

    Returns list of contract cell IDs.
    """
    contract_ids = []
    for agent in agents:
        contract_id = board.contract(
            from_agent=from_agent,
            channel=channel,
            task_id=task_id,
            agent=agent,
            race=True,
        )
        contract_ids.append(contract_id)
    return contract_ids


def get_race_results(board: HiveBoard, *, task_id: str) -> list[Cell]:
    """Get all results submitted for a racing task."""
    contracts = board.refs(task_id)
    race_contracts = [c for c in contracts if c.type == "contract"]

    results = []
    for contract in race_contracts:
        contract_refs = board.refs(contract.id)
        for cell in contract_refs:
            if cell.type == "result":
                results.append(cell)
    return results
