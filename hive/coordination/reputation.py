"""Reputation scoring from feedback cells.

Uses exponential decay weighting -- recent feedback matters more.
Default score is 5.0 (neutral) when no feedback exists.
"""
from hive.board import HiveBoard

DEFAULT_SCORE = 5.0
DECAY_FACTOR = 0.95


def reputation(board: HiveBoard, agent_id: str, capability: str | None = None) -> float:
    """Calculate reputation score for an agent.

    Finds all feedback cells for contracts assigned to this agent,
    applies exponential decay weighting (most recent first).
    """
    # Find all contracts where this agent was the worker.
    # Unbounded: truncation would score reputation on a partial history.
    contracts = board.query(type="contract", limit=None)
    agent_contracts = [c for c in contracts if c.data.get("agent") == agent_id]

    if not agent_contracts:
        return DEFAULT_SCORE

    # Find all feedback referencing these contracts
    feedbacks = []
    for contract in agent_contracts:
        contract_feedbacks = board.refs(contract.id)
        for fb in contract_feedbacks:
            if fb.type == "feedback":
                if capability and capability not in fb.tags:
                    continue
                feedbacks.append(fb)

    if not feedbacks:
        return DEFAULT_SCORE

    # Sort by timestamp descending (most recent first)
    feedbacks.sort(key=lambda f: f.ts, reverse=True)

    # Exponential decay weighting
    weights = [DECAY_FACTOR ** i for i in range(len(feedbacks))]
    scores = [f.data.get("score", DEFAULT_SCORE) for f in feedbacks]

    total_weight = sum(weights)
    if total_weight == 0:
        return DEFAULT_SCORE

    # scores come from cell data (untyped JSON); coerce defensively
    return float(sum(float(s) * w for s, w in zip(scores, weights, strict=True))) / total_weight
