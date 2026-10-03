"""Run the real shell/wrapper against local stand-in CLIs; no paid APIs or live bus."""
import json
import os
import shutil
import subprocess
from pathlib import Path

import pytest

from hive.runner_scan import scan

BASH = shutil.which("bash")
REPO = Path(__file__).resolve().parents[1]
pytestmark = pytest.mark.skipif(BASH is None, reason="bash not available")


@pytest.fixture
def runner_env(tmp_path):
    binaries = tmp_path / "bin"
    binaries.mkdir()
    channels = tmp_path / "channels"
    channels.mkdir()
    checkout = tmp_path / "checkout"
    checkout.mkdir()
    shutil.copytree(REPO / "hive", checkout / "hive")
    shutil.copy2(REPO / "codex-wrap.py", checkout / "codex-wrap.py")
    env = dict(os.environ)
    env.update({
        "COMMS_DIR": str(checkout), "CHANNELS_DIR": str(channels),
        "COMMS_AGENT": "test/worker", "AGENT_CMD": "gemini", "POLL_SECS": "0.01",
        "RUNNER_STATE_DIR": str(tmp_path / "state"), "RUNNER_ONCE": "1",
        "PATH": str(binaries) + os.pathsep + env["PATH"],
    })
    return env, binaries, channels


def cli(binaries, name, code, output="A concrete response from the local test CLI."):
    path = binaries / name
    path.write_text(f"#!/usr/bin/env bash\nprintf '%s\\n' {json.dumps(output)}\nexit {code}\n")
    path.chmod(0o755)


def invoke(runner_env, command="gemini"):
    env, _, _ = runner_env
    env["AGENT_CMD"] = command
    # Source only the production helpers: no dispatch loop, clock-in or home-directory writes.
    helpers = (REPO / "agent-runner.sh").read_text().split("# Main loop\n", 1)[0]
    return subprocess.run(
        [BASH, "-c", helpers + '\ninvoke_agent "Test a concrete local task"\n', "test", "general"],
        env=env, capture_output=True, text=True, timeout=10,
    )


@pytest.mark.parametrize("code", [7, 23, 124])
def test_gemini_failure_reaches_the_caller(runner_env, code):
    _, binaries, _ = runner_env
    cli(binaries, "gemini", code)
    assert invoke(runner_env).returncode == code


def test_unknown_cli_is_not_success(runner_env):
    assert invoke(runner_env, "not-a-supported-cli").returncode != 0


def test_claude_human_handoff_is_not_execution_success(runner_env):
    assert invoke(runner_env, "claude").returncode != 0


@pytest.mark.parametrize("code", [0, 23, 124])
def test_codex_wrapper_preserves_child_exit(runner_env, code):
    env, binaries, _ = runner_env
    cli(binaries, "codex", code)
    result = subprocess.run(
        ["python", str(REPO / "codex-wrap.py")], input="Exercise wrapper status propagation",
        env=env, capture_output=True, text=True, timeout=10,
    )
    assert result.returncode == code


@pytest.mark.parametrize("terminal", ["error", "cancel"])
def test_failed_or_canceled_dependency_cannot_dispatch(terminal):
    cells = [
        {"id": "parent", "type": "task", "msg": "Prerequisite task", "data": {}},
        {"id": "child", "type": "task", "msg": "Downstream task", "data": {"depends_on": ["parent"]}},
        {"id": "end", "type": terminal, "data": {"task_id": "parent"}},
    ]
    assert list(scan(map(json.dumps, cells))) == []


def test_later_result_cannot_undo_a_terminal_failure():
    cells = [
        {"id": "parent", "type": "task", "msg": "Prerequisite task", "data": {}},
        {"id": "child", "type": "task", "msg": "Downstream task", "data": {"depends_on": ["parent"]}},
        {"id": "error", "type": "error", "data": {"task_id": "parent"}},
        {"id": "late", "type": "result", "data": {"task_id": "parent"}},
    ]
    assert list(scan(map(json.dumps, cells))) == []


def run_once(runner_env, command):
    env, _, channels = runner_env
    env["AGENT_CMD"] = command
    bus = channels / "general.jsonl"
    bus.write_text(json.dumps({
        "id": "task-1", "type": "task", "msg": "Exercise one isolated local runner invocation", "data": {},
    }) + "\n")
    result = subprocess.run(
        [BASH, str(REPO / "agent-runner.sh"), "general"],
        env=env, capture_output=True, text=True, timeout=10,
    )
    assert result.returncode == 0, result.stdout + result.stderr
    cells = [json.loads(line) for line in bus.read_text().splitlines()]
    return cells, result


@pytest.mark.parametrize("command,code,expected", [
    ("gemini", 0, "result"), ("gemini", 23, "error"), ("gemini", 124, "error"),
    ("codex", 0, "result"), ("codex", 23, "error"), ("claude", 0, "blocked"),
    ("unsupported", 0, "error"),
])
def test_dispatch_posts_actual_outcome(runner_env, command, code, expected):
    _, binaries, _ = runner_env
    cli(binaries, command, code)
    cells, result = run_once(runner_env, command)
    outcomes = [c for c in cells if c["type"] in {"result", "error", "blocked"}]
    assert len(outcomes) == 1
    assert outcomes[0]["type"] == expected
    assert outcomes[0]["data"]["task_id"] == "task-1"
    if command in {"gemini", "codex"}:
        assert outcomes[0]["data"]["exit_code"] == code
    if expected != "result":
        assert "complete — result posted" not in result.stdout


def test_short_output_is_blocked_instead_of_padded_into_success(runner_env):
    _, binaries, _ = runner_env
    cli(binaries, "gemini", 0, "ok")
    cells, _ = run_once(runner_env, "gemini")
    assert not any(c["type"] == "result" for c in cells)
    assert any(c["type"] == "blocked" for c in cells)


def test_restarting_does_not_repeat_a_recorded_outcome(runner_env):
    env, binaries, channels = runner_env
    cli(binaries, "gemini", 23)
    cells, _ = run_once(runner_env, "gemini")
    again = subprocess.run(
        [BASH, str(REPO / "agent-runner.sh"), "general"],
        env=env, capture_output=True, text=True, timeout=10,
    )
    assert again.returncode == 0
    assert [json.loads(x) for x in (channels / "general.jsonl").read_text().splitlines()] == cells
