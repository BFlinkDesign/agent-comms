"""Tests for hive.transports.sqlite -- SQLite Board transport."""
import os
import tempfile

from hive.cell import make_cell
from hive.transports.sqlite import SQLiteTransport


def _make_transport(path=None):
    if path is None:
        fd, path = tempfile.mkstemp(suffix=".db")
        os.close(fd)
    return SQLiteTransport(path), path


class TestPutAndGet:
    def test_put_returns_cell_id(self):
        t, _ = _make_transport()
        c = make_cell(type="task", from_agent="claude/1", channel="general", data={"title": "test"})
        result = t.put(c)
        assert result == c.id

    def test_get_returns_cell(self):
        t, _ = _make_transport()
        c = make_cell(type="task", from_agent="claude/1", channel="general", data={"title": "test"})
        t.put(c)
        got = t.get(c.id)
        assert got is not None
        assert got.id == c.id
        assert got.data == c.data

    def test_get_missing_returns_none(self):
        t, _ = _make_transport()
        assert t.get("hive:0000000000000000") is None

    def test_put_is_idempotent(self):
        t, _ = _make_transport()
        c = make_cell(type="task", from_agent="claude/1", channel="general", data={"title": "test"})
        id1 = t.put(c)
        id2 = t.put(c)
        assert id1 == id2


class TestQuery:
    def test_query_by_type(self):
        t, _ = _make_transport()
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={}, ts="2026-03-01T22:00:00-06:00"))
        t.put(make_cell(type="card", from_agent="claude/1", channel="general", data={}, ts="2026-03-01T22:00:01-06:00"))
        results = t.query(type="task")
        assert len(results) == 1
        assert results[0].type == "task"

    def test_query_by_channel(self):
        t, _ = _make_transport()
        t.put(make_cell(type="task", from_agent="claude/1", channel="signx", data={}, ts="2026-03-01T22:00:00-06:00"))
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={}, ts="2026-03-01T22:00:01-06:00"))
        results = t.query(channel="signx")
        assert len(results) == 1

    def test_query_by_from_prefix(self):
        t, _ = _make_transport()
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={}, ts="2026-03-01T22:00:00-06:00"))
        t.put(make_cell(type="task", from_agent="gemini/1", channel="general", data={}, ts="2026-03-01T22:00:01-06:00"))
        results = t.query(from_prefix="claude")
        assert len(results) == 1
        assert results[0].from_agent == "claude/1"

    def test_query_by_tags(self):
        t, _ = _make_transport()
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={}, tags=["priority:high", "dept:signx"], ts="2026-03-01T22:00:00-06:00"))
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={}, tags=["priority:low"], ts="2026-03-01T22:00:01-06:00"))
        results = t.query(tags=["priority:high"])
        assert len(results) == 1

    def test_query_limit(self):
        t, _ = _make_transport()
        for i in range(10):
            t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={"i": i}, ts=f"2026-03-01T22:00:{i:02d}-06:00"))
        results = t.query(type="task", limit=3)
        assert len(results) == 3

    def test_query_order_desc(self):
        t, _ = _make_transport()
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={"i": 1}, ts="2026-03-01T22:00:01-06:00"))
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={"i": 2}, ts="2026-03-01T22:00:02-06:00"))
        results = t.query(type="task", order="desc")
        assert results[0].data["i"] == 2

    def test_query_since(self):
        t, _ = _make_transport()
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={"old": True}, ts="2026-03-01T20:00:00-06:00"))
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={"new": True}, ts="2026-03-01T23:00:00-06:00"))
        results = t.query(type="task", since="2026-03-01T22:00:00-06:00")
        assert len(results) == 1
        assert results[0].data.get("new") is True

    def test_query_order_by_rowid_uses_commit_order(self):
        t, _ = _make_transport()
        # Second insert carries an EARLIER timestamp than the first.
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={"n": 1}, ts="2026-03-01T23:00:00-06:00"))
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={"n": 2}, ts="2026-03-01T20:00:00-06:00"))
        by_ts = t.query(type="task")
        by_rowid = t.query(type="task", order_by="rowid")
        assert [c.data["n"] for c in by_ts] == [2, 1]
        assert [c.data["n"] for c in by_rowid] == [1, 2]

    def test_query_order_by_rejects_unknown_column(self):
        t, _ = _make_transport()
        try:
            t.query(type="task", order_by="id; DROP TABLE cells")
        except ValueError:
            pass
        else:
            raise AssertionError("expected ValueError for invalid order_by")


class TestRefs:
    def test_refs_returns_referencing_cells(self):
        t, _ = _make_transport()
        parent = make_cell(type="task", from_agent="claude/1", channel="general", data={"title": "parent"})
        t.put(parent)
        child = make_cell(type="bid", from_agent="gemini/1", channel="general", data={"approach": "fast"}, refs=[parent.id])
        t.put(child)
        referencing = t.refs(parent.id)
        assert len(referencing) == 1
        assert referencing[0].id == child.id

    def test_refs_empty_when_no_references(self):
        t, _ = _make_transport()
        c = make_cell(type="task", from_agent="claude/1", channel="general", data={})
        t.put(c)
        assert t.refs(c.id) == []


class TestExpire:
    def test_expire_removes_expired_cells(self):
        t, _ = _make_transport()
        c = make_cell(type="heartbeat", from_agent="claude/1", channel="general", data={}, ttl=1, ts="2020-01-01T00:00:00+00:00")
        t.put(c)
        removed = t.expire()
        assert removed >= 1
        assert t.get(c.id) is None

    def test_expire_keeps_permanent_cells(self):
        t, _ = _make_transport()
        c = make_cell(type="task", from_agent="claude/1", channel="general", data={}, ttl=0)
        t.put(c)
        t.expire()
        assert t.get(c.id) is not None


class TestWatch:
    def test_watch_notifies_on_put(self):
        t, _ = _make_transport()
        received = []
        t.watch("general", lambda cell: received.append(cell))
        c = make_cell(type="task", from_agent="claude/1", channel="general", data={})
        t.put(c)
        assert len(received) == 1
        assert received[0].id == c.id

    def test_watch_filters_by_type(self):
        t, _ = _make_transport()
        received = []
        t.watch("general", lambda cell: received.append(cell), type_filter="card")
        t.put(make_cell(type="task", from_agent="claude/1", channel="general", data={}, ts="2026-03-01T22:00:00-06:00"))
        t.put(make_cell(type="card", from_agent="claude/1", channel="general", data={}, ts="2026-03-01T22:00:01-06:00"))
        assert len(received) == 1
        assert received[0].type == "card"
