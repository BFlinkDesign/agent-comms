"""Tests for hive.coordination.dag -- task dependency resolution."""
import os
import tempfile

from hive.board import HiveBoard
from hive.coordination.dag import get_ready_tasks, get_task_deps


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(db_path=os.path.join(tmpdir, "test.db"), channels_dir=os.path.join(tmpdir, "ch"))


class TestDAG:
    def test_task_with_no_deps_is_ready(self):
        board = _make_board()
        board.task(from_agent="claude/1", channel="general", title="standalone")
        ready = get_ready_tasks(board, channel="general")
        assert len(ready) == 1

    def test_task_with_unfinished_dep_not_ready(self):
        board = _make_board()
        t1 = board.task(from_agent="claude/1", channel="general", title="step 1")
        board.put(type="task", from_agent="claude/1", channel="general", data={"title": "step 2"}, refs=[t1])
        ready = get_ready_tasks(board, channel="general")
        # Only step 1 should be ready (step 2 depends on step 1 result)
        assert len(ready) == 1
        assert ready[0].data["title"] == "step 1"

    def test_task_with_finished_dep_is_ready(self):
        board = _make_board()
        t1 = board.task(from_agent="claude/1", channel="general", title="step 1")
        # Complete step 1
        c1 = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}, refs=[t1])
        board.put(type="result", from_agent="gemini/1", channel="general", data={"output": "done"}, refs=[c1])
        # Step 2 depends on step 1
        board.put(type="task", from_agent="claude/1", channel="general", data={"title": "step 2"}, refs=[t1])
        ready = get_ready_tasks(board, channel="general")
        titles = [t.data["title"] for t in ready]
        assert "step 2" in titles

    def test_get_task_deps(self):
        board = _make_board()
        t1 = board.task(from_agent="claude/1", channel="general", title="A")
        t2_id = board.put(type="task", from_agent="claude/1", channel="general", data={"title": "B"}, refs=[t1])
        t2 = board.get(t2_id)
        deps = get_task_deps(board, t2)
        assert len(deps) == 1
        assert deps[0].data["title"] == "A"
