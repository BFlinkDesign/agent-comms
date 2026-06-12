"""Advisory file lease management.

Agents claim leases before editing files. Other agents check before claiming.
Leases are advisory (like flock in Unix). Bad actors get bad feedback scores.
Leases expire via TTL -- no daemon needed.
"""
from datetime import datetime, timezone

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
    """
    if is_leased(board, resource=resource):
        return None

    return board.put(
        type="lease",
        from_agent=holder,
        channel=channel,
        data={"resource": resource, "holder": holder},
        ttl=ttl,
        tags=[f"resource:{resource}"],
    )


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
    leases = board.query(type="lease", tags=[f"resource:{resource}"])
    for lease in leases:
        if _lease_expired(lease):
            continue
        releases = board.refs(lease.id)
        if any(r.type == "release" for r in releases):
            continue  # released
        return True
    return False


def _lease_expired(lease: Cell) -> bool:
    """True if the lease's TTL has elapsed. ttl == 0 means no expiry."""
    if lease.ttl <= 0:
        return False
    try:
        created = datetime.fromisoformat(lease.ts)
    except ValueError:
        return True  # unparseable timestamp -- treat as expired, don't deadlock
    if created.tzinfo is None:
        created = created.replace(tzinfo=timezone.utc)
    age = (datetime.now(timezone.utc) - created).total_seconds()
    return age > lease.ttl
