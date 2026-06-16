"""Tests for hive.board -- the HiveBoard facade."""
import os
import tempfile

from hive.board import HiveBoard


def _make_board():
    tmpdir = tempfile.mkdtemp()
    db_path = os.path.join(tmpdir, "test.db")
    channels_dir = os.path.join(tmpdir, "channels")
    return HiveBoard(db_path=db_path, channels_dir=channels_dir), tmpdir


class TestHiveBoard:
    def test_put_and_get(self):
        board, _ = _make_board()
        cell_id = board.put(type="task", from_agent="claude/1", channel="general", data={"title": "test"})
        cell = board.get(cell_id)
        assert cell is not None
        assert cell.data["title"] == "test"

    def test_put_creates_jsonl_file(self):
        board, tmpdir = _make_board()
        board.put(type="task", from_agent="claude/1", channel="general", data={})
        assert os.path.exists(os.path.join(tmpdir, "channels", "general.jsonl"))

    def test_query_delegates_to_sqlite(self):
        board, _ = _make_board()
        board.put(type="task", from_agent="claude/1", channel="general", data={"a": 1})
        board.put(type="card", from_agent="claude/1", channel="general", data={"b": 2})
        results = board.query(type="task")
        assert len(results) == 1

    def test_refs_works(self):
        board, _ = _make_board()
        parent_id = board.put(type="task", from_agent="claude/1", channel="general", data={"title": "parent"})
        board.put(type="bid", from_agent="gemini/1", channel="general", data={}, refs=[parent_id])
        refs = board.refs(parent_id)
        assert len(refs) == 1
        assert refs[0].type == "bid"

    def test_expire_works(self):
        board, _ = _make_board()
        board.put(type="heartbeat", from_agent="claude/1", channel="general", data={}, ttl=1, ts="2020-01-01T00:00:00+00:00")
        removed = board.expire()
        assert removed >= 1

    def test_task_convenience(self):
        board, _ = _make_board()
        board.task(from_agent="claude/1", channel="general", title="Do thing", spec="Details here")
        results = board.query(type="task")
        assert len(results) == 1
        assert results[0].data["title"] == "Do thing"

    def test_card_convenience(self):
        board, _ = _make_board()
        board.card(from_agent="gemini/1", capabilities=["scanning", "analysis"])
        results = board.query(type="card")
        assert len(results) == 1
        assert "scanning" in results[0].data["capabilities"]

    def test_heartbeat_convenience(self):
        board, _ = _make_board()
        cid = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"})
        board.heartbeat(from_agent="gemini/1", contract_id=cid, progress=50)
        results = board.query(type="heartbeat")
        assert len(results) == 1
        assert results[0].data["progress"] == 50

    def test_result_convenience(self):
        board, _ = _make_board()
        cid = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"})
        board.result(from_agent="gemini/1", channel="general", contract_id=cid, output="done", artifacts=["out.txt"])
        results = board.query(type="result")
        assert len(results) == 1
        assert results[0].data["output"] == "done"

    def test_feedback_convenience(self):
        board, _ = _make_board()
        cid = board.put(type="contract", from_agent="claude/1", channel="general", data={"agent": "gemini/1"})
        rid = board.put(type="result", from_agent="gemini/1", channel="general", data={"output": "x"}, refs=[cid])
        board.feedback(from_agent="claude/1", channel="general", result_id=rid, contract_id=cid, score=8, notes="good")
        results = board.query(type="feedback")
        assert len(results) == 1
        assert results[0].data["score"] == 8

    def test_contract_convenience(self):
        board, _ = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        cid = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
        cell = board.get(cid)
        assert cell is not None
        assert cell.type == "contract"
        assert cell.data["agent"] == "gemini/1"
        assert tid in cell.refs

    def test_contract_refs_task(self):
        board, _ = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        cid = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1")
        back_refs = board.refs(tid)
        assert any(c.id == cid for c in back_refs)

    def test_contract_race_flag(self):
        board, _ = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        cid = board.contract(from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1", race=True)
        cell = board.get(cid)
        assert cell is not None
        assert cell.data["race"] is True

    def test_contract_extra_refs(self):
        board, _ = _make_board()
        tid = board.task(from_agent="claude/1", channel="general", title="T")
        bid_id = board.put(type="bid", from_agent="gemini/1", channel="general", data={"cost": 3}, refs=[tid])
        cid = board.contract(
            from_agent="claude/1", channel="general", task_id=tid, agent="gemini/1", refs=[bid_id],
        )
        cell = board.get(cid)
        assert cell is not None
        assert tid in cell.refs
        assert bid_id in cell.refs
