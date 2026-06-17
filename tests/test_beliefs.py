"""Tests for hive.coordination.beliefs -- auditable agent priors."""
import os
import tempfile

from hive.board import HiveBoard
from hive.coordination.beliefs import (
    assert_belief,
    belief_audit,
    confirm_belief,
    get_active_beliefs,
    get_refuted_beliefs,
    refute_belief,
)


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(
        db_path=os.path.join(tmpdir, "test.db"),
        channels_dir=os.path.join(tmpdir, "ch"),
    )


class TestAssertBelief:
    def test_assert_belief_returns_cell_id(self):
        board = _make_board()
        bid = assert_belief(
            board,
            from_agent="claude/1",
            channel="general",
            claim="Error 1102 is caused by sequential requests",
            confidence=0.8,
        )
        assert bid.startswith("hive:")

    def test_belief_cell_stored_correctly(self):
        board = _make_board()
        bid = assert_belief(
            board,
            from_agent="claude/1",
            channel="general",
            claim="The cache is stale",
            confidence=0.9,
            evidence=["saw stale response 3x"],
        )
        cell = board.get(bid)
        assert cell is not None
        assert cell.type == "belief"
        assert cell.data["claim"] == "The cache is stale"
        assert cell.data["confidence"] == 0.9
        assert cell.data["status"] == "active"
        assert "saw stale response 3x" in cell.data["evidence"]


class TestRefuteAndConfirm:
    def test_refute_creates_refutation_cell(self):
        board = _make_board()
        bid = assert_belief(
            board, from_agent="claude/1", channel="general",
            claim="X causes Y", confidence=0.7
        )
        rid = refute_belief(
            board, belief_id=bid, from_agent="claude/1", channel="general",
            reason="X does not cause Y -- Z does",
            correction="Assert Z instead"
        )
        refutation = board.get(rid)
        assert refutation is not None
        assert refutation.type == "refutation"
        assert bid in refutation.refs

    def test_confirm_creates_confirmation_cell(self):
        board = _make_board()
        bid = assert_belief(
            board, from_agent="claude/1", channel="general",
            claim="Retry after 2s fixes it", confidence=0.6
        )
        cid = confirm_belief(
            board, belief_id=bid, from_agent="claude/1", channel="general",
            evidence="3 successful retries confirmed"
        )
        confirmation = board.get(cid)
        assert confirmation.type == "confirmation"
        assert bid in confirmation.refs


class TestGetActiveBeliefs:
    def test_active_excludes_refuted(self):
        board = _make_board()
        b1 = assert_belief(board, from_agent="claude/1", channel="general", claim="A is true")
        b2 = assert_belief(board, from_agent="claude/1", channel="general", claim="B is true")
        # Refute b1
        refute_belief(board, belief_id=b1, from_agent="claude/1", channel="general", reason="A is false")
        active = get_active_beliefs(board)
        active_ids = [b.id for b in active]
        assert b1 not in active_ids
        assert b2 in active_ids

    def test_active_excludes_confirmed(self):
        board = _make_board()
        b1 = assert_belief(board, from_agent="claude/1", channel="general", claim="C is true")
        confirm_belief(board, belief_id=b1, from_agent="claude/1", channel="general")
        active = get_active_beliefs(board)
        assert all(b.id != b1 for b in active)


class TestGetRefutedBeliefs:
    def test_returns_refuted_with_correction(self):
        board = _make_board()
        bid = assert_belief(
            board, from_agent="claude/1", channel="general",
            claim="Sequential requests cause 1102"
        )
        refute_belief(
            board, belief_id=bid, from_agent="claude/1", channel="general",
            reason="Load balancer reuse issue",
            correction="Use connection pooling"
        )
        refuted = get_refuted_beliefs(board)
        assert len(refuted) == 1
        assert refuted[0]["claim"] == "Sequential requests cause 1102"
        assert refuted[0]["correction"] == "Use connection pooling"


class TestBeliefAuditLimit:
    def test_audit_counts_beyond_200_beliefs(self):
        """Regression: belief_audit must scan all beliefs, not truncate at 200."""
        board = _make_board()
        for i in range(201):
            assert_belief(board, from_agent="claude/1", channel="general", claim=f"claim {i}")

        audit = belief_audit(board)
        # Without limit=None the audit caps at 200 and reports total=200, active=200.
        assert audit["total"] == 201
        assert audit["active"] == 201


class TestBeliefAudit:
    def test_audit_counts_all_categories(self):
        board = _make_board()
        # 1 active, 1 confirmed, 1 refuted
        assert_belief(board, from_agent="claude/1", channel="general", claim="X")
        b2 = assert_belief(board, from_agent="claude/1", channel="general", claim="Y")
        b3 = assert_belief(board, from_agent="claude/1", channel="general", claim="Z")
        confirm_belief(board, belief_id=b2, from_agent="claude/1", channel="general")
        refute_belief(board, belief_id=b3, from_agent="claude/1", channel="general", reason="wrong")

        audit = belief_audit(board)
        assert audit["total"] == 3
        assert audit["active"] == 1
        assert audit["confirmed"] == 1
        assert audit["refuted"] == 1
        assert audit["accuracy"] == 0.5  # 1 confirmed / (1 confirmed + 1 refuted)
