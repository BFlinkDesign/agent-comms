"""Tests for hive.coordination.leases."""
import os
import tempfile
from datetime import datetime, timedelta, timezone

from hive.board import HiveBoard
from hive.cell import make_cell
from hive.coordination.leases import acquire_lease, release_lease, is_leased


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(db_path=os.path.join(tmpdir, "test.db"), channels_dir=os.path.join(tmpdir, "ch"))


class TestLeases:
    def test_acquire_lease_succeeds(self):
        board = _make_board()
        lease_id = acquire_lease(board, resource="src/main.py", holder="claude/1")
        assert lease_id is not None
        assert lease_id.startswith("hive:")

    def test_is_leased_after_acquire(self):
        board = _make_board()
        acquire_lease(board, resource="src/main.py", holder="claude/1")
        assert is_leased(board, resource="src/main.py") is True

    def test_is_leased_false_when_free(self):
        board = _make_board()
        assert is_leased(board, resource="src/main.py") is False

    def test_release_frees_resource(self):
        board = _make_board()
        lease_id = acquire_lease(board, resource="src/main.py", holder="claude/1")
        release_lease(board, lease_id=lease_id, holder="claude/1")
        assert is_leased(board, resource="src/main.py") is False

    def test_cannot_acquire_already_leased(self):
        board = _make_board()
        acquire_lease(board, resource="src/main.py", holder="claude/1")
        lease2 = acquire_lease(board, resource="src/main.py", holder="gemini/1")
        assert lease2 is None  # already leased


def _put_lease_with_ts(board, *, resource, holder, ttl, ts):
    """Write a lease cell with an explicit timestamp (simulates an old lease)."""
    cell = make_cell(
        type="lease",
        from_agent=holder,
        channel="roster",
        data={"resource": resource, "holder": holder},
        ts=ts,
        ttl=ttl,
        tags=[f"resource:{resource}"],
    )
    return board.put_cell(cell)


class TestLeaseExpiry:
    def test_expired_lease_not_active(self):
        board = _make_board()
        old_ts = (datetime.now(timezone.utc) - timedelta(seconds=600)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=300, ts=old_ts)
        assert is_leased(board, resource="src/main.py") is False

    def test_can_reacquire_after_expiry(self):
        board = _make_board()
        old_ts = (datetime.now(timezone.utc) - timedelta(seconds=600)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=300, ts=old_ts)
        lease2 = acquire_lease(board, resource="src/main.py", holder="gemini/1")
        assert lease2 is not None

    def test_unexpired_lease_still_active(self):
        board = _make_board()
        recent_ts = (datetime.now(timezone.utc) - timedelta(seconds=10)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=300, ts=recent_ts)
        assert is_leased(board, resource="src/main.py") is True

    def test_zero_ttl_lease_never_expires(self):
        board = _make_board()
        old_ts = (datetime.now(timezone.utc) - timedelta(days=30)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=0, ts=old_ts)
        assert is_leased(board, resource="src/main.py") is True

    def test_naive_timestamp_treated_as_utc(self):
        board = _make_board()
        naive_old = (datetime.utcnow() - timedelta(seconds=600)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=300, ts=naive_old)
        assert is_leased(board, resource="src/main.py") is False
