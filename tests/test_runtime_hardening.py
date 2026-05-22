"""Runtime hardening tests for config, paths, and channel validation."""
import asyncio
import importlib
import os
import subprocess
import sys
import tempfile
from pathlib import Path

import pytest
from fastapi.responses import JSONResponse
from starlette.requests import Request

from hive.board import HiveBoard
from hive.config import (
    ConfigError,
    channel_file_path,
    default_channels_dir,
    default_db_path,
    validate_channel_name,
)
from hive.cell import make_cell
from hive.transports.jsonl import JSONLTransport


@pytest.fixture
def tmp_dir():
    with tempfile.TemporaryDirectory() as directory:
        yield Path(directory)


def test_default_channels_dir_prefers_hive_env(monkeypatch):
    monkeypatch.setenv("HIVE_CHANNELS_DIR", "/tmp/hive-channels")
    monkeypatch.setenv("COMMS_CHANNELS", "/tmp/legacy-channels")

    assert default_channels_dir() == "/tmp/hive-channels"


def test_default_channels_dir_falls_back_to_comms_env(monkeypatch):
    monkeypatch.delenv("HIVE_CHANNELS_DIR", raising=False)
    monkeypatch.setenv("COMMS_CHANNELS", "/tmp/legacy-channels")

    assert default_channels_dir() == "/tmp/legacy-channels"


def test_default_db_path_uses_hive_env(monkeypatch):
    monkeypatch.setenv("HIVE_DB_PATH", "/tmp/hive.db")

    assert default_db_path() == "/tmp/hive.db"


@pytest.mark.parametrize("channel", ["general", "signx-intel", "agent_1", "a"])
def test_validate_channel_name_accepts_safe_names(channel):
    assert validate_channel_name(channel) == channel


@pytest.mark.parametrize(
    "channel",
    ["", "../general", "foo/bar", "/tmp/general", "Upper", "bad.name", "bad name", "-bad"],
)
def test_validate_channel_name_rejects_unsafe_names(channel):
    with pytest.raises(ConfigError):
        validate_channel_name(channel)


def test_channel_file_path_stays_under_channels_dir(tmp_dir):
    path = channel_file_path(str(tmp_dir), "general")

    assert path == os.path.join(str(tmp_dir), "general.jsonl")


def test_channel_file_path_rejects_traversal(tmp_dir):
    with pytest.raises(ConfigError):
        channel_file_path(str(tmp_dir), "../escape")


def test_channel_file_path_rejects_existing_symlink(tmp_dir):
    target = tmp_dir / "outside.jsonl"
    target.write_text("", encoding="utf-8")
    link = tmp_dir / "general.jsonl"
    link.symlink_to(target)

    with pytest.raises(ConfigError):
        channel_file_path(str(tmp_dir), "general")


def test_jsonl_transport_rejects_unsafe_channel_before_write(tmp_dir):
    transport = JSONLTransport(str(tmp_dir))
    cell = make_cell(type="msg", from_agent="agent-a", channel="../escape", data={})

    with pytest.raises(ConfigError):
        transport.put(cell)

    assert not (tmp_dir.parent / "escape.jsonl").exists()


def test_hive_board_rejects_unsafe_channel_before_sqlite_write(tmp_dir):
    board = HiveBoard(db_path=str(tmp_dir / "hive.db"), channels_dir=str(tmp_dir / "channels"))

    with pytest.raises(ConfigError):
        board.put(type="task", from_agent="claude/1", channel="../escape", data={"title": "bad"})

    assert board.query() == []
    assert not (tmp_dir / "escape.jsonl").exists()


def _import_dashboard_with_env(monkeypatch, tmp_dir, **env):
    previous = sys.modules.pop("dashboard.server", None)
    monkeypatch.setenv("HIVE_CHANNELS_DIR", str(tmp_dir / "channels"))
    monkeypatch.setenv("HIVE_DB_PATH", str(tmp_dir / "hive.db"))
    for key, value in env.items():
        if value is None:
            monkeypatch.delenv(key, raising=False)
        else:
            monkeypatch.setenv(key, value)
    try:
        return importlib.import_module("dashboard.server")
    finally:
        sys.modules.pop("dashboard.server", None)
        if previous is not None:
            sys.modules["dashboard.server"] = previous


def test_dashboard_uses_env_config_and_safe_cors(monkeypatch, tmp_dir):
    monkeypatch.delenv("HIVE_DASHBOARD_ALLOW_ORIGINS", raising=False)

    server = _import_dashboard_with_env(monkeypatch, tmp_dir)

    assert str(server.CHANNELS_DIR) == str(tmp_dir / "channels")
    assert str(server.DB_PATH) == str(tmp_dir / "hive.db")
    cors = next(m for m in server.app.user_middleware if m.cls.__name__ == "CORSMiddleware")
    assert cors.kwargs["allow_origins"] == ["http://127.0.0.1:7842", "http://localhost:7842"]
    assert cors.kwargs["allow_credentials"] is False


def test_dashboard_rejects_invalid_port(monkeypatch, tmp_dir):
    with pytest.raises(ConfigError):
        _import_dashboard_with_env(monkeypatch, tmp_dir, HIVE_DASHBOARD_PORT="not-a-port")


def test_dashboard_token_is_enforced_when_configured(monkeypatch, tmp_dir):
    server = _import_dashboard_with_env(monkeypatch, tmp_dir, HIVE_DASHBOARD_TOKEN="secret")

    async def ok_response(_request):
        return JSONResponse({"ok": True})

    unauthenticated = Request({"type": "http", "method": "GET", "path": "/", "headers": []})
    authenticated = Request({
        "type": "http",
        "method": "GET",
        "path": "/",
        "headers": [(b"authorization", b"Bearer secret")],
    })

    assert asyncio.run(server._require_dashboard_token(unauthenticated, ok_response)).status_code == 401
    assert asyncio.run(server._require_dashboard_token(authenticated, ok_response)).status_code == 200


def test_dashboard_rejects_non_loopback_client_without_token(monkeypatch, tmp_dir):
    server = _import_dashboard_with_env(monkeypatch, tmp_dir)

    async def ok_response(_request):
        return JSONResponse({"ok": True})

    remote_request = Request({
        "type": "http",
        "method": "GET",
        "path": "/",
        "headers": [],
        "client": ("10.0.0.1", 12345),
    })

    response = asyncio.run(server._require_dashboard_token(remote_request, ok_response))

    assert response.status_code == 401


def test_comms_rejects_unsafe_channel(tmp_dir):
    command = (
        f'COMMS_AGENT="test/agent" HIVE_CHANNELS_DIR="{tmp_dir}" '
        'PATH="/tmp/agent-comms-venv/bin:$PATH" '
        "bash -lc 'source ./comms.sh; comms send ../escape \"this message should be rejected\"'"
    )

    result = subprocess.run(command, cwd=os.getcwd(), shell=True, text=True, capture_output=True)

    assert result.returncode != 0
    assert "invalid channel" in result.stderr.lower()
    assert not (tmp_dir.parent / "escape.jsonl").exists()


def test_comms_rejects_symlinked_channel_file(tmp_dir):
    target = tmp_dir / "outside.jsonl"
    target.write_text("", encoding="utf-8")
    (tmp_dir / "general.jsonl").symlink_to(target)
    command = (
        f'COMMS_AGENT="test/agent" HIVE_CHANNELS_DIR="{tmp_dir}" '
        'PATH="/tmp/agent-comms-venv/bin:$PATH" '
        "bash -lc 'source ./comms.sh; comms send general \"this should reject symlink\"'"
    )

    result = subprocess.run(command, cwd=os.getcwd(), shell=True, text=True, capture_output=True)

    assert result.returncode != 0
    assert target.read_text(encoding="utf-8") == ""


def test_agent_runner_rejects_symlinked_channel_file(tmp_dir):
    target = tmp_dir / "outside.jsonl"
    target.write_text("", encoding="utf-8")
    (tmp_dir / "general.jsonl").symlink_to(target)
    command = [
        "bash",
        "./agent-runner.sh",
        "general",
    ]
    env = {
        **os.environ,
        "HIVE_CHANNELS_DIR": str(tmp_dir),
        "COMMS_AGENT": "test/runner",
        "AGENT_CMD": "claude",
        "POLL_SECS": "1",
    }

    result = subprocess.run(command, cwd=os.getcwd(), env=env, text=True, capture_output=True, timeout=5)

    assert result.returncode != 0
    assert "symlinked channel file" in result.stderr.lower()
    assert target.read_text(encoding="utf-8") == ""


def test_comms_rejects_invalid_json_without_creating_channel(tmp_dir):
    command = (
        f'COMMS_AGENT="test/agent" HIVE_CHANNELS_DIR="{tmp_dir}" '
        'PATH="/tmp/agent-comms-venv/bin:$PATH" '
        "bash -lc 'source ./comms.sh; comms send general "
        "\"this message has invalid json data\" --data \"{bad\"'"
    )

    result = subprocess.run(command, cwd=os.getcwd(), shell=True, text=True, capture_output=True)

    assert result.returncode != 0
    assert "failed to send" in result.stderr.lower()
    assert not (tmp_dir / "general.jsonl").exists()
