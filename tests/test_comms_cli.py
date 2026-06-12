"""Tests for comms.sh -- the fleet's primary CLI.

Drives the real script via `bash comms.sh <cmd> ...` with COMMS_CHANNELS and
COMMS_DIR pointed at temp dirs, then asserts on the JSONL cells it writes.
Skipped entirely where bash is unavailable.
"""
import json
import os
import shutil
import subprocess
import tempfile

import pytest

BASH = shutil.which("bash")
REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
COMMS = os.path.join(REPO, "comms.sh")

pytestmark = pytest.mark.skipif(BASH is None, reason="bash not available")


@pytest.fixture()
def env(tmp_path):
    channels = tmp_path / "channels"
    channels.mkdir()
    # comms hive imports the hive package from COMMS_DIR and writes hive.db
    # there; link the package into the temp dir so the repo stays clean.
    comms_dir = tmp_path / "comms"
    comms_dir.mkdir()
    try:
        os.symlink(os.path.join(REPO, "hive"), comms_dir / "hive")
    except OSError:
        shutil.copytree(os.path.join(REPO, "hive"), comms_dir / "hive")
    e = dict(os.environ)
    e["COMMS_CHANNELS"] = str(channels)
    e["COMMS_DIR"] = str(comms_dir)
    e["COMMS_AGENT"] = "test/agent"
    return e, channels


def run(env_pair, *args):
    e, _ = env_pair
    return subprocess.run(
        [BASH, COMMS, *args], env=e, capture_output=True, text=True, timeout=30
    )


def cells(env_pair, channel):
    _, channels = env_pair
    f = channels / f"{channel}.jsonl"
    if not f.exists():
        return []
    return [json.loads(line) for line in f.read_text().splitlines() if line.strip()]


class TestMessaging:
    def test_send_writes_status_cell(self, env):
        r = run(env, "send", "general", "hello from tests")
        assert r.returncode == 0, r.stderr
        cs = cells(env, "general")
        assert len(cs) == 1
        assert cs[0]["type"] == "status"
        assert cs[0]["msg"] == "hello from tests"
        assert cs[0]["from"] == "test/agent"

    def test_task_ref_writes_result_with_task_id(self, env):
        r = run(env, "task-ref", "general", "TASK-7 complete: all green", "task-7-uuid")
        assert r.returncode == 0, r.stderr
        cs = cells(env, "general")
        assert len(cs) == 1
        assert cs[0]["type"] == "result"
        assert cs[0]["data"]["task_id"] == "task-7-uuid"

    def test_task_ref_requires_all_args(self, env):
        r = run(env, "task-ref", "general", "missing the task id")
        assert r.returncode == 1
        assert "usage" in r.stdout


class TestInputHardening:
    def test_read_filter_with_quote_does_not_break(self, env):
        run(env, "send", "general", "a message")
        r = run(env, "read", "general", "--from", "o'brien'; import os")
        assert r.returncode == 0, r.stderr
        # Filter simply matches nothing -- no traceback, no injection.
        assert "Traceback" not in r.stderr

    def test_read_rejects_non_numeric_last(self, env):
        run(env, "send", "general", "a message")
        r = run(env, "read", "general", "--last", "ten")
        assert r.returncode == 1
        assert "must be a number" in r.stdout

    def test_log_rejects_non_numeric_count(self, env):
        r = run(env, "log", "20); import os")
        assert r.returncode == 1
        assert "must be a number" in r.stdout


class TestProtocolCommands:
    """Every command PROTOCOL.md lists under REAL COMMANDS ONLY must exist."""

    def test_belief_top_level_command(self, env):
        r = run(env, "belief", "general", "tests catch protocol drift", "0.9")
        assert r.returncode == 0, r.stderr
        assert r.stdout.strip().startswith("hive:")

    def test_trace_and_refute_reach_hive_bridge(self, env):
        r = run(env, "trace", "contract-1", "general", "success", '[{"attempt": 1}]')
        assert r.returncode == 0, r.stderr
        assert r.stdout.strip().startswith("hive:")

        bid = run(env, "belief", "general", "a wrong prior", "0.5").stdout.strip()
        r = run(env, "refute", bid, "evidence said otherwise", "corrected claim", "general")
        assert r.returncode == 0, r.stderr
        assert r.stdout.strip().startswith("hive:")

    def test_help_documents_new_commands(self, env):
        out = run(env, "help").stdout
        for cmd in ("task-ref", "comms trace", "comms belief", "comms refute"):
            assert cmd in out
