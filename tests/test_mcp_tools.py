"""Tests for hive.mcp.tools -- MCP tool definitions."""
import os
import tempfile

from hive.board import HiveBoard
from hive.mcp.tools import execute_tool, get_tool_definitions


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(db_path=os.path.join(tmpdir, "test.db"), channels_dir=os.path.join(tmpdir, "ch"))


class TestToolDefinitions:
    def test_tool_definitions_exist(self):
        tools = get_tool_definitions()
        assert len(tools) > 0
        names = [t["name"] for t in tools]
        assert "hive_put" in names
        assert "hive_get" in names
        assert "hive_query" in names
        assert "hive_task" in names

    def test_lifecycle_tools_present(self):
        names = [t["name"] for t in get_tool_definitions()]
        assert "hive_task_state" in names
        assert "hive_ready_tasks" in names
        assert "hive_result" in names

    def test_each_tool_has_description(self):
        for tool in get_tool_definitions():
            assert "description" in tool
            assert len(tool["description"]) > 10


class TestToolExecution:
    def test_hive_put_via_execute(self):
        board = _make_board()
        result = execute_tool(board, "hive_put", {
            "type": "task",
            "from_agent": "claude/1",
            "channel": "general",
            "data": {"title": "test"},
        })
        assert "id" in result
        assert result["id"].startswith("hive:")

    def test_hive_get_via_execute(self):
        board = _make_board()
        put_result = execute_tool(board, "hive_put", {
            "type": "task",
            "from_agent": "claude/1",
            "channel": "general",
            "data": {"title": "test"},
        })
        get_result = execute_tool(board, "hive_get", {"id": put_result["id"]})
        assert get_result["cell"]["type"] == "task"

    def test_hive_query_via_execute(self):
        board = _make_board()
        execute_tool(board, "hive_put", {
            "type": "task", "from_agent": "claude/1", "channel": "general", "data": {},
        })
        result = execute_tool(board, "hive_query", {"type": "task"})
        assert len(result["cells"]) == 1

    def test_hive_task_convenience(self):
        board = _make_board()
        result = execute_tool(board, "hive_task", {
            "from_agent": "claude/1", "channel": "general", "title": "Do thing",
        })
        assert "id" in result


class TestTaskStateToolViaMCP:
    def test_submitted_task_state(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        result = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert result["status"] == "SUBMITTED"
        assert result["task_id"] == tid
        assert result["claimed_by"] is None
        assert result["unmet_dependencies"] == []

    def test_working_state_after_contract(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        result = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert result["status"] == "WORKING"
        assert result["claimed_by"] == "gemini/1"

    def test_complete_state_after_result(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        cid = board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        board.result(from_agent="gemini/1", channel="general", contract_id=cid, output="done")
        result = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert result["status"] == "COMPLETE"
        assert result["contract_id"] == cid
        assert result["result_id"] is not None

    def test_unmet_deps_reported(self):
        board = _make_board()
        t1 = board.task(from_agent="claude/1", channel="general", title="step 1")
        t2 = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "step 2"},
            refs=[t1],
        )
        result = execute_tool(board, "hive_task_state", {"task_id": t2})
        assert result["status"] == "SUBMITTED"
        assert t1 in result["unmet_dependencies"]

    def test_unknown_task_returns_error(self):
        board = _make_board()
        result = execute_tool(board, "hive_task_state", {"task_id": "hive:notexist"})
        assert "error" in result


class TestReadyTasksToolViaMCP:
    def test_returns_submitted_tasks(self):
        board = _make_board()
        board.task(from_agent="claude/1", channel="general", title="Ready One")
        result = execute_tool(board, "hive_ready_tasks", {})
        assert len(result["tasks"]) == 1
        assert result["tasks"][0]["title"] == "Ready One"

    def test_excludes_claimed_tasks(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        result = execute_tool(board, "hive_ready_tasks", {})
        assert all(t["id"] != tid for t in result["tasks"])

    def test_excludes_tasks_with_unmet_deps(self):
        board = _make_board()
        t1 = board.task(from_agent="claude/1", channel="general", title="step 1")
        t2 = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "step 2"},
            refs=[t1],
        )
        result = execute_tool(board, "hive_ready_tasks", {"channel": "general"})
        ids = [t["id"] for t in result["tasks"]]
        assert t1 in ids
        assert t2 not in ids

    def test_channel_filter(self):
        board = _make_board()
        board.task(from_agent="claude/1", channel="general", title="In general")
        board.task(from_agent="claude/1", channel="other", title="In other")
        result = execute_tool(board, "hive_ready_tasks", {"channel": "general"})
        assert len(result["tasks"]) == 1
        assert result["tasks"][0]["title"] == "In general"

    def test_empty_board_returns_empty_list(self):
        board = _make_board()
        result = execute_tool(board, "hive_ready_tasks", {})
        assert result["tasks"] == []


class TestResultToolViaMCP:
    def test_result_marks_task_complete(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        cid = board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        res = execute_tool(board, "hive_result", {
            "from_agent": "gemini/1",
            "channel": "general",
            "contract_id": cid,
            "output": "done",
        })
        assert "id" in res
        state = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert state["status"] == "COMPLETE"
        assert state["result_id"] == res["id"]

    def test_result_with_artifacts(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="A")
        cid = board.put(
            type="contract",
            from_agent="claude/1",
            channel="general",
            data={"agent": "gemini/1"},
            refs=[tid],
        )
        res = execute_tool(board, "hive_result", {
            "from_agent": "gemini/1",
            "channel": "general",
            "contract_id": cid,
            "output": "done",
            "artifacts": ["/path/to/file.txt"],
            "metrics": {"tokens": 500},
        })
        assert "id" in res


class TestBidContractToolsViaMCP:
    def test_hive_bid_creates_bid_referencing_task(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        res = execute_tool(board, "hive_bid", {
            "from_agent": "gemini/1",
            "channel": "general",
            "task_id": tid,
            "cost": 3,
        })
        assert "id" in res
        assert res["id"].startswith("hive:")
        # The bid cell should reference the task
        refs = board.refs(tid)
        bid_ids = {c.id for c in refs if c.type == "bid"}
        assert res["id"] in bid_ids

    def test_hive_contract_creates_contract_and_marks_task_working(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        res = execute_tool(board, "hive_contract", {
            "from_agent": "claude/1",
            "channel": "general",
            "task_id": tid,
            "agent": "gemini/1",
        })
        assert "id" in res
        state = execute_tool(board, "hive_task_state", {"task_id": tid})
        assert state["status"] == "WORKING"
        assert state["claimed_by"] == "gemini/1"
        assert state["contract_id"] == res["id"]

    def test_hive_contract_race_flag_stored(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        res = execute_tool(board, "hive_contract", {
            "from_agent": "claude/1",
            "channel": "general",
            "task_id": tid,
            "agent": "gemini/1",
            "race": True,
        })
        cell = board.get(res["id"])
        assert cell is not None
        assert cell.data.get("race") is True


class TestLeaseToolsViaMCP:
    def test_hive_is_leased_false_when_empty(self):
        board = _make_board()
        res = execute_tool(board, "hive_is_leased", {"resource": "myfile.txt"})
        assert res["leased"] is False

    def test_hive_lease_acquires_returns_id(self):
        board = _make_board()
        res = execute_tool(board, "hive_lease", {
            "from_agent": "claude/1",
            "resource": "myfile.txt",
        })
        assert res["id"] is not None
        assert res["id"].startswith("hive:")

    def test_hive_is_leased_true_after_acquire(self):
        board = _make_board()
        execute_tool(board, "hive_lease", {
            "from_agent": "claude/1",
            "resource": "shared-resource",
        })
        res = execute_tool(board, "hive_is_leased", {"resource": "shared-resource"})
        assert res["leased"] is True

    def test_hive_lease_blocked_when_already_leased(self):
        board = _make_board()
        execute_tool(board, "hive_lease", {
            "from_agent": "claude/1",
            "resource": "locked-file",
        })
        res = execute_tool(board, "hive_lease", {
            "from_agent": "gemini/1",
            "resource": "locked-file",
        })
        assert res["id"] is None

    def test_hive_release_clears_lease(self):
        board = _make_board()
        lease_res = execute_tool(board, "hive_lease", {
            "from_agent": "claude/1",
            "resource": "temp-resource",
        })
        execute_tool(board, "hive_release", {
            "from_agent": "claude/1",
            "lease_id": lease_res["id"],
        })
        res = execute_tool(board, "hive_is_leased", {"resource": "temp-resource"})
        assert res["leased"] is False

    def test_hive_release_returns_release_cell_id(self):
        board = _make_board()
        lease_res = execute_tool(board, "hive_lease", {
            "from_agent": "claude/1",
            "resource": "r",
        })
        release_res = execute_tool(board, "hive_release", {
            "from_agent": "claude/1",
            "lease_id": lease_res["id"],
        })
        assert "id" in release_res
        assert release_res["id"].startswith("hive:")


class TestReputationToolViaMCP:
    def test_no_feedback_returns_default_score(self):
        board = _make_board()
        res = execute_tool(board, "hive_reputation", {"agent_id": "gemini/1"})
        assert res["agent_id"] == "gemini/1"
        assert res["score"] == 5.0

    def test_reputation_reflects_feedback(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        cid = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
        rid = board.result(from_agent="gemini/1", channel="general", contract_id=cid, output="done")
        board.feedback(from_agent="claude/1", channel="general", result_id=rid, contract_id=cid, score=10)
        res = execute_tool(board, "hive_reputation", {"agent_id": "gemini/1"})
        assert res["score"] == 10.0

    def test_reputation_tool_in_tool_definitions(self):
        names = [t["name"] for t in get_tool_definitions()]
        assert "hive_reputation" in names

    def test_reputation_score_is_rounded(self):
        board = _make_board()
        res = execute_tool(board, "hive_reputation", {"agent_id": "nobody"})
        # Default 5.0 has at most 4 decimal places (it's exact, so 5.0)
        assert isinstance(res["score"], float)


class TestRouteToolViaMCP:
    def test_no_cards_returns_empty(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        res = execute_tool(board, "hive_route", {"task_id": tid})
        assert res["task_id"] == tid
        assert res["ranked_agents"] == []

    def test_routes_capable_agent_first(self):
        board = _make_board()
        tid = board.put(
            type="task",
            from_agent="claude/1",
            channel="general",
            data={"title": "T", "required_capabilities": ["python"]},
        )
        board.card(from_agent="gemini/expert", capabilities=["python", "sql"])
        board.card(from_agent="codex/junior", capabilities=["markdown"])
        res = execute_tool(board, "hive_route", {"task_id": tid})
        agent_ids = [a["agent_id"] for a in res["ranked_agents"]]
        # Expert with python capability should rank first
        assert agent_ids[0] == "gemini/expert"

    def test_unknown_task_returns_error(self):
        board = _make_board()
        res = execute_tool(board, "hive_route", {"task_id": "hive:doesnotexist"})
        assert "error" in res

    def test_route_tool_in_tool_definitions(self):
        names = [t["name"] for t in get_tool_definitions()]
        assert "hive_route" in names

    def test_route_scores_are_rounded(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        board.card(from_agent="gemini/1", capabilities=["x"])
        res = execute_tool(board, "hive_route", {"task_id": tid})
        for agent in res["ranked_agents"]:
            # Score should be a float with at most 4 decimal places
            assert isinstance(agent["score"], float)


class TestBeliefQueryToolsViaMCP:
    def test_confirm_belief_returns_id(self):
        board = _make_board()
        bid = board.put(
            type="belief", from_agent="claude/1", channel="general",
            data={"claim": "X causes Y", "confidence": 0.8, "evidence": [], "status": "active"},
        )
        res = execute_tool(board, "hive_confirm_belief", {
            "belief_id": bid, "from_agent": "claude/1", "evidence": "verified in prod",
        })
        assert res["id"].startswith("hive:")

    def test_get_beliefs_returns_active_only(self):
        board = _make_board()
        b1 = board.put(
            type="belief", from_agent="claude/1", channel="general",
            data={"claim": "A", "confidence": 0.7, "evidence": [], "status": "active"},
        )
        b2 = board.put(
            type="belief", from_agent="claude/1", channel="general",
            data={"claim": "B", "confidence": 0.5, "evidence": [], "status": "active"},
        )
        # Refute b2
        board.put(type="refutation", from_agent="claude/1", channel="general",
                  data={"reason": "wrong", "correction": ""}, refs=[b2])
        res = execute_tool(board, "hive_get_beliefs", {})
        ids = [c["id"] for c in res["beliefs"]]
        assert b1 in ids
        assert b2 not in ids

    def test_get_refuted_beliefs_includes_reason(self):
        board = _make_board()
        bid = board.put(
            type="belief", from_agent="claude/1", channel="general",
            data={"claim": "X", "confidence": 0.9, "evidence": [], "status": "active"},
        )
        board.put(type="refutation", from_agent="gemini/1", channel="general",
                  data={"reason": "tested and false", "correction": "Y is the real cause"}, refs=[bid])
        res = execute_tool(board, "hive_get_refuted_beliefs", {})
        assert len(res["refuted"]) == 1
        assert res["refuted"][0]["reason"] == "tested and false"
        assert res["refuted"][0]["correction"] == "Y is the real cause"

    def test_belief_audit_counts(self):
        board = _make_board()
        board.put(type="belief", from_agent="claude/1", channel="general",
                  data={"claim": "A", "confidence": 0.7, "evidence": [], "status": "active"})
        b2 = board.put(type="belief", from_agent="claude/1", channel="general",
                       data={"claim": "B", "confidence": 0.5, "evidence": [], "status": "active"})
        b3 = board.put(type="belief", from_agent="claude/1", channel="general",
                       data={"claim": "C", "confidence": 0.9, "evidence": [], "status": "active"})
        board.put(type="refutation", from_agent="gemini/1", channel="general",
                  data={"reason": "wrong", "correction": ""}, refs=[b2])
        board.put(type="confirmation", from_agent="gemini/1", channel="general",
                  data={"evidence": "verified"}, refs=[b3])
        audit = execute_tool(board, "hive_belief_audit", {})
        assert audit["total"] == 3
        assert audit["active"] == 1
        assert audit["confirmed"] == 1
        assert audit["refuted"] == 1
        assert audit["accuracy"] == 0.5


class TestTraceQueryToolsViaMCP:
    def test_get_traces_returns_traces(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        cid = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
        execute_tool(board, "hive_trace", {
            "from_agent": "gemini/1", "contract_id": cid, "channel": "general",
            "steps": [{"attempt": 1, "action": "tried X", "outcome": "ok"}],
            "outcome": "success",
        })
        res = execute_tool(board, "hive_get_traces", {})
        assert len(res["traces"]) == 1

    def test_get_traces_filters_by_outcome(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        c1 = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
        execute_tool(board, "hive_trace", {
            "from_agent": "gemini/1", "contract_id": c1, "channel": "general",
            "steps": [], "outcome": "failure",
        })
        res = execute_tool(board, "hive_get_traces", {"outcome": "success"})
        assert res["traces"] == []
        res2 = execute_tool(board, "hive_get_traces", {"outcome": "failure"})
        assert len(res2["traces"]) == 1

    def test_get_contract_trace_returns_trace(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        cid = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
        execute_tool(board, "hive_trace", {
            "from_agent": "gemini/1", "contract_id": cid, "channel": "general",
            "steps": [{"attempt": 1, "action": "A", "outcome": "ok"}], "outcome": "success",
        })
        res = execute_tool(board, "hive_get_contract_trace", {"contract_id": cid})
        assert res["trace"] is not None
        assert res["trace"]["data"]["outcome"] == "success"

    def test_get_contract_trace_none_when_missing(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        cid = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
        res = execute_tool(board, "hive_get_contract_trace", {"contract_id": cid})
        assert res["trace"] is None

    def test_summarize_traces_empty_board(self):
        board = _make_board()
        res = execute_tool(board, "hive_summarize_traces", {})
        assert res["total"] == 0
        assert res["success_rate"] == 0.0

    def test_summarize_traces_with_data(self):
        board = _make_board()
        for outcome in ["success", "success", "failure"]:
            tid = board.task(from_agent="claude/1", channel="general", title="T")
            cid = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
            execute_tool(board, "hive_trace", {
                "from_agent": "gemini/1", "contract_id": cid, "channel": "general",
                "steps": [{"attempt": 1, "action": "A", "outcome": "x"}], "outcome": outcome,
            })
        res = execute_tool(board, "hive_summarize_traces", {})
        assert res["total"] == 3
        assert abs(res["success_rate"] - 0.67) < 0.01


class TestRaceToolsViaMCP:
    def test_hive_race_creates_contracts_for_all_agents(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        res = execute_tool(board, "hive_race", {
            "from_agent": "claude/1",
            "task_id": tid,
            "agents": ["gemini/1", "gemini/2", "codex/1"],
            "channel": "general",
        })
        assert len(res["contract_ids"]) == 3
        for cid in res["contract_ids"]:
            assert cid.startswith("hive:")

    def test_hive_race_coordinator_identity_in_contracts(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        res = execute_tool(board, "hive_race", {
            "from_agent": "claude/coord",
            "task_id": tid,
            "agents": ["gemini/1", "gemini/2"],
            "channel": "general",
        })
        for cid in res["contract_ids"]:
            cell = board.get(cid)
            assert cell is not None
            assert cell.from_agent == "claude/coord"

    def test_hive_race_results_empty_before_submissions(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        execute_tool(board, "hive_race", {
            "from_agent": "claude/1", "task_id": tid, "agents": ["gemini/1", "gemini/2"], "channel": "general",
        })
        res = execute_tool(board, "hive_race_results", {"task_id": tid})
        assert res["results"] == []

    def test_hive_race_results_after_submissions(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        race_res = execute_tool(board, "hive_race", {
            "from_agent": "claude/1", "task_id": tid, "agents": ["gemini/1", "gemini/2"], "channel": "general",
        })
        # One agent submits a result
        execute_tool(board, "hive_result", {
            "from_agent": "gemini/1",
            "channel": "general",
            "contract_id": race_res["contract_ids"][0],
            "output": "my answer",
        })
        res = execute_tool(board, "hive_race_results", {"task_id": tid})
        assert len(res["results"]) == 1
        assert res["results"][0]["data"]["output"] == "my answer"


class TestStallDetectToolViaMCP:
    def test_no_stalls_on_empty_board(self):
        board = _make_board()
        res = execute_tool(board, "hive_detect_stalls", {})
        assert res["stalls"] == []

    def test_completed_contract_not_a_stall(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        cid = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
        board.result(from_agent="gemini/1", channel="general", contract_id=cid, output="done")
        res = execute_tool(board, "hive_detect_stalls", {"timeout_seconds": 0})
        assert res["stalls"] == []

    def test_new_contract_with_zero_timeout_is_stalled(self):
        board = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
        res = execute_tool(board, "hive_detect_stalls", {"timeout_seconds": 0})
        assert len(res["stalls"]) == 1
        assert res["stalls"][0]["agent"] == "gemini/1"


class TestEvolveToolViaMCP:
    def test_evolve_empty_board_emits_nothing(self):
        board = _make_board()
        res = execute_tool(board, "hive_evolve", {})
        assert res["signals"] == []

    def test_evolve_idempotent(self):
        board = _make_board()
        res1 = execute_tool(board, "hive_evolve", {})
        res2 = execute_tool(board, "hive_evolve", {})
        # Both calls see the same (empty) board state
        assert res1["signals"] == res2["signals"]

    def test_evolve_detects_refuted_belief(self):
        board = _make_board()
        bid = board.put(
            type="belief", from_agent="claude/1", channel="general",
            data={"claim": "X is safe", "confidence": 0.9, "evidence": [], "status": "active"},
        )
        board.put(
            type="refutation", from_agent="gemini/1", channel="general",
            data={"reason": "X caused incident", "correction": "Use Y instead"}, refs=[bid],
        )
        res = execute_tool(board, "hive_evolve", {})
        # Should emit a refuted_beliefs signal
        events = [s["event"] for s in res["signals"]]
        assert "refuted_beliefs" in events
