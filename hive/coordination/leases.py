"""Advisory file lease management.

Agents claim leases before editing files. Other agents check before claiming.
Leases are advisory (like flock in Unix). Bad actors get bad feedback scores.
Leases expire via TTL -- no daemon needed.
"""
from datetime import UTC, datetime

from hive.board import HiveBoard
from hive.cell import Cell

DEFAULT_LEASE_TTL = 300  # 5 minutes


def acquire_lease(
    board: HiveBoard,
    *,
    resource: str,
    holder: str,
    ttl: int = DEFAULT_LEASE_TTL,
    channel: str = "roster",
) -> str | None:
    """Attempt to acquire a lease on a resource.

    Returns lease cell ID if acquired, None if already leased.

    Uses claim-then-verify: write our lease cell first, then re-read and
    keep it only if it is the arbitration winner. Two agents racing through
    the is_leased() pre-check both write claims, but the loser always
    commits after the winner, so its post-write read sees the winner's cell
    and it backs off. Winners are ordered by commit order (rowid), not ts,
    so arbitration does not depend on the writers' clocks agreeing.
    """
    if is_leased(board, resource=resource):
        return None

    lease_id = board.put(
        type="lease",
        from_agent=holder,
        channel=channel,
        data={"resource": resource, "holder": holder},
        ttl=ttl,
        tags=[f"resource:{resource}"],
    )

    winner = _active_leases(board, resource=resource)
    if winner and winner[0].id != lease_id:
        release_lease(board, lease_id=lease_id, holder=holder, channel=channel)
        return None
    return lease_id


def release_lease(
    board: HiveBoard,
    *,
    lease_id: str,
    holder: str,
    channel: str = "roster",
) -> str:
    """Release a lease."""
    return board.put(
        type="release",
        from_agent=holder,
        channel=channel,
        data={},
        refs=[lease_id],
    )


def is_leased(board: HiveBoard, *, resource: str) -> bool:
    """Check if a resource currently has an active lease.

    A lease counts as active only if it has not been released AND its TTL
    has not elapsed. Board cells are only physically deleted by expire(),
    which nothing is guaranteed to run, so expiry is enforced here at read
    time -- otherwise a crashed holder would deadlock the resource forever.
    """
    return bool(_active_leases(board, resource=resource))


def _active_leases(board: HiveBoard, *, resource: str) -> list[Cell]:
    """All unexpired, unreleased leases on a resource, in commit order.

    The first element is the arbitration winner when claims race.
    """
    leases = board.query(
        type="lease", tags=[f"resource:{resource}"], order_by="rowid", limit=None
    )
    active = []
    for lease in leases:
        if _lease_expired(lease):
            continue
        releases = board.refs(lease.id)
        if any(r.type == "release" for r in releases):
            continue  # released
        active.append(lease)
    return active


def _lease_expired(lease: Cell) -> bool:
    """True if the lease's TTL has elapsed. ttl == 0 means no expiry."""
    if lease.ttl <= 0:
        return False
    try:
        created = datetime.fromisoformat(lease.ts)
    except ValueError:
        return True  # unparseable timestamp -- treat as expired, don't deadlock
    if created.tzinfo is None:
        created = created.replace(tzinfo=UTC)
    age = (datetime.now(UTC) - created).total_seconds()
    return age > lease.ttl
