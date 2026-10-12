"""Belief management -- auditable agent priors for HIVE.

Agents write "belief" cells to explicitly state what they assume is true
before acting. A Critic agent can inspect these beliefs, identify wrong ones,
and emit corrections. This enables surgical repair instead of blind retries.

Example workflow:
    1. Agent writes a belief: "I believe error 1102 is caused by sequential requests"
    2. Agent acts on that belief
    3. If it fails, the Critic finds the belief cell and marks it as "refuted"
    4. The evolution module detects refuted beliefs and emits improvement signals
"""
from typing import Any

from hive.board import HiveBoard
from hive.cell import Cell


def assert_belief(
    board: HiveBoard,
    *,
    from_agent: str,
    channel: str,
    claim: str,
    confidence: float = 0.7,
    evidence: list[str] | None = None,
    refs: list[str] | None = None,
    tags: list[str] | None = None,
) -> str:
    """Record an explicit prior belief before acting.

    Args:
        claim: Natural language statement of what the agent believes to be true
        confidence: Subjective confidence 0.0 to 1.0
        evidence: List of evidence strings supporting this belief
        refs: Cell IDs this belief is about (e.g. contract, task)
    """
    return board.put(
        type="belief",
        from_agent=from_agent,
        channel=channel,
        data={
            "claim": claim,
            "confidence": confidence,
            "evidence": evidence or [],
            "status": "active",  # active | confirmed | refuted
        },
        refs=refs or [],
        tags=tags or [],
    )


def refute_belief(
    board: HiveBoard,
    *,
    belief_id: str,
    from_agent: str,
    channel: str,
    reason: str,
    correction: str | None = None,
) -> str:
    """Mark a belief as refuted by evidence.

    Emits a "refutation" cell referencing the original belief.
    This is picked up by the evolution module to generate improvement signals.
    """
    return board.put(
        type="refutation",
        from_agent=from_agent,
        channel=channel,
        data={
            "reason": reason,
            "correction": correction or "",
        },
        refs=[belief_id],
    )


def confirm_belief(
    board: HiveBoard,
    *,
    belief_id: str,
    from_agent: str,
    channel: str,
    evidence: str = "",
) -> str:
    """Mark a belief as confirmed by evidence."""
    return board.put(
        type="confirmation",
        from_agent=from_agent,
        channel=channel,
        data={"evidence": evidence},
        refs=[belief_id],
    )


def get_active_beliefs(
    board: HiveBoard,
    *,
    channel: str | None = None,
    from_agent: str | None = None,
    limit: int = 50,
) -> list[Cell]:
    """Get beliefs that have not been refuted or confirmed."""
    kwargs: dict[str, Any] = {"type": "belief", "limit": limit}
    if channel:
        kwargs["channel"] = channel
    if from_agent:
        kwargs["from_prefix"] = from_agent
    beliefs = board.query(**kwargs)

    active = []
    for belief in beliefs:
        children = board.refs(belief.id)
        if not any(c.type in ("refutation", "confirmation") for c in children):
            active.append(belief)
    return active


def get_refuted_beliefs(
    board: HiveBoard,
    *,
    channel: str | None = None,
    limit: int | None = 50,
) -> list[dict[str, Any]]:
    """Return refuted beliefs with their corrections.

    Used by evolution module to generate improvement signals.
    """
    kwargs: dict[str, Any] = {"type": "belief", "limit": limit}
    if channel:
        kwargs["channel"] = channel
    beliefs = board.query(**kwargs)

    refuted = []
    for belief in beliefs:
        children = board.refs(belief.id)
        refutations = [c for c in children if c.type == "refutation"]
        if refutations:
            refuted.append({
                "belief_id": belief.id,
                "claim": belief.data.get("claim", ""),
                "from_agent": belief.from_agent,
                "confidence": belief.data.get("confidence", 0.0),
                "reason": refutations[0].data.get("reason", ""),
                "correction": refutations[0].data.get("correction", ""),
            })
    return refuted


def belief_audit(board: HiveBoard, *, channel: str | None = None) -> dict[str, Any]:
    """Summary audit: how many beliefs active vs. confirmed vs. refuted."""
    kwargs: dict[str, Any] = {"type": "belief", "limit": None}
    if channel:
        kwargs["channel"] = channel
    beliefs = board.query(**kwargs)

    active_count = 0
    confirmed_count = 0
    refuted_count = 0

    for belief in beliefs:
        children = board.refs(belief.id)
        types = {c.type for c in children}
        if "refutation" in types:
            refuted_count += 1
        elif "confirmation" in types:
            confirmed_count += 1
        else:
            active_count += 1

    total = len(beliefs)
    return {
        "total": total,
        "active": active_count,
        "confirmed": confirmed_count,
        "refuted": refuted_count,
        "accuracy": round(confirmed_count / (confirmed_count + refuted_count), 2)
        if (confirmed_count + refuted_count) > 0 else None,
    }
