"""Tests for hive.coordination.router."""
import os
import tempfile

from hive.board import HiveBoard
from hive.coordination.router import route_task


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(db_path=os.path.join(tmpdir, "test.db"), channels_dir=os.path.join(tmpdir, "ch"))


class TestRouter:
    def test_no_agents_returns_empty(self):
        board = _make_board()
        task_id = board.task(from_agent="claude/1", channel="general", title="test")
        task = board.get(task_id)
        result = route_task(board, task)
        assert result == []

    def test_routes_to_capable_agent(self):
        board = _make_board()
        board.card(from_agent="gemini/1", capabilities=["scanning", "data-analysis"])
        board.card(from_agent="claude/1", capabilities=["architecture", "protocol-work"])
        task_id = board.put(type="task", from_agent="claude/1", channel="general", data={"title": "scan codebase", "required_capabilities": ["scanning"]})
        task = board.get(task_id)
        rankings = route_task(board, task)
        assert len(rankings) > 0
        assert rankings[0][0] == "gemini/1"

    def test_reputation_affects_ranking(self):
        board = _make_board()
        board.card(from_agent="agent/a", capabilities=["coding"])
        board.card(from_agent="agent/b", capabilities=["coding"])
        # Give agent/a good feedback
        task_id = board.put(type="task", from_agent="claude/1", channel="general", data={})
        c = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "agent/a"}, refs=[task_id])
        r = board.put(type="result", from_agent="agent/a", channel="general", data={"output": "done"}, refs=[c])
        board.feedback(from_agent="claude/1", channel="general", result_id=r, contract_id=c, score=10)
        # Route a coding task
        task_id2 = board.put(type="task", from_agent="claude/1", channel="general", data={"title": "code it", "required_capabilities": ["coding"]})
        task2 = board.get(task_id2)
        rankings = route_task(board, task2)
        assert rankings[0][0] == "agent/a"
