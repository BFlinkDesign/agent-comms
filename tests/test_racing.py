"""Tests for hive.coordination.racing."""
import os
import tempfile

from hive.board import HiveBoard
from hive.coordination.racing import get_race_results, start_race


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(db_path=os.path.join(tmpdir, "test.db"), channels_dir=os.path.join(tmpdir, "ch"))


class TestRacing:
    def test_start_race_creates_contracts(self):
        board = _make_board()
        task_id = board.put(type="task", from_agent="claude/1", channel="general", data={"title": "race", "race": True})
        agents = ["gemini/1", "codex/1"]
        contract_ids = start_race(board, task_id=task_id, agents=agents)
        assert len(contract_ids) == 2

    def test_race_results_collects_all(self):
        board = _make_board()
        task_id = board.put(type="task", from_agent="claude/1", channel="general", data={"title": "race", "race": True})
        contracts = start_race(board, task_id=task_id, agents=["gemini/1", "codex/1"])
        # Both agents submit results
        board.put(type="result", from_agent="gemini/1", channel="general", data={"output": "gemini answer"}, refs=[contracts[0]])
        board.put(type="result", from_agent="codex/1", channel="general", data={"output": "codex answer"}, refs=[contracts[1]])
        results = get_race_results(board, task_id=task_id)
        assert len(results) == 2
