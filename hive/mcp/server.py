"""MCP server for HIVE -- exposes Board operations as MCP tools.

Run: python -m hive.mcp.server [--db hive.db] [--channels channels/]

Register in Claude Code settings.json:
{
  "mcpServers": {
    "hive": {
      "command": "python",
      "args": ["-m", "hive.mcp.server", "--db", "C:/tools/agent-comms/hive.db", "--channels", "C:/tools/agent-comms/channels"]
    }
  }
}
"""
import json
import sys
from typing import Any

from hive.board import HiveBoard
from hive.config import default_channels_dir, default_db_path
from hive.mcp.tools import get_tool_definitions, execute_tool


def _read_message() -> dict | None:
    """Read a JSON-RPC message from stdin (LSP-style Content-Length headers)."""
    headers: dict[str, str] = {}
    while True:
        line = sys.stdin.buffer.readline()
        if not line:
            return None
        line_str = line.decode("utf-8").strip()
        if not line_str:
            break  # empty line = end of headers
        if ":" in line_str:
            key, value = line_str.split(":", 1)
            headers[key.strip().lower()] = value.strip()

    content_length = int(headers.get("content-length", "0"))
    if content_length == 0:
        return None

    body = sys.stdin.buffer.read(content_length)
    return json.loads(body.decode("utf-8"))


def _write_message(msg: dict):
    """Write a JSON-RPC message to stdout."""
    body = json.dumps(msg).encode("utf-8")
    header = f"Content-Length: {len(body)}\r\n\r\n".encode("utf-8")
    sys.stdout.buffer.write(header + body)
    sys.stdout.buffer.flush()


def _make_response(request_id: Any, result: Any) -> dict:
    return {"jsonrpc": "2.0", "id": request_id, "result": result}


def _make_error(request_id: Any, code: int, message: str) -> dict:
    return {"jsonrpc": "2.0", "id": request_id, "error": {"code": code, "message": message}}


def run_server(db_path: str | None = None, channels_dir: str | None = None):
    """Run the HIVE MCP server (stdio transport)."""
    board = HiveBoard(
        db_path=db_path or default_db_path(),
        channels_dir=channels_dir or default_channels_dir(),
    )
    tools = get_tool_definitions()

    while True:
        msg = _read_message()
        if msg is None:
            break

        request_id = msg.get("id")
        method = msg.get("method", "")
        params = msg.get("params", {})

        if method == "initialize":
            _write_message(_make_response(request_id, {
                "protocolVersion": "2024-11-05",
                "capabilities": {"tools": {}},
                "serverInfo": {"name": "hive", "version": "1.1.0"},
            }))

        elif method == "notifications/initialized":
            pass  # no response needed

        elif method == "tools/list":
            tool_list = [
                {
                    "name": t["name"],
                    "description": t["description"],
                    "inputSchema": t["inputSchema"],
                }
                for t in tools
            ]
            _write_message(_make_response(request_id, {"tools": tool_list}))

        elif method == "tools/call":
            tool_name = params.get("name", "")
            tool_args = params.get("arguments", {})
            try:
                result = execute_tool(board, tool_name, tool_args)
                _write_message(_make_response(request_id, {
                    "content": [{"type": "text", "text": json.dumps(result, indent=2)}],
                }))
            except Exception as e:
                _write_message(_make_response(request_id, {
                    "content": [{"type": "text", "text": json.dumps({"error": str(e)})}],
                    "isError": True,
                }))

        else:
            if request_id is not None:
                _write_message(_make_error(request_id, -32601, f"Method not found: {method}"))


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser(description="HIVE MCP Server")
    parser.add_argument("--db", default=None)
    parser.add_argument("--channels", default=None)
    args = parser.parse_args()
    run_server(db_path=args.db, channels_dir=args.channels)
