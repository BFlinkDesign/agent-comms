"""Tests for hive.mcp.tools -- MCP tool definitions."""
import os
import tempfile

from hive.board import HiveBoard
from hive.mcp.tools import execute_tool, get_tool_definitions


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(db_path=os.path.join(tmpdir, "test.db"), channels_dir=os.path.join(tmpdir, "ch"))


class TestToolDefinitions:
    def test_tool_definitions_exist(self):
        tools = get_tool_definitions()
        assert len(tools) > 0
        names = [t["name"] for t in tools]
        assert "hive_put" in names
        assert "hive_get" in names
        assert "hive_query" in names
        assert "hive_task" in names

    def test_lifecycle_tools_present(self):
        names = [t["name"] for t in get_tool_definitions()]
        assert "hive_task_state" in names
        assert "hive_ready_tasks" in names
        assert "hive_result" in names

    def test_each_tool_has_description(self):
        for tool in get_tool_definitions():
            assert "description" in tool
            assert len(tool["description"]) > 10


class TestToolExecution:
    def test_hive_put_via_execute(self):
        board = _make_board()
        result = execute_tool(board, "hive_put", {
            "type": "task",
            "from_agent": "claude/1",
            "channel": "general",
            "data": {"title": "test"},
        })
        assert "id" in result
        assert result["id"].startswith("hive:")

    def test_hive_get_via_execute(self):
        board = _make_board()
        put_result = execute_tool(board, "hive_put", {
            "type": "task",
            "from_agent": "claude/1",
            "channel": "general",
            "data": {"title": "test"},
        })
        get_result = execute_tool(board, "hive_get", {"id": put_result["id"]})
        assert get_result["cell"]["type"] == "task"

    def test_hive_query_via_execute(self):
        board = _make_board()
        execute_tool(board, "hive_put", {
            "type": "task", "from_agent": "claude/1", "channel": "general", "data": {},
        })
        result = execute_tool(board, "hive_query", {"type": "task"})
        assert len(result["cells"]) == 1

    def test_hive_task_convenience(self):
        board = _make_board()
        result = execute_tool(board, "hive_task", {
            "from_agent": "claude/1", "channel": "general", "title": "Do thing",
        })
        assert "id" in result


class TestTaskStateToolViaMCP:
    def test_submitted_task_state(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        result = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert result["status"] == "SUBMITTED"
        assert result["task_id"] == tid
        assert result["claimed_by"] is None
        assert result["unmet_dependencies"] == []

    def test_working_state_after_contract(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        result = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert result["status"] == "WORKING"
        assert result["claimed_by"] == "gemini/1"

    def test_complete_state_after_result(self):
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
        result = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert result["status"] == "COMPLETE"
        assert result["contract_id"] == cid
        assert result["result_id"] is not None

    def test_unmet_deps_reported(self):
        board = _make_board()
        t1 = board.task(from_agent="claude/1", channel="general", title="step 1")
        t2 = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "step 2"},
            refs=[t1],
        )
        result = execute_tool(board, "hive_task_state", {"task_id": t2})
        assert result["status"] == "SUBMITTED"
        assert t1 in result["unmet_dependencies"]

    def test_unknown_task_returns_error(self):
        board = _make_board()
        result = execute_tool(board, "hive_task_state", {"task_id": "hive:notexist"})
        assert "error" in result


class TestReadyTasksToolViaMCP:
    def test_returns_submitted_tasks(self):
        board = _make_board()
        board.task(from_agent="claude/1", channel="general", title="Ready One")
        result = execute_tool(board, "hive_ready_tasks", {})
        assert len(result["tasks"]) == 1
        assert result["tasks"][0]["title"] == "Ready One"

    def test_excludes_claimed_tasks(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        result = execute_tool(board, "hive_ready_tasks", {})
        assert all(t["id"] != tid for t in result["tasks"])

    def test_excludes_tasks_with_unmet_deps(self):
        board = _make_board()
        t1 = board.task(from_agent="claude/1", channel="general", title="step 1")
        t2 = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "step 2"},
            refs=[t1],
        )
        result = execute_tool(board, "hive_ready_tasks", {"channel": "general"})
        ids = [t["id"] for t in result["tasks"]]
        assert t1 in ids
        assert t2 not in ids

    def test_channel_filter(self):
        board = _make_board()
        board.task(from_agent="claude/1", channel="general", title="In general")
        board.task(from_agent="claude/1", channel="other", title="In other")
        result = execute_tool(board, "hive_ready_tasks", {"channel": "general"})
        assert len(result["tasks"]) == 1
        assert result["tasks"][0]["title"] == "In general"

    def test_empty_board_returns_empty_list(self):
        board = _make_board()
        result = execute_tool(board, "hive_ready_tasks", {})
        assert result["tasks"] == []


class TestResultToolViaMCP:
    def test_result_marks_task_complete(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        cid = board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        res = execute_tool(board, "hive_result", {
            "from_agent": "gemini/1",
            "channel": "general",
            "contract_id": cid,
            "output": "done",
        })
        assert "id" in res
        state = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert state["status"] == "COMPLETE"
        assert state["result_id"] == res["id"]

    def test_result_with_artifacts(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        cid = board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        res = execute_tool(board, "hive_result", {
            "from_agent": "gemini/1",
            "channel": "general",
            "contract_id": cid,
            "output": "done",
            "artifacts": ["/path/to/file.txt"],
            "metrics": {"tokens": 500},
        })
        assert "id" in res
