"""Cell -- the atomic, immutable, content-addressable unit of HIVE.

Every piece of data in the protocol is a Cell. Cells are immutable once created.
Their ID is deterministic: SHA-256 of (type + from + ts + channel + JSON(data)).
Cells link to each other via `refs`, forming a DAG.
"""
import hashlib
import json
from dataclasses import dataclass, field
from datetime import datetime, timezone
from typing import Any


@dataclass(frozen=True)
class Cell:
    """An immutable, content-addressable HIVE cell."""

    id: str
    v: int
    type: str
    from_agent: str  # 'from' is reserved in Python
    ts: str
    channel: str
    data: dict[str, Any]
    refs: tuple[str, ...] = ()
    ttl: int = 0
    tags: tuple[str, ...] = ()
    sig: str | None = None


def _generate_id(type: str, from_agent: str, ts: str, channel: str, data: dict[str, Any]) -> str:
    """Generate deterministic content-addressable cell ID.

    Formula: hive: + SHA-256(type + from + ts + channel + JSON(data))[:16]
    JSON is serialized with compact separators and sorted keys for determinism.
    """
    data_json = json.dumps(data, separators=(",", ":"), sort_keys=True)
    payload = f"{type}{from_agent}{ts}{channel}{data_json}"
    digest = hashlib.sha256(payload.encode("utf-8")).hexdigest()[:16]
    return f"hive:{digest}"


def make_cell(
    *,
    type: str,
    from_agent: str,
    channel: str,
    data: dict[str, Any],
    ts: str | None = None,
    refs: list[str] | None = None,
    ttl: int = 0,
    tags: list[str] | None = None,
    sig: str | None = None,
) -> Cell:
    """Create a new Cell with auto-generated ID and timestamp."""
    if ts is None:
        ts = datetime.now(timezone.utc).astimezone().isoformat()
    cell_refs = tuple(refs) if refs else ()
    cell_tags = tuple(tags) if tags else ()
    cell_id = _generate_id(type, from_agent, ts, channel, data)
    return Cell(
        id=cell_id,
        v=1,
        type=type,
        from_agent=from_agent,
        ts=ts,
        channel=channel,
        data=data,
        refs=cell_refs,
        ttl=ttl,
        tags=cell_tags,
        sig=sig,
    )


def cell_to_dict(cell: Cell) -> dict[str, Any]:
    """Serialize a Cell to a JSON-compatible dict.

    Maps `from_agent` back to `from` for protocol compatibility.
    """
    return {
        "id": cell.id,
        "v": cell.v,
        "type": cell.type,
        "from": cell.from_agent,
        "ts": cell.ts,
        "channel": cell.channel,
        "refs": list(cell.refs),
        "ttl": cell.ttl,
        "tags": list(cell.tags),
        "data": cell.data,
        "sig": cell.sig,
    }


def cell_from_dict(d: dict[str, Any]) -> Cell:
    """Deserialize a dict to a Cell.

    Handles `from` -> `from_agent` mapping and missing optional fields.
    """
    return Cell(
        id=d["id"],
        v=d.get("v", 1),
        type=d["type"],
        from_agent=d["from"],
        ts=d["ts"],
        channel=d["channel"],
        data=d.get("data", {}),
        refs=tuple(d.get("refs", [])),
        ttl=d.get("ttl", 0),
        tags=tuple(d.get("tags", [])),
        sig=d.get("sig"),
    )
