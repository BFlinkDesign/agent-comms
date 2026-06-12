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
