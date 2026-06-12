"""Tests for hive.coordination.stall_detector."""
import os
import tempfile

from hive.board import HiveBoard
from hive.coordination.stall_detector import detect_stalls


def _make_board():
    tmpdir = tempfile.mkdtemp()
    return HiveBoard(db_path=os.path.join(tmpdir, "test.db"), channels_dir=os.path.join(tmpdir, "ch"))


class TestStallDetector:
    def test_no_contracts_no_stalls(self):
        board = _make_board()
        stalls = detect_stalls(board)
        assert stalls == []

    def test_contract_with_result_not_stalled(self):
        board = _make_board()
        task_id = board.put(type="task", from_agent="claude/1", channel="general", data={})
        contract_id = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}, refs=[task_id])
        board.put(type="result", from_agent="gemini/1", channel="general", data={"output": "done"}, refs=[contract_id])
        stalls = detect_stalls(board)
        assert stalls == []

    def test_old_contract_no_heartbeat_is_stalled(self):
        board = _make_board()
        task_id = board.put(type="task", from_agent="claude/1", channel="general", data={}, ts="2020-01-01T00:00:00+00:00")
        contract_id = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}, refs=[task_id], ts="2020-01-01T00:00:01+00:00")
        stalls = detect_stalls(board, timeout_seconds=60)
        assert len(stalls) == 1
        assert stalls[0]["contract_id"] == contract_id
        assert stalls[0]["agent"] == "gemini/1"


class TestStallSignalDedup:
    def _signals_for(self, board, contract_id):
        return [
            r for r in board.refs(contract_id)
            if r.type == "signal" and r.data.get("event") == "stall_detected"
        ]

    def test_repeated_runs_emit_one_signal(self):
        board = _make_board()
        contract_id = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}, ts="2020-01-01T00:00:00+00:00")

        first = detect_stalls(board, timeout_seconds=60)
        second = detect_stalls(board, timeout_seconds=60)
        third = detect_stalls(board, timeout_seconds=60)

        # The stall is still reported on every run...
        assert len(first) == len(second) == len(third) == 1
        # ...but only one signal cell is ever written for the episode.
        assert len(self._signals_for(board, contract_id)) == 1

    def test_new_heartbeat_starts_new_episode(self):
        board = _make_board()
        contract_id = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"}, ts="2020-01-01T00:00:00+00:00")
        # A signal from a previous episode, then the agent made progress.
        board.put(
            type="signal",
            from_agent="hive/stall-detector",
            channel="general",
            data={"event": "stall_detected", "payload": {}},
            refs=[contract_id],
            ts="2020-01-02T00:00:00+00:00",
        )
        board.put(
            type="heartbeat",
            from_agent="gemini/1",
            channel="general",
            data={"contract_id": contract_id},
            refs=[contract_id],
            ts="2020-01-03T00:00:00+00:00",
        )

        stalls = detect_stalls(board, timeout_seconds=60)

        assert len(stalls) == 1
        # Progress after the old signal means this stall is a new episode.
        assert len(self._signals_for(board, contract_id)) == 2
