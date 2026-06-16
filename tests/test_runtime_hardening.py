"""Runtime hardening tests: config defaults, channel validation, path safety.

Covers the Python-level channel-safety guard wired into HiveBoard and the
JSONL transport. The shell- and dashboard-level enforcement that PR #2 also
proposed is intentionally out of scope here (the comms.sh / dashboard write
paths already diverged on main and need separate, focused integration).
"""
import os
import tempfile
from pathlib import Path

import pytest

from hive.board import HiveBoard
from hive.cell import make_cell
from hive.config import (
    ConfigError,
    channel_file_path,
    default_channels_dir,
    default_db_path,
    validate_channel_name,
)
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


def test_default_db_path_prefers_hive_db_path(monkeypatch):
    monkeypatch.setenv("HIVE_DB_PATH", "/tmp/hive.db")
    assert default_db_path() == "/tmp/hive.db"


def test_default_db_path_falls_back_to_hive_db_alias(monkeypatch):
    monkeypatch.delenv("HIVE_DB_PATH", raising=False)
    monkeypatch.setenv("HIVE_DB", "/tmp/legacy.db")
    assert default_db_path() == "/tmp/legacy.db"


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
    cell = make_cell(type="status", from_agent="agent-a", channel="../escape", data={})
    with pytest.raises(ConfigError):
        transport.put(cell)
    assert not (tmp_dir.parent / "escape.jsonl").exists()


def test_jsonl_transport_does_not_follow_symlink(tmp_dir):
    target = tmp_dir / "outside.jsonl"
    target.write_text("", encoding="utf-8")
    (tmp_dir / "general.jsonl").symlink_to(target)
    transport = JSONLTransport(str(tmp_dir))
    cell = make_cell(type="status", from_agent="agent-a", channel="general", data={})
    with pytest.raises(ConfigError):
        transport.put(cell)
    # The symlink target must remain untouched.
    assert target.read_text(encoding="utf-8") == ""


def test_hive_board_rejects_unsafe_channel_before_sqlite_write(tmp_dir):
    board = HiveBoard(db_path=str(tmp_dir / "hive.db"), channels_dir=str(tmp_dir / "channels"))
    with pytest.raises(ConfigError):
        board.put(type="task", from_agent="claude/1", channel="../escape", data={"title": "bad"})
    # Channel was rejected before either write: no SQLite row, no escaped file.
    assert board.query() == []
    assert not (tmp_dir / "escape.jsonl").exists()
