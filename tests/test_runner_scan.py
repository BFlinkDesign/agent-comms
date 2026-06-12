"""Tests for hive.runner_scan -- the agent-runner.sh channel scanner."""
import json

from hive.runner_scan import scan


def _cell(type_, id_, msg="", **data):
    return json.dumps({"id": id_, "type": type_, "msg": msg, "data": data})


def _scan(lines):
    return list(scan(lines))


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
