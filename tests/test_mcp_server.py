"""Integration tests for hive.mcp.server -- JSON-RPC dispatch layer."""
import json
import os
import tempfile

from hive.board import HiveBoard
from hive.mcp.server import handle_message
from hive.mcp.tools import get_tool_definitions


def _make_board() -> HiveBoard:
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(
        db_path=os.path.join(tmpdir, "test.db"),
        channels_dir=os.path.join(tmpdir, "ch"),
    )


def _tools() -> list[dict]:
    return get_tool_definitions()


def _req(method: str, params: dict | None = None, *, req_id: int = 1) -> dict:
    msg: dict = {"jsonrpc": "2.0", "id": req_id, "method": method}
    if params is not None:
        msg["params"] = params
    return msg


def _notify(method: str, params: dict | None = None) -> dict:
    """Build a JSON-RPC notification (no id)."""
    msg: dict = {"jsonrpc": "2.0", "method": method}
    if params is not None:
        msg["params"] = params
    return msg


class TestInitializeHandshake:
    def test_initialize_returns_protocol_version(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("initialize", {}))
        assert resp is not None
        assert resp["result"]["protocolVersion"] == "2024-11-05"

    def test_initialize_reports_server_info(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("initialize", {}))
        assert resp is not None
        info = resp["result"]["serverInfo"]
        assert info["name"] == "hive"
        assert "version" in info

    def test_initialize_advertises_tools_capability(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("initialize", {}))
        assert resp is not None
        assert "tools" in resp["result"]["capabilities"]

    def test_initialized_notification_returns_none(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _notify("notifications/initialized"))
        assert resp is None

    def test_ping_returns_empty_result(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("ping", {}))
        assert resp is not None
        assert resp["result"] == {}


class TestToolsList:
    def test_tools_list_returns_all_tools(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("tools/list"))
        assert resp is not None
        tools = resp["result"]["tools"]
        names = {t["name"] for t in tools}
        assert "hive_put" in names
        assert "hive_get" in names
        assert "hive_task" in names
        assert "hive_task_state" in names
        assert "hive_ready_tasks" in names
        assert "hive_result" in names

    def test_each_tool_has_name_description_schema(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("tools/list"))
        assert resp is not None
        for tool in resp["result"]["tools"]:
            assert "name" in tool
            assert "description" in tool
            assert "inputSchema" in tool


class TestToolsCall:
    def test_hive_put_via_jsonrpc(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("tools/call", {
            "name": "hive_put",
            "arguments": {
                "type": "task",
                "from_agent": "claude/test",
                "channel": "general",
                "data": {"title": "mcp server test"},
            },
        }))
        assert resp is not None
        assert "isError" not in resp["result"] or not resp["result"].get("isError")
        content = json.loads(resp["result"]["content"][0]["text"])
        assert "id" in content
        assert content["id"].startswith("hive:")

    def test_hive_task_state_via_jsonrpc(self) -> None:
        board = _make_board()
        tid = board.task(from_agent="claude/test", channel="general", title="state test")
        resp = handle_message(board, _tools(), _req("tools/call", {
            "name": "hive_task_state",
            "arguments": {"task_id": tid},
        }))
        assert resp is not None
        content = json.loads(resp["result"]["content"][0]["text"])
        assert content["status"] == "SUBMITTED"
        assert content["task_id"] == tid

    def test_hive_ready_tasks_via_jsonrpc(self) -> None:
        board = _make_board()
        board.task(from_agent="claude/test", channel="general", title="ready check")
        resp = handle_message(board, _tools(), _req("tools/call", {
            "name": "hive_ready_tasks",
            "arguments": {},
        }))
        assert resp is not None
        content = json.loads(resp["result"]["content"][0]["text"])
        assert len(content["tasks"]) == 1
        assert content["tasks"][0]["title"] == "ready check"

    def test_unknown_tool_returns_error_content(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("tools/call", {
            "name": "hive_does_not_exist",
            "arguments": {},
        }))
        assert resp is not None
        content = json.loads(resp["result"]["content"][0]["text"])
        assert "error" in content

    def test_tool_exception_returns_is_error(self) -> None:
        board = _make_board()
        # hive_get with a missing required arg will cause a KeyError
        resp = handle_message(board, _tools(), _req("tools/call", {
            "name": "hive_get",
            "arguments": {},  # missing required "id"
        }))
        assert resp is not None
        assert resp["result"].get("isError") is True


class TestUnknownMethods:
    def test_unknown_method_with_id_returns_error_code(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("resources/list", req_id=42))
        assert resp is not None
        assert resp["id"] == 42
        assert resp["error"]["code"] == -32601
        assert "resources/list" in resp["error"]["message"]

    def test_unknown_notification_ignored(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _notify("some/notification"))
        assert resp is None

    def test_response_id_matches_request(self) -> None:
        board = _make_board()
        resp = handle_message(board, _tools(), _req("ping", req_id=99))
        assert resp is not None
        assert resp["id"] == 99
