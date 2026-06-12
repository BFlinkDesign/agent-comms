"""Tests for hive.coordination.memory -- episodic trace storage."""
import os
import tempfile

from hive.board import HiveBoard
from hive.coordination.memory import (
    get_contract_trace,
    get_traces,
    record_trace,
    summarize_traces,
)


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(
        db_path=os.path.join(tmpdir, "test.db"),
        channels_dir=os.path.join(tmpdir, "ch"),
    )


class TestRecordTrace:
    def test_record_trace_returns_cell_id(self):
        board = _make_board()
        contract_id = board.put(
            type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}
        )
        trace_id = record_trace(
            board,
            from_agent="gemini/1",
            contract_id=contract_id,
            channel="general",
            steps=[{"attempt": 1, "action": "tried X", "outcome": "succeeded"}],
        )
        assert trace_id.startswith("hive:")

    def test_trace_links_to_contract(self):
        board = _make_board()
        contract_id = board.put(
            type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}
        )
        record_trace(
            board,
            from_agent="gemini/1",
            contract_id=contract_id,
            channel="general",
            steps=[{"attempt": 1, "action": "A", "outcome": "ok"}],
        )
        trace = get_contract_trace(board, contract_id)
        assert trace is not None
        assert trace.type == "trace"
        assert contract_id in trace.refs

    def test_get_contract_trace_none_when_missing(self):
        board = _make_board()
        contract_id = board.put(
            type="contract", from_agent="claude/1", channel="general", data={}
        )
        assert get_contract_trace(board, contract_id) is None

    def test_trace_stores_steps(self):
        board = _make_board()
        contract_id = board.put(
            type="contract", from_agent="claude/1", channel="general", data={}
        )
        steps = [
            {"attempt": 1, "action": "tried direct", "outcome": "failed: 404"},
            {"attempt": 2, "action": "tried fallback", "outcome": "succeeded"},
        ]
        record_trace(
            board,
            from_agent="gemini/1",
            contract_id=contract_id,
            channel="general",
            steps=steps,
            outcome="success",
        )
        trace = get_contract_trace(board, contract_id)
        assert trace.data["step_count"] == 2
        assert trace.data["outcome"] == "success"
        assert len(trace.data["steps"]) == 2


class TestGetTraces:
    def test_get_traces_returns_traces(self):
        board = _make_board()
        c = board.put(type="contract", from_agent="claude/1", channel="general", data={})
        record_trace(
            board, from_agent="gemini/1", contract_id=c, channel="general",
            steps=[{"attempt": 1, "action": "A", "outcome": "done"}]
        )
        traces = get_traces(board)
        assert len(traces) == 1

    def test_get_traces_filters_by_outcome(self):
        board = _make_board()
        c1 = board.put(type="contract", from_agent="claude/1", channel="general", data={})
        c2 = board.put(type="contract", from_agent="claude/1", channel="general", data={}, ts="2026-03-01T01:00:00+00:00")
        record_trace(
            board, from_agent="gemini/1", contract_id=c1, channel="general",
            steps=[], outcome="success"
        )
        record_trace(
            board, from_agent="gemini/1", contract_id=c2, channel="general",
            steps=[], outcome="failure"
        )
        successes = get_traces(board, outcome="success")
        assert len(successes) == 1
        assert successes[0].data["outcome"] == "success"


class TestSummarizeTraces:
    def test_summarize_empty_returns_zeros(self):
        board = _make_board()
        summary = summarize_traces(board)
        assert summary["total"] == 0
        assert summary["success_rate"] == 0.0

    def test_summarize_computes_success_rate(self):
        board = _make_board()
        # 2 success, 1 failure
        for i, outcome in enumerate(["success", "success", "failure"]):
            c = board.put(type="contract", from_agent="claude/1", channel="general", data={}, ts=f"2026-03-01T{i:02d}:00:00+00:00")
            record_trace(
                board, from_agent="gemini/1", contract_id=c, channel="general",
                steps=[{"a": i}], outcome=outcome
            )
        summary = summarize_traces(board)
        assert summary["total"] == 3
        assert abs(summary["success_rate"] - 0.67) < 0.01
