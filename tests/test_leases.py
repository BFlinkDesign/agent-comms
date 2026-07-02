"""Tests for hive.coordination.leases."""
import os
import tempfile
from datetime import UTC, datetime, timedelta

from hive.board import HiveBoard
from hive.cell import make_cell
from hive.coordination.leases import acquire_lease, is_leased, release_lease


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
        old_ts = (datetime.now(UTC) - timedelta(seconds=600)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=300, ts=old_ts)
        assert is_leased(board, resource="src/main.py") is False

    def test_can_reacquire_after_expiry(self):
        board = _make_board()
        old_ts = (datetime.now(UTC) - timedelta(seconds=600)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=300, ts=old_ts)
        lease2 = acquire_lease(board, resource="src/main.py", holder="gemini/1")
        assert lease2 is not None

    def test_unexpired_lease_still_active(self):
        board = _make_board()
        recent_ts = (datetime.now(UTC) - timedelta(seconds=10)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=300, ts=recent_ts)
        assert is_leased(board, resource="src/main.py") is True

    def test_zero_ttl_lease_never_expires(self):
        board = _make_board()
        old_ts = (datetime.now(UTC) - timedelta(days=30)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=0, ts=old_ts)
        assert is_leased(board, resource="src/main.py") is True

    def test_naive_timestamp_treated_as_utc(self):
        board = _make_board()
        naive_old = (datetime.utcnow() - timedelta(seconds=600)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="claude/1", ttl=300, ts=naive_old)
        assert is_leased(board, resource="src/main.py") is False


class TestLimitNoneScan:
    def test_active_lease_not_missed_beyond_100_history(self):
        """Regression: _active_leases must scan all history, not truncate at 100."""
        from hive.coordination.leases import _active_leases

        board = _make_board()

        # 100 released lease cells for the same resource (fills the old default limit)
        for _ in range(100):
            lid = board.put(
                type="lease",
                from_agent="claude/test",
                channel="roster",
                data={"resource": "db", "holder": "claude/test"},
                ttl=0,
                tags=["resource:db"],
            )
            board.put(type="release", from_agent="claude/test", channel="roster",
                      data={}, refs=[lid])

        # The 101st lease cell is the live, unreleased one
        active_id = board.put(
            type="lease",
            from_agent="claude/test",
            channel="roster",
            data={"resource": "db", "holder": "claude/test"},
            ttl=0,
            tags=["resource:db"],
        )

        # Without limit=None the query stops at 100, sees only released leases,
        # and incorrectly reports the resource as free.
        assert is_leased(board, resource="db") is True
        active = _active_leases(board, resource="db")
        assert len(active) == 1
        assert active[0].id == active_id


class TestLeaseRaceArbitration:
    def test_race_loser_backs_off(self, monkeypatch):
        """Two agents pass the is_leased pre-check; the later claim must lose."""
        import hive.coordination.leases as leases_mod

        board = _make_board()
        lease_a = acquire_lease(board, resource="src/main.py", holder="claude/1")
        assert lease_a is not None

        # Simulate the race window: gemini's pre-check ran before claude's
        # claim landed, so it sees the resource as free and writes a claim.
        monkeypatch.setattr(leases_mod, "is_leased", lambda *a, **k: False)
        lease_b = leases_mod.acquire_lease(board, resource="src/main.py", holder="gemini/1")
        monkeypatch.undo()

        assert lease_b is None  # verify step detected the earlier claim
        active = leases_mod._active_leases(board, resource="src/main.py")
        assert len(active) == 1
        assert active[0].id == lease_a
        assert active[0].data["holder"] == "claude/1"

    def test_winner_is_commit_order_not_timestamp(self):
        """Arbitration must use commit order so clock skew can't grant two winners."""
        from hive.coordination.leases import _active_leases

        board = _make_board()
        first = acquire_lease(board, resource="src/main.py", holder="claude/1")
        # A second claim lands later but carries an earlier (skewed) timestamp.
        skewed_ts = (datetime.now(UTC) - timedelta(seconds=60)).isoformat()
        _put_lease_with_ts(board, resource="src/main.py", holder="gemini/1", ttl=300, ts=skewed_ts)

        active = _active_leases(board, resource="src/main.py")
        assert active[0].id == first  # commit order wins, not the skewed ts
