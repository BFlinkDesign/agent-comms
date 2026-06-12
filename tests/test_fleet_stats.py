"""Tests for hive.fleet_stats -- the comms perf/fire scanner."""
import json
import subprocess
import sys

from hive.fleet_stats import collect, fire_summary

FLEET_STATS = "hive/fleet_stats.py"


def _write_channel(dirpath, name, cells):
    f = dirpath / f"{name}.jsonl"
    f.write_text("\n".join(json.dumps(c) for c in cells) + "\n")


def _cell(from_agent, type_, channel, msg="m", ts="2026-03-01T00:00:00+00:00"):
    return {"from": from_agent, "type": type_, "channel": channel, "msg": msg, "ts": ts}


class TestCollect:
    def test_counts_by_type(self, tmp_path):
        _write_channel(tmp_path, "general", [
            _cell("gemini/1", "result", "general"),
            _cell("gemini/1", "result", "general", ts="2026-03-01T01:00:00+00:00"),
            _cell("gemini/1", "error", "general"),
            _cell("gemini/1", "task", "general"),
            _cell("gemini/1", "phone-home", "general"),
        ])
        s = collect(str(tmp_path))["gemini/1"]
        assert s["results"] == 2
        assert s["errors"] == 1
        assert s["tasks"] == 1
        assert s["phone_homes"] == 1

    def test_filter_is_substring_match(self, tmp_path):
        _write_channel(tmp_path, "general", [
            _cell("gemini/1", "result", "general"),
            _cell("claude/1", "result", "general"),
        ])
        stats = collect(str(tmp_path), "gemini")
        assert list(stats) == ["gemini/1"]

    def test_roster_excluded_from_channels(self, tmp_path):
        _write_channel(tmp_path, "general", [_cell("a/1", "result", "general")])
        _write_channel(tmp_path, "roster", [_cell("a/1", "clock-in", "roster")])
        s = collect(str(tmp_path))["a/1"]
        assert s["channels"] == {"general"}

    def test_first_and_last_seen(self, tmp_path):
        _write_channel(tmp_path, "general", [
            _cell("a/1", "result", "general", ts="2026-03-02T00:00:00+00:00"),
            _cell("a/1", "result", "general", ts="2026-03-01T00:00:00+00:00"),
            _cell("a/1", "result", "general", ts="2026-03-03T00:00:00+00:00"),
        ])
        s = collect(str(tmp_path))["a/1"]
        assert s["first_seen"].startswith("2026-03-01")
        assert s["last_seen"].startswith("2026-03-03")

    def test_malformed_lines_skipped(self, tmp_path):
        f = tmp_path / "general.jsonl"
        f.write_text('not json{\n' + json.dumps(_cell("a/1", "result", "general")) + "\n")
        assert collect(str(tmp_path))["a/1"]["results"] == 1


class TestFireSummary:
    def test_aggregates_matching_agents(self, tmp_path):
        _write_channel(tmp_path, "general", [
            _cell("gemini/signx", "result", "general"),
            _cell("gemini/warehouse", "result", "general"),
            _cell("gemini/warehouse", "error", "general"),
            _cell("claude/1", "result", "general"),
        ])
        s = fire_summary(str(tmp_path), "gemini")
        assert s["results"] == 2
        assert s["errors"] == 1
        assert s["success_rate"] == "67%"

    def test_no_activity_gives_na_rate(self, tmp_path):
        s = fire_summary(str(tmp_path), "ghost/agent")
        assert s == {"results": 0, "errors": 0, "tasks": 0, "success_rate": "n/a", "channels": []}


class TestCLI:
    def run(self, *args):
        return subprocess.run(
            [sys.executable, FLEET_STATS, *args],
            capture_output=True, text=True, timeout=30,
        )

    def test_perf_mode(self, tmp_path):
        _write_channel(tmp_path, "general", [_cell("gemini/1", "result", "general")])
        r = self.run("perf", str(tmp_path))
        assert r.returncode == 0, r.stderr
        assert "gemini/1" in r.stdout
        assert "Success rate: 100%" in r.stdout

    def test_fire_json_mode(self, tmp_path):
        _write_channel(tmp_path, "general", [_cell("gemini/1", "result", "general")])
        r = self.run("fire-json", str(tmp_path), "gemini/1")
        assert r.returncode == 0, r.stderr
        assert json.loads(r.stdout) == {
            "results": 1, "errors": 0, "tasks": 0,
            "success_rate": "100%", "channels": ["general"],
        }

    def test_unknown_mode_errors(self, tmp_path):
        r = self.run("bogus", str(tmp_path))
        assert r.returncode == 1
