"""SQLite transport for the HIVE board protocol.

Implements all 6 Board operations:
  PUT    -- idempotent INSERT OR IGNORE
  GET    -- fetch by cell id
  QUERY  -- filtered scan with optional post-filter
  WATCH  -- in-memory callback registry, notified on PUT
  EXPIRE -- DELETE rows past their TTL
  REFS   -- find cells whose refs array contains a given cell_id

Concurrency model
-----------------
* WAL journal mode for concurrent readers + one writer.
* Thread-local sqlite3 connections (sqlite3 connections are NOT thread-safe).
* WATCH callbacks are protected by a threading.Lock.
"""
from __future__ import annotations

import json
import sqlite3
import threading
from collections.abc import Callable
from pathlib import Path
from typing import Any

from hive.cell import Cell, cell_from_dict, cell_to_dict

# ---------------------------------------------------------------------------
# Schema
# ---------------------------------------------------------------------------

_DDL = """
CREATE TABLE IF NOT EXISTS cells (
    id      TEXT PRIMARY KEY,
    v       INTEGER NOT NULL,
    type    TEXT NOT NULL,
    "from"  TEXT NOT NULL,
    ts      TEXT NOT NULL,
    channel TEXT NOT NULL,
    refs    TEXT NOT NULL DEFAULT '[]',
    ttl     INTEGER NOT NULL DEFAULT 0,
    tags    TEXT NOT NULL DEFAULT '[]',
    data    TEXT NOT NULL DEFAULT '{}',
    sig     TEXT
);

CREATE INDEX IF NOT EXISTS idx_cells_type    ON cells (type);
CREATE INDEX IF NOT EXISTS idx_cells_channel ON cells (channel);
CREATE INDEX IF NOT EXISTS idx_cells_from    ON cells ("from");
CREATE INDEX IF NOT EXISTS idx_cells_ts      ON cells (ts);
"""

# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------


def _row_to_cell(row: sqlite3.Row) -> Cell:
    """Convert a sqlite3.Row to a Cell, parsing JSON columns to tuples."""
    d: dict[str, Any] = {
        "id": row["id"],
        "v": row["v"],
        "type": row["type"],
        "from": row["from"],
        "ts": row["ts"],
        "channel": row["channel"],
        "refs": json.loads(row["refs"]),
        "ttl": row["ttl"],
        "tags": json.loads(row["tags"]),
        "data": json.loads(row["data"]),
        "sig": row["sig"],
    }
    return cell_from_dict(d)


def _cell_to_row(cell: Cell) -> dict[str, Any]:
    """Flatten a Cell to a dict of SQLite column values."""
    d = cell_to_dict(cell)
    return {
        "id": d["id"],
        "v": d["v"],
        "type": d["type"],
        "from": d["from"],
        "ts": d["ts"],
        "channel": d["channel"],
        "refs": json.dumps(d["refs"]),
        "ttl": d["ttl"],
        "tags": json.dumps(d["tags"]),
        "data": json.dumps(d["data"], separators=(",", ":"), sort_keys=True),
        "sig": d["sig"],
    }


# ---------------------------------------------------------------------------
# Transport class
# ---------------------------------------------------------------------------


class SQLiteTransport:
    """HIVE board backed by a single SQLite database file.

    Parameters
    ----------
    db_path:
        Filesystem path to the SQLite file.  Use ``":memory:"`` only for
        single-threaded tests (thread-local connections each get a separate
        in-memory DB).
    """

    def __init__(self, db_path: str | Path) -> None:
        self._db_path = str(db_path)
        self._local = threading.local()
        self._watch_lock = threading.Lock()
        # channel -> list of callbacks
        self._watchers: dict[str, list[Callable[[Cell], None]]] = {}
        # Eagerly initialise the schema on the calling thread's connection.
        self._init_schema()

    # ------------------------------------------------------------------
    # Connection management (thread-local)
    # ------------------------------------------------------------------

    def _conn(self) -> sqlite3.Connection:
        """Return (or create) the thread-local sqlite3 connection."""
        if not hasattr(self._local, "conn") or self._local.conn is None:
            conn = sqlite3.connect(self._db_path, check_same_thread=False)
            conn.row_factory = sqlite3.Row
            conn.execute("PRAGMA journal_mode=WAL")
            conn.execute("PRAGMA foreign_keys=ON")
            self._local.conn = conn
        return self._local.conn

    def _init_schema(self) -> None:
        conn = self._conn()
        conn.executescript(_DDL)
        conn.commit()

    def close(self) -> None:
        """Close the current thread's connection if open."""
        conn = getattr(self._local, "conn", None)
        if conn is not None:
            conn.close()
            self._local.conn = None

    # ------------------------------------------------------------------
    # PUT
    # ------------------------------------------------------------------

    def put(self, cell: Cell) -> str:
        """Insert a cell idempotently.  Returns the cell id.

        Uses INSERT OR IGNORE so a second PUT with the same id is a no-op
        (cells are immutable; the first write wins).
        """
        row = _cell_to_row(cell)
        conn = self._conn()
        conn.execute(
            """
            INSERT OR IGNORE INTO cells
                (id, v, type, "from", ts, channel, refs, ttl, tags, data, sig)
            VALUES
                (:id, :v, :type, :from, :ts, :channel, :refs, :ttl, :tags, :data, :sig)
            """,
            row,
        )
        conn.commit()
        self._notify_watchers(cell)
        return cell.id

    # ------------------------------------------------------------------
    # GET
    # ------------------------------------------------------------------

    def get(self, cell_id: str) -> Cell | None:
        """Fetch a single cell by id.  Returns None if not found."""
        conn = self._conn()
        cur = conn.execute("SELECT * FROM cells WHERE id = ?", (cell_id,))
        row = cur.fetchone()
        return _row_to_cell(row) if row is not None else None

    # ------------------------------------------------------------------
    # QUERY
    # ------------------------------------------------------------------

    def query(
        self,
        *,
        type: str | None = None,
        channel: str | None = None,
        from_prefix: str | None = None,
        since: str | None = None,
        tags: list[str] | tuple[str, ...] | None = None,
        refs: str | None = None,
        limit: int = 100,
        order: str = "asc",
        order_by: str = "ts",
    ) -> list[Cell]:
        """Return cells matching the given filters.

        SQL filters (applied in database):
            type        -- exact match on `type`
            channel     -- exact match on `channel`
            from_prefix -- LIKE "prefix%" match on `from`
            since       -- ts strictly greater than this ISO string
            refs        -- LIKE '%cell_id%' pre-filter (post-filtered precisely)

        Python post-filters (applied after SQL):
            tags        -- cell must contain ALL listed tags

        Parameters
        ----------
        order:
            ``"asc"`` (oldest first) or ``"desc"`` (newest first).
        order_by:
            ``"ts"`` (cell timestamp, default) or ``"rowid"`` (commit order).
            Use rowid when winner-takes-first semantics must not depend on
            the writers' clocks agreeing (e.g. lease claim arbitration).
        """
        clauses: list[str] = []
        params: list[Any] = []

        if type is not None:
            clauses.append('type = ?')
            params.append(type)
        if channel is not None:
            clauses.append('channel = ?')
            params.append(channel)
        if from_prefix is not None:
            clauses.append('"from" LIKE ?')
            params.append(from_prefix + "%")
        if since is not None:
            clauses.append('ts > ?')
            params.append(since)
        if refs is not None:
            # Pre-filter: refs column contains the id string anywhere.
            # We do an exact post-filter below.
            clauses.append('refs LIKE ?')
            params.append(f'%{refs}%')

        where = ("WHERE " + " AND ".join(clauses)) if clauses else ""
        direction = "ASC" if order.lower() == "asc" else "DESC"
        if order_by not in ("ts", "rowid"):
            raise ValueError(f"order_by must be 'ts' or 'rowid', got {order_by!r}")
        sql = f'SELECT * FROM cells {where} ORDER BY {order_by} {direction} LIMIT ?'
        params.append(limit)

        conn = self._conn()
        cur = conn.execute(sql, params)
        rows = cur.fetchall()
        cells = [_row_to_cell(r) for r in rows]

        # Python post-filter: tags (cell must have ALL requested tags)
        if tags:
            tag_set = set(tags)
            cells = [c for c in cells if tag_set.issubset(set(c.tags))]

        # Python post-filter: refs precise check (the LIKE may have false positives)
        if refs is not None:
            cells = [c for c in cells if refs in c.refs]

        return cells

    # ------------------------------------------------------------------
    # REFS
    # ------------------------------------------------------------------

    def refs(self, cell_id: str) -> list[Cell]:
        """Return all cells whose `refs` array contains `cell_id`.

        Uses a LIKE pre-filter for speed then verifies precisely in Python
        to avoid false positives (e.g. one id being a substring of another).
        """
        conn = self._conn()
        cur = conn.execute(
            "SELECT * FROM cells WHERE refs LIKE ?",
            (f"%{cell_id}%",),
        )
        rows = cur.fetchall()
        cells = [_row_to_cell(r) for r in rows]
        # Precise post-filter: id must be an element of the refs tuple
        return [c for c in cells if cell_id in c.refs]

    # ------------------------------------------------------------------
    # EXPIRE
    # ------------------------------------------------------------------

    def expire(self) -> int:
        """Delete cells whose TTL has elapsed.

        A cell with ``ttl == 0`` is permanent and never deleted.

        TTL is stored in seconds.  ``ts`` is an ISO 8601 string (may include
        timezone offset).  SQLite's ``julianday()`` understands both UTC 'Z'
        suffix and '+HH:MM' offsets as of SQLite 3.38+.

        Returns the number of rows deleted.
        """
        conn = self._conn()
        cur = conn.execute(
            """
            DELETE FROM cells
            WHERE ttl > 0
              AND (julianday('now') > julianday(ts) + CAST(ttl AS REAL) / 86400.0)
            """,
        )
        conn.commit()
        return cur.rowcount

    # ------------------------------------------------------------------
    # WATCH
    # ------------------------------------------------------------------

    def watch(
        self,
        channel: str,
        callback: Callable[[Cell], None],
        type_filter: str | None = None,
    ) -> None:
        """Register a callback to be called on every PUT to `channel`.

        If `type_filter` is given, only cells of that type trigger the callback.
        Callbacks are invoked synchronously in the thread that called PUT.
        Exceptions raised inside a callback are suppressed (logged to stderr).
        """
        with self._watch_lock:
            self._watchers.setdefault(channel, []).append((callback, type_filter))

    def unwatch(self, channel: str, callback: Callable[[Cell], None]) -> None:
        """Remove a previously registered callback."""
        with self._watch_lock:
            bucket = self._watchers.get(channel, [])
            self._watchers[channel] = [(cb, tf) for cb, tf in bucket if cb is not callback]

    def _notify_watchers(self, cell: Cell) -> None:
        with self._watch_lock:
            entries = list(self._watchers.get(cell.channel, []))
        for cb, type_filter in entries:
            if type_filter is not None and cell.type != type_filter:
                continue
            try:
                cb(cell)
            except Exception:
                pass
