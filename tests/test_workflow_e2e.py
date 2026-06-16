"""End-to-end workflow tests: full task lifecycle exercised via MCP tools.

These tests prove the complete coordination chain — task creation, bidding,
contract claim, heartbeat, result, feedback — works correctly from the MCP
interface that agents actually use in production.
"""
import os
import tempfile

from hive.board import HiveBoard
from hive.mcp.tools import execute_tool


def _make_board() -> HiveBoard:
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(
        db_path=os.path.join(tmpdir, "test.db"),
        channels_dir=os.path.join(tmpdir, "ch"),
    )


class TestFullTaskLifecycle:
    """Full lifecycle: SUBMITTED → WORKING → COMPLETE via MCP tools."""

    def test_happy_path_task_to_complete(self) -> None:
        board = _make_board()

        # 1. Coordinator creates a task
        task_res = execute_tool(board, "hive_task", {
            "from_agent": "claude/coord",
            "channel": "general",
            "title": "Analyse dataset",
            "spec": "Run correlation analysis on Q1 data",
            "bounty": 8,
        })
        tid = task_res["id"]
        assert tid.startswith("hive:")

        # 2. Task should appear as ready
        ready = execute_tool(board, "hive_ready_tasks", {"channel": "general"})
        assert any(t["id"] == tid for t in ready["tasks"])

        # 3. Worker checks task state before claiming
        state = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert state["status"] == "SUBMITTED"
        assert state["claimed_by"] is None

        # 4. Worker places a bid
        bid_res = execute_tool(board, "hive_bid", {
            "from_agent": "gemini/worker",
            "channel": "general",
            "task_id": tid,
            "cost": 5,
        })
        assert bid_res["id"].startswith("hive:")

        # 5. Coordinator awards the contract
        contract_res = execute_tool(board, "hive_contract", {
            "from_agent": "claude/coord",
            "channel": "general",
            "task_id": tid,
            "agent": "gemini/worker",
        })
        cid = contract_res["id"]
        assert cid.startswith("hive:")

        # 6. Task is now WORKING
        state = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert state["status"] == "WORKING"
        assert state["claimed_by"] == "gemini/worker"
        assert state["contract_id"] == cid

        # 7. Task no longer appears in ready list
        ready = execute_tool(board, "hive_ready_tasks", {"channel": "general"})
        assert all(t["id"] != tid for t in ready["tasks"])

        # 8. Worker sends a heartbeat
        hb_res = execute_tool(board, "hive_heartbeat", {
            "from_agent": "gemini/worker",
            "contract_id": cid,
            "progress": 50,
        })
        assert hb_res["id"].startswith("hive:")

        # 9. Worker posts the result
        result_res = execute_tool(board, "hive_result", {
            "from_agent": "gemini/worker",
            "channel": "general",
            "contract_id": cid,
            "output": "Correlation coefficient: 0.87",
            "metrics": {"tokens": 1200},
        })
        rid = result_res["id"]
        assert rid.startswith("hive:")

        # 10. Task is COMPLETE
        state = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert state["status"] == "COMPLETE"
        assert state["result_id"] == rid
        assert state["contract_id"] == cid

    def test_multi_task_dag_sequential(self) -> None:
        """Step 2 is only claimable after step 1 completes (DAG dependency via refs)."""
        board = _make_board()

        t1 = execute_tool(board, "hive_task", {
            "from_agent": "claude/coord", "channel": "general", "title": "Step 1: fetch data",
        })["id"]

        # Step 2 depends on step 1 via refs
        t2 = execute_tool(board, "hive_put", {
            "type": "task",
            "from_agent": "claude/coord",
            "channel": "general",
            "data": {"title": "Step 2: analyse data"},
            "refs": [t1],
        })["id"]

        # Only step 1 is ready initially
        ready = execute_tool(board, "hive_ready_tasks", {"channel": "general"})
        ids = [t["id"] for t in ready["tasks"]]
        assert t1 in ids
        assert t2 not in ids

        # Complete step 1
        c1 = execute_tool(board, "hive_contract", {
            "from_agent": "claude/coord", "channel": "general", "task_id": t1, "agent": "gemini/1",
        })["id"]
        execute_tool(board, "hive_result", {
            "from_agent": "gemini/1", "channel": "general", "contract_id": c1, "output": "fetched",
        })

        # Now step 2 is ready
        ready = execute_tool(board, "hive_ready_tasks", {"channel": "general"})
        ids = [t["id"] for t in ready["tasks"]]
        assert t2 in ids
        assert t1 not in ids  # t1 is COMPLETE


class TestLeaseWorkflow:
    """Lease-protected critical section via MCP tools."""

    def test_lease_protects_resource_across_agents(self) -> None:
        board = _make_board()

        # Agent 1 acquires lease
        l1 = execute_tool(board, "hive_lease", {
            "from_agent": "claude/1", "resource": "prod-db",
        })
        assert l1["id"] is not None

        # Resource is leased
        assert execute_tool(board, "hive_is_leased", {"resource": "prod-db"})["leased"] is True

        # Agent 2 cannot acquire
        l2 = execute_tool(board, "hive_lease", {
            "from_agent": "gemini/2", "resource": "prod-db",
        })
        assert l2["id"] is None

        # Agent 1 releases
        execute_tool(board, "hive_release", {
            "from_agent": "claude/1", "lease_id": l1["id"],
        })

        # Now agent 2 can acquire
        assert execute_tool(board, "hive_is_leased", {"resource": "prod-db"})["leased"] is False
        l3 = execute_tool(board, "hive_lease", {
            "from_agent": "gemini/2", "resource": "prod-db",
        })
        assert l3["id"] is not None


class TestFeedbackWorkflow:
    """Task lifecycle including feedback scoring."""

    def test_feedback_after_result(self) -> None:
        board = _make_board()

        tid = execute_tool(board, "hive_task", {
            "from_agent": "claude/1", "channel": "general", "title": "T",
        })["id"]
        cid = execute_tool(board, "hive_contract", {
            "from_agent": "claude/1", "channel": "general", "task_id": tid, "agent": "gemini/1",
        })["id"]
        rid = execute_tool(board, "hive_result", {
            "from_agent": "gemini/1", "channel": "general", "contract_id": cid, "output": "done",
        })["id"]

        fb = execute_tool(board, "hive_feedback", {
            "from_agent": "claude/1",
            "channel": "general",
            "result_id": rid,
            "contract_id": cid,
            "score": 9,
            "notes": "excellent",
        })
        assert fb["id"].startswith("hive:")

        cell = board.get(fb["id"])
        assert cell is not None
        assert cell.data["score"] == 9
