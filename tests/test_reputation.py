"""Tests for hive.coordination.reputation."""
import os
import tempfile

from hive.board import HiveBoard
from hive.coordination.reputation import reputation


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(db_path=os.path.join(tmpdir, "test.db"), channels_dir=os.path.join(tmpdir, "ch"))


class TestReputation:
    def test_no_feedback_returns_default(self):
        board = _make_board()
        score = reputation(board, "claude/1")
        assert score == 5.0  # neutral default

    def test_single_perfect_feedback(self):
        board = _make_board()
        task_id = board.put(type="task", from_agent="claude/1", channel="general", data={"title": "test"})
        contract_id = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}, refs=[task_id])
        result_id = board.put(type="result", from_agent="gemini/1", channel="general", data={"output": "done"}, refs=[contract_id])
        board.feedback(from_agent="claude/1", channel="general", result_id=result_id, contract_id=contract_id, score=10)
        score = reputation(board, "gemini/1")
        assert score == 10.0

    def test_recent_feedback_weighted_higher(self):
        board = _make_board()
        # Old feedback (score=2), new feedback (score=9)
        task_id = board.put(type="task", from_agent="claude/1", channel="general", data={})
        c1 = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}, refs=[task_id], ts="2026-01-01T00:00:00+00:00")
        r1 = board.put(type="result", from_agent="gemini/1", channel="general", data={"output": "v1"}, refs=[c1], ts="2026-01-01T00:01:00+00:00")
        board.put(type="feedback", from_agent="claude/1", channel="general", data={"score": 2}, refs=[r1, c1], ts="2026-01-01T00:02:00+00:00")

        c2 = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}, refs=[task_id], ts="2026-03-01T00:00:00+00:00")
        r2 = board.put(type="result", from_agent="gemini/1", channel="general", data={"output": "v2"}, refs=[c2], ts="2026-03-01T00:01:00+00:00")
        board.put(type="feedback", from_agent="claude/1", channel="general", data={"score": 9}, refs=[r2, c2], ts="2026-03-01T00:02:00+00:00")

        score = reputation(board, "gemini/1")
        assert score > 5.5  # Recent (9) weighted more than old (2)
