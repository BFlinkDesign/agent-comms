"""HiveBoard -- the public API facade for the HIVE protocol.

Dual-writes to SQLite (primary, queryable) + JSONL (projection, backward compat).
All queries go through SQLite. JSONL is write-only.
"""
from collections.abc import Callable
from typing import Any

from hive.cell import Cell, make_cell
from hive.config import validate_channel_name
from hive.transports.jsonl import JSONLTransport
from hive.transports.sqlite import SQLiteTransport


class HiveBoard:
    """Unified Board interface for the HIVE protocol."""

    def __init__(
        self,
        db_path: str = "hive.db",
        channels_dir: str = "channels",
    ):
        self._sqlite = SQLiteTransport(db_path)
        self._jsonl = JSONLTransport(channels_dir)

    # --- Core Board Operations ---

    def put(
        self,
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
    ) -> str:
        """Create a cell and write it to both transports. Returns cell ID."""
        cell = make_cell(
            type=type,
            from_agent=from_agent,
            channel=channel,
            data=data,
            ts=ts,
            refs=refs,
            ttl=ttl,
            tags=tags,
            sig=sig,
        )
        return self.put_cell(cell)

    def put_cell(self, cell: Cell) -> str:
        """Write a pre-built cell to both transports. Returns cell ID.

        The channel name is validated before either write, so an unsafe
        channel never produces a SQLite row or a JSONL file.
        """
        validate_channel_name(cell.channel)
        self._sqlite.put(cell)
        self._jsonl.put(cell)
        return cell.id

    def get(self, cell_id: str) -> Cell | None:
        """Retrieve a cell by ID."""
        return self._sqlite.get(cell_id)

    def query(self, **kwargs: Any) -> list[Cell]:
        """Find cells matching criteria. See SQLiteTransport.query for params."""
        return self._sqlite.query(**kwargs)

    def refs(self, cell_id: str) -> list[Cell]:
        """Return all cells that reference the given ID."""
        return self._sqlite.refs(cell_id)

    def expire(self) -> int:
        """Remove expired cells. Returns count removed."""
        return self._sqlite.expire()

    def watch(
        self,
        channel: str,
        callback: Callable[[Cell], None],
        type_filter: str | None = None,
    ) -> None:
        """Subscribe to new cells on a channel."""
        self._sqlite.watch(channel, callback, type_filter)

    # --- Convenience Methods for Common Cell Types ---

    def task(
        self,
        *,
        from_agent: str,
        channel: str,
        title: str,
        spec: str = "",
        bounty: int = 5,
        deadline: str | None = None,
        quality_gates: list[str] | None = None,
        race: bool = False,
        auto_assign: bool = False,
        refs: list[str] | None = None,
        tags: list[str] | None = None,
    ) -> str:
        """Create a task cell."""
        data: dict[str, Any] = {
            "title": title,
            "spec": spec,
            "bounty": bounty,
            "race": race,
            "auto_assign": auto_assign,
        }
        if deadline:
            data["deadline"] = deadline
        if quality_gates:
            data["quality_gates"] = quality_gates
        return self.put(type="task", from_agent=from_agent, channel=channel, data=data, refs=refs, tags=tags)

    def card(
        self,
        *,
        from_agent: str,
        capabilities: list[str],
        cost_profile: dict[str, Any] | None = None,
        models: list[str] | None = None,
        channel: str = "roster",
    ) -> str:
        """Create an agent capability card."""
        data: dict[str, Any] = {"capabilities": capabilities}
        if cost_profile:
            data["cost_profile"] = cost_profile
        if models:
            data["models"] = models
        return self.put(type="card", from_agent=from_agent, channel=channel, data=data)

    def heartbeat(
        self,
        *,
        from_agent: str,
        contract_id: str,
        progress: int = 0,
        status: str = "working",
        channel: str = "roster",
    ) -> str:
        """Create a heartbeat cell."""
        return self.put(
            type="heartbeat",
            from_agent=from_agent,
            channel=channel,
            data={"contract_id": contract_id, "progress": progress, "status": status},
            refs=[contract_id],
            ttl=120,
        )

    def result(
        self,
        *,
        from_agent: str,
        channel: str,
        contract_id: str,
        output: str,
        artifacts: list[str] | None = None,
        metrics: dict[str, Any] | None = None,
    ) -> str:
        """Create a result cell."""
        data: dict[str, Any] = {"output": output}
        if artifacts:
            data["artifacts"] = artifacts
        if metrics:
            data["metrics"] = metrics
        return self.put(type="result", from_agent=from_agent, channel=channel, data=data, refs=[contract_id])

    def feedback(
        self,
        *,
        from_agent: str,
        channel: str,
        result_id: str,
        contract_id: str,
        score: int,
        notes: str = "",
        tags: list[str] | None = None,
    ) -> str:
        """Create a feedback cell."""
        return self.put(
            type="feedback",
            from_agent=from_agent,
            channel=channel,
            data={"score": score, "notes": notes},
            refs=[result_id, contract_id],
            tags=tags,
        )
