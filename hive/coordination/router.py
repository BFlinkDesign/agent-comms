"""Capability-based task routing.

Matches task requirements against agent capability cards.
Score = capability_overlap * reputation / (1 + cost).
"""
from hive.board import HiveBoard
from hive.cell import Cell
from hive.coordination.reputation import reputation


def route_task(board: HiveBoard, task: Cell) -> list[tuple[str, float]]:
    """Rank agents by suitability for a task.

    Returns list of (agent_id, score) sorted by score descending.
    """
    required = set(task.data.get("required_capabilities", []))
    # Unbounded: a truncated scan would make some agents unroutable.
    cards = board.query(type="card", limit=None)

    if not cards:
        return []

    scores = []
    for card in cards:
        agent_id = card.from_agent
        capabilities = set(card.data.get("capabilities", []))

        # Capability overlap; without requirements, any card scores 1.0
        # (0.5 for cards that declare no capabilities at all)
        overlap = 1.0 if capabilities else 0.5
        if required:
            overlap = len(required & capabilities) / len(required)

        # Cost factor
        cost_profile = card.data.get("cost_profile", {})
        cost = cost_profile.get("output", 1)

        # Reputation
        rep = reputation(board, agent_id)

        score = overlap * rep / (1 + cost)
        scores.append((agent_id, score))

    scores.sort(key=lambda x: -x[1])
    return scores
