"""Heartbeat monitoring and stall detection.

Checks contracts without results. If the last heartbeat is older than
the timeout, emits a stall signal.
"""
from datetime import UTC, datetime
from typing import Any

from hive.board import HiveBoard
from hive.cell import Cell


def detect_stalls(
    board: HiveBoard,
    timeout_seconds: int = 300,
) -> list[dict[str, Any]]:
    """Find contracts that appear stalled (no recent heartbeat, no result).

    Returns list of stall info dicts with contract_id, agent, last_heartbeat.
    Also emits signal cells to the board for each detected stall.

    A signal cell is emitted at most once per stall episode: if a
    stall_detected signal for the contract already exists and no heartbeat
    has arrived since it was emitted, the stall is still returned but no
    duplicate signal is written. Otherwise every poll cycle would flood the
    channel with near-identical signal cells (their content-addressed IDs
    differ each run because age_seconds changes).
    """
    # Unbounded: a truncated scan would silently stop watching contracts.
    contracts = board.query(type="contract", limit=None)
    stalls = []

    for contract in contracts:
        # Check if there's a result for this contract
        refs = board.refs(contract.id)
        has_result = any(r.type == "result" for r in refs)
        if has_result:
            continue

        # Check last heartbeat
        heartbeats = [r for r in refs if r.type == "heartbeat"]
        heartbeats.sort(key=lambda h: h.ts, reverse=True)

        last_hb_ts = heartbeats[0].ts if heartbeats else None
        now = datetime.now(UTC)

        if last_hb_ts:
            try:
                last_dt = datetime.fromisoformat(last_hb_ts)
                age = (now - last_dt).total_seconds()
            except ValueError:
                age = timeout_seconds + 1
        else:
            # No heartbeats -- check contract age
            try:
                contract_dt = datetime.fromisoformat(contract.ts)
                age = (now - contract_dt).total_seconds()
            except ValueError:
                age = timeout_seconds + 1

        if age > timeout_seconds:
            agent = contract.data.get("agent", "unknown")
            stall_info = {
                "contract_id": contract.id,
                "agent": agent,
                "last_heartbeat": last_hb_ts,
                "age_seconds": age,
            }
            stalls.append(stall_info)

            if _already_signaled(refs, last_hb_ts):
                continue

            # Emit signal
            board.put(
                type="signal",
                from_agent="hive/stall-detector",
                channel=contract.channel,
                data={
                    "event": "stall_detected",
                    "payload": stall_info,
                },
                refs=[contract.id],
            )

    return stalls


def _already_signaled(contract_refs: list[Cell], last_hb_ts: str | None) -> bool:
    """True if a stall signal for this episode was already emitted.

    A heartbeat newer than the latest signal starts a new episode (the agent
    made progress, then stalled again), so a fresh signal is warranted.
    """
    signal_ts: list[str] = [
        r.ts
        for r in contract_refs
        if r.type == "signal" and r.data.get("event") == "stall_detected"
    ]
    if not signal_ts:
        return False
    if last_hb_ts is None:
        return True
    return max(signal_ts) > last_hb_ts
