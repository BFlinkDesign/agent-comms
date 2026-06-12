"""Full lifecycle integration test -- exercises every HIVE layer.

Scenario: Claude posts a task, Gemini bids via card, gets contracted,
sends heartbeats, delivers a result, receives feedback. Then verify
reputation, DAG, stall detection, racing, leases, evolution, and MCP tools.
"""
import os
import tempfile

from hive.board import HiveBoard
from hive.coordination.dag import get_ready_tasks
from hive.coordination.leases import acquire_lease, is_leased, release_lease
from hive.coordination.racing import get_race_results, start_race
from hive.coordination.reputation import reputation
from hive.coordination.router import route_task
from hive.coordination.stall_detector import detect_stalls
from hive.mcp.tools import execute_tool, get_tool_definitions


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(
        db_path=os.path.join(tmpdir, "test.db"),
        channels_dir=os.path.join(tmpdir, "channels"),
    ), tmpdir


class TestFullLifecycle:
    def test_task_to_feedback_pipeline(self):
        """Complete workflow: task -> card -> route -> contract -> heartbeat -> result -> feedback."""
        board, tmpdir = _make_board()

        # 1. Gemini publishes a capability card
        card_id = board.card(
            from_agent="gemini/1",
            capabilities=["scanning", "data-analysis"],
        )
        assert card_id.startswith("hive:")

        # 2. Claude posts a task
        task_id = board.task(
            from_agent="claude/1",
            channel="general",
            title="Scan codebase for dead imports",
            spec="Find all unused imports in src/",
        )

        # 3. Router ranks agents
        task = board.get(task_id)
        rankings = route_task(board, task)
        assert len(rankings) >= 1
        best_agent = rankings[0][0]
        assert best_agent == "gemini/1"

        # 4. Claude creates a contract
        contract_id = board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": best_agent, "task_title": "Scan codebase"},
            refs=[task_id],
        )

        # 5. Gemini sends heartbeats
        hb_id = board.heartbeat(
            from_agent="gemini/1",
            contract_id=contract_id,
            progress=50,
            status="scanning",
        )
        assert hb_id.startswith("hive:")

        # 6. Gemini delivers result
        result_id = board.result(
            from_agent="gemini/1",
            channel="general",
            contract_id=contract_id,
            output="Found 12 dead imports across 5 files",
            artifacts=["dead_imports.txt"],
        )

        # 7. Claude provides feedback
        board.feedback(
            from_agent="claude/1",
            channel="general",
            result_id=result_id,
            contract_id=contract_id,
            score=9,
            notes="Thorough scan, good output format",
        )

        # 8. Verify reputation updated
        rep = reputation(board, "gemini/1")
        assert rep == 9.0  # single feedback, score is exact

        # 9. Verify JSONL files written
        assert os.path.exists(os.path.join(tmpdir, "channels", "general.jsonl"))
        assert os.path.exists(os.path.join(tmpdir, "channels", "roster.jsonl"))

        # 10. Verify refs DAG traversal
        contract_refs = board.refs(contract_id)
        ref_types = {c.type for c in contract_refs}
        assert "heartbeat" in ref_types
        assert "result" in ref_types

    def test_dag_resolution_multi_step(self):
        """Multi-step task DAG: A -> B -> C, only A is ready initially."""
        board, _ = _make_board()

        a_id = board.task(from_agent="claude/1", channel="general", title="Step A")
        b_id = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "Step B"},
            refs=[a_id],
        )
        board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "Step C"},
            refs=[b_id],
        )

        # Only A is ready
        ready = get_ready_tasks(board, channel="general")
        assert len(ready) == 1
        assert ready[0].data["title"] == "Step A"

        # Complete A
        c_a = board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[a_id],
        )
        board.put(
            type="result",
            from_agent="gemini/1",
            channel="general",
            data={"output": "A done"},
            refs=[c_a],
        )

        # Now B is ready (A has result), C is not (B has no result)
        ready = get_ready_tasks(board, channel="general")
        titles = [t.data["title"] for t in ready]
        assert "Step B" in titles
        assert "Step C" not in titles

    def test_lease_lifecycle(self):
        """Acquire -> verify -> release -> verify free."""
        board, _ = _make_board()

        lid = acquire_lease(board, resource="src/app.py", holder="claude/1")
        assert lid is not None
        assert is_leased(board, resource="src/app.py") is True
        assert acquire_lease(board, resource="src/app.py", holder="gemini/1") is None

        release_lease(board, lease_id=lid, holder="claude/1")
        assert is_leased(board, resource="src/app.py") is False

        # Now gemini can acquire
        lid2 = acquire_lease(board, resource="src/app.py", holder="gemini/1")
        assert lid2 is not None

    def test_racing_end_to_end(self):
        """Two agents race, both submit results."""
        board, _ = _make_board()

        task_id = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "Race task", "race": True},
        )
        contracts = start_race(
            board, task_id=task_id, agents=["gemini/1", "codex/1"]
        )
        assert len(contracts) == 2

        board.put(
            type="result",
            from_agent="gemini/1",
            channel="general",
            data={"output": "gemini wins"},
            refs=[contracts[0]],
        )
        board.put(
            type="result",
            from_agent="codex/1",
            channel="general",
            data={"output": "codex wins"},
            refs=[contracts[1]],
        )

        results = get_race_results(board, task_id=task_id)
        assert len(results) == 2
        outputs = {r.data["output"] for r in results}
        assert "gemini wins" in outputs
        assert "codex wins" in outputs

    def test_stall_detection_with_old_contract(self):
        """Old contract without heartbeat or result triggers stall."""
        board, _ = _make_board()

        task_id = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={},
            ts="2020-01-01T00:00:00+00:00",
        )
        board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[task_id],
            ts="2020-01-01T00:00:01+00:00",
        )

        stalls = detect_stalls(board, timeout_seconds=60)
        assert len(stalls) >= 1
        assert stalls[0]["agent"] == "gemini/1"

    def test_mcp_tools_roundtrip(self):
        """MCP tool layer: put -> get -> query."""
        board, _ = _make_board()

        tools = get_tool_definitions()
        assert len(tools) >= 9

        put_result = execute_tool(
            board,
            "hive_task",
            {"from_agent": "claude/1", "channel": "general", "title": "MCP test"},
        )
        assert put_result["id"].startswith("hive:")

        get_result = execute_tool(board, "hive_get", {"id": put_result["id"]})
        assert get_result["cell"]["type"] == "task"
        assert get_result["cell"]["data"]["title"] == "MCP test"

        query_result = execute_tool(board, "hive_query", {"type": "task"})
        assert len(query_result["cells"]) >= 1

    def test_expire_cleans_ttl_cells(self):
        """Cells with TTL in the past get expired."""
        board, _ = _make_board()

        board.put(
            type="heartbeat",
            from_agent="gemini/1",
            channel="roster",
            data={"status": "alive"},
            ttl=1,
            ts="2020-01-01T00:00:00+00:00",
        )
        board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "permanent"},
            ttl=0,
        )

        removed = board.expire()
        assert removed >= 1

        # Permanent task still exists
        tasks = board.query(type="task")
        assert len(tasks) == 1

    def test_watch_fires_on_put(self):
        """Watch callback fires synchronously on put."""
        board, _ = _make_board()
        received = []
        board.watch("general", lambda cell: received.append(cell.type))
        board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "watched"},
        )
        assert "task" in received
