"""Tests for hive.shell_write — validated JSONL cell writer for shell scripts."""
import json
import sys
from pathlib import Path

import pytest

# Import the module under test directly (it lives in hive/, not a package member,
# so we add the hive directory to sys.path temporarily).
sys.path.insert(0, str(Path(__file__).parent.parent / "hive"))
import shell_write  # type: ignore[import-not-found]


@pytest.fixture()
def tmp_channels(tmp_path: Path) -> Path:
    d = tmp_path / "channels"
    d.mkdir()
    return d


def _run(channels_dir: Path, channel: str, type_: str = "status", msg: str = "hello", data: str = "{}") -> int:
    argv = ["shell_write.py", "agent/1", channel, type_, msg, data, str(channels_dir)]
    return shell_write.main(argv)


class TestValidWrite:
    def test_creates_jsonl_file(self, tmp_channels: Path) -> None:
        rc = _run(tmp_channels, "general")
        assert rc == 0
        assert (tmp_channels / "general.jsonl").exists()

    def test_cell_is_valid_json(self, tmp_channels: Path) -> None:
        _run(tmp_channels, "general", msg="test msg")
        lines = (tmp_channels / "general.jsonl").read_text().splitlines()
        assert len(lines) == 1
        cell = json.loads(lines[0])
        assert cell["from"] == "agent/1"
        assert cell["channel"] == "general"
        assert cell["msg"] == "test msg"

    def test_returns_cell_id_on_stdout(self, tmp_channels: Path, capsys: pytest.CaptureFixture[str]) -> None:
        rc = _run(tmp_channels, "general")
        out = capsys.readouterr().out.strip()
        assert rc == 0
        assert len(out) == 36  # UUID4 length

    def test_appends_multiple_cells(self, tmp_channels: Path) -> None:
        _run(tmp_channels, "general", msg="first")
        _run(tmp_channels, "general", msg="second")
        lines = (tmp_channels / "general.jsonl").read_text().splitlines()
        assert len(lines) == 2

    def test_hyphens_and_underscores_in_channel_name(self, tmp_channels: Path) -> None:
        assert _run(tmp_channels, "signx-intel") == 0
        assert _run(tmp_channels, "agent_1") == 0

    def test_creates_channels_dir_if_missing(self, tmp_path: Path) -> None:
        missing = tmp_path / "new-channels"
        rc = _run(missing, "general")
        assert rc == 0
        assert (missing / "general.jsonl").exists()


class TestChannelValidation:
    def test_rejects_empty_channel(self, tmp_channels: Path) -> None:
        assert _run(tmp_channels, "") != 0

    def test_rejects_uppercase_channel(self, tmp_channels: Path) -> None:
        assert _run(tmp_channels, "General") != 0

    def test_rejects_dotted_channel(self, tmp_channels: Path) -> None:
        assert _run(tmp_channels, "bad.name") != 0

    def test_rejects_space_in_channel(self, tmp_channels: Path) -> None:
        assert _run(tmp_channels, "bad name") != 0

    def test_rejects_leading_hyphen(self, tmp_channels: Path) -> None:
        assert _run(tmp_channels, "-bad") != 0

    def test_rejects_slash_traversal(self, tmp_channels: Path) -> None:
        rc = _run(tmp_channels, "../escape")
        assert rc != 0
        assert not (tmp_channels.parent / "escape.jsonl").exists()


class TestSymlinkGuard:
    def test_rejects_existing_symlink(self, tmp_channels: Path, tmp_path: Path) -> None:
        target = tmp_path / "outside.jsonl"
        target.write_text("", encoding="utf-8")
        (tmp_channels / "general.jsonl").symlink_to(target)
        rc = _run(tmp_channels, "general")
        assert rc != 0
        assert target.read_text() == ""

    def test_allows_regular_file(self, tmp_channels: Path) -> None:
        (tmp_channels / "general.jsonl").write_text("", encoding="utf-8")
        assert _run(tmp_channels, "general") == 0


class TestWrongArgCount:
    def test_too_few_args_returns_2(self) -> None:
        rc = shell_write.main(["shell_write.py", "agent/1"])
        assert rc == 2
