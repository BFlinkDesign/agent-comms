"""Tests for hive.runner_scan -- the agent-runner.sh channel scanner."""
import json
import os
import tempfile

from hive.runner_scan import scan, scan_board


def _cell(type_, id_, msg="", **data):
    return json.dumps({"id": id_, "type": type_, "msg": msg, "data": data})


def _scan(lines):
    return list(scan(lines))


def _make_board():
    from hive.board import HiveBoard
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(
        db_path=os.path.join(tmpdir, "test.db"),
        channels_dir=os.path.join(tmpdir, "channels"),
    )


class TestScanBasics:
    def test_open_task_emitted(self):
        out = _scan([_cell("task", "t1", "do the thing")])
        assert out == [("t1", "do the thing")]

    def test_claimed_task_excluded(self):
        out = _scan([
            _cell("task", "t1", "do the thing"),
            _cell("claim", "c1", task_id="t1"),
        ])
        assert out == []

    def test_completed_task_excluded(self):
        out = _scan([
            _cell("task", "t1", "do the thing"),
            _cell("result", "r1", task_id="t1"),
        ])
        assert out == []

    def test_errored_task_excluded(self):
        out = _scan([
            _cell("task", "t1", "do the thing"),
            _cell("error", "e1", task_id="t1"),
        ])
        assert out == []

    def test_malformed_lines_skipped(self):
        out = _scan(["not json{", "", _cell("task", "t1", "valid task")])
        assert out == [("t1", "valid task")]


class TestDependencyEnforcement:
    def test_task_with_unmet_dependency_not_emitted(self):
        out = _scan([
            _cell("task", "t1", "TASK-2: root-cause"),
            _cell("task", "t2", "TASK-3: calibrate", depends_on=["t1"]),
        ])
        assert out == [("t1", "TASK-2: root-cause")]

    def test_task_emitted_once_dependency_completes(self):
        out = _scan([
            _cell("task", "t1", "TASK-2: root-cause"),
            _cell("task", "t2", "TASK-3: calibrate", depends_on=["t1"]),
            _cell("result", "r1", task_id="t1"),
        ])
        assert out == [("t2", "TASK-3: calibrate")]

    def test_unknown_dependency_id_blocks(self):
        # FLEET-OPS incident: Codex claimed TASK-3 before TASK-2 was posted.
        out = _scan([
            _cell("task", "t2", "TASK-3: calibrate", depends_on=["not-posted-yet"]),
        ])
        assert out == []

    def test_multiple_deps_all_must_complete(self):
        lines = [
            _cell("task", "t1", "TASK-1"),
            _cell("task", "t2", "TASK-2"),
            _cell("task", "t3", "TASK-3", depends_on=["t1", "t2"]),
            _cell("result", "r1", task_id="t1"),
        ]
        assert ("t3", "TASK-3") not in _scan(lines)
        lines.append(_cell("result", "r2", task_id="t2"))
        out = _scan(lines)
        assert ("t3", "TASK-3") in out

    def test_non_list_depends_on_ignored(self):
        out = _scan([_cell("task", "t1", "weird deps", depends_on="t0")])
        assert out == [("t1", "weird deps")]


class TestScanBoardMode:
    """scan_board() uses HiveBoard + lifecycle.is_task_ready — canonical reducer."""

    def test_submitted_task_is_ready(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        ids = [r[0] for r in results]
        assert tid in ids

    def test_contract_claimed_task_not_ready(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        assert all(r[0] != tid for r in results)

    def test_legacy_claim_excludes_task(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        board.put(type="claim", from_agent="gemini/1", channel="general", data={"task_id": tid})
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        assert all(r[0] != tid for r in results)

    def test_completed_via_contract_result_not_ready(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        cid = board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        board.result(from_agent="gemini/1", channel="general", contract_id=cid, output="done")
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        assert all(r[0] != tid for r in results)

    def test_dep_via_refs_blocks_task(self):
        """Task with dependency expressed via refs (not data.depends_on) is blocked."""
        board = _make_board()
        t1 = board.task(from_agent="claude/1", channel="general", title="step 1")
        t2_id = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "step 2"},
            refs=[t1],
        )
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        ids = [r[0] for r in results]
        assert t1 in ids
        assert t2_id not in ids

    def test_dep_via_refs_unblocked_when_complete(self):
        board = _make_board()
        t1 = board.task(from_agent="claude/1", channel="general", title="step 1")
        c1 = board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[t1],
        )
        board.result(from_agent="gemini/1", channel="general", contract_id=c1, output="done")
        t2_id = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "step 2"},
            refs=[t1],
        )
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        ids = [r[0] for r in results]
        assert t2_id in ids
        assert t1 not in ids  # t1 is COMPLETE

    def test_channel_filter_respected(self):
        board = _make_board()
        board.task(from_agent="claude/1", channel="general", title="A")
        board.task(from_agent="claude/1", channel="other", title="B")
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        assert len(results) == 1
        assert results[0][1] == "A"

    def test_task_title_exposed_as_msg(self):
        board = _make_board()
        board.task(from_agent="claude/1", channel="general", title="My Task Title")
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        assert results[0][1] == "My Task Title"

    def test_failed_task_not_emitted(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        board.put(type="error", from_agent="claude/1", channel="general", data={"task_id": tid})
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        assert all(r[0] != tid for r in results)

    def test_canceled_task_not_emitted(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        board.put(type="cancel", from_agent="claude/1", channel="general", data={"task_id": tid})
        results = list(scan_board(board._sqlite._db_path, board._jsonl._dir, "general"))
        assert all(r[0] != tid for r in results)
