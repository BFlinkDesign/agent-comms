<!-- Parent: ../AGENTS.md -->
<!-- Generated: 2026-03-02 | Updated: 2026-03-02 -->

# hive/ — HIVE Protocol Core Library

## Purpose

The core Python library implementing the HIVE protocol — immutable, content-addressable cells with deterministic SHA-256 IDs, transactional dual-write persistence (SQLite primary + JSONL secondary), and stateless coordination functions. All interagent communication flows through cells stored in HiveBoard. This package provides schema validation, cell lifecycle management, and a task-oriented API for submitting work, tracking dependencies, and collecting results.

## Key Files

| File | Description |
|------|-------------|
| `__init__.py` | Package exports: HiveBoard, Cell, cell_from_dict, cell_to_dict, make_cell. Version 1.1.0 |
| `board.py` | HiveBoard facade — dual-writes every cell to SQLite (primary, queryable) + JSONL (write-only compat). Public API: put, get, query, refs, watch, expire. Convenience methods: task, card, heartbeat, result, feedback. |
| `cell.py` | Cell dataclass (frozen=True) — immutable, content-addressed by SHA-256 hash of (type + from_agent + ts + channel + JSON(data)). ID format: "hive:" + first 16 chars of digest. refs and tags are tuple[str, ...] only. |

## Subdirectories

| Directory | Purpose |
|-----------|---------|
| `coordination/` | Stateless coordination functions: routing logic, reputation scoring, lease management, DAG validation, belief state, memory operations (see `coordination/AGENTS.md`) |
| `transports/` | Storage backends: SQLiteTransport (primary, queryable), JSONLTransport (write-only projection for backward compat) (see `transports/AGENTS.md`) |
| `mcp/` | MCP server exposing HIVE tools (cell_put, cell_get, cell_query, board_watch) to Claude Code (see `mcp/AGENTS.md`) |

## For AI Agents

### Working In This Directory

- **Cell is immutable:** frozen=True in dataclass. Never mutate; create new cells.
- **Tuple refs/tags:** Frozen dataclass requires tuple[str, ...] NOT list[str]. make_cell() handles list → tuple conversion.
- **Cell IDs are deterministic:** "hive:" + first 16 hex chars of SHA-256(type + from_agent + ts + channel + sorted_JSON_data). Same inputs = same ID always.
- **Import from hive.board:** `from hive.board import HiveBoard` — do NOT import from hive.transports directly.
- **Zero external dependencies in core:** hive/ uses only Python stdlib (sqlite3, json, hashlib, dataclasses, datetime, threading).

### Testing Requirements

```bash
cd C:/tools/agent-comms && python -m pytest tests/ --timeout=30 -q
```

- 145 tests total (all must pass)
- Test timeout: 30 seconds (critical for detecting hangs)
- Never run tests without `--timeout=30`

### Common Patterns

**Create and query cells:**
```python
from hive.board import HiveBoard

# Initialize board
board = HiveBoard(db_path="hive.db", channels_dir="C:/Users/Brady.EAGLE/.ai/channels")

# Create a task cell
task_id = board.task(
    from_agent="claude/architect",
    channel="general",
    title="Build Feature X",
    spec="Implement async queue with backpressure",
    bounty=10,
    quality_gates=["unit_tests", "integration_tests"],
    refs=None,
    tags=["priority:high", "epic:v1.0"]
)

# Query tasks on a channel
tasks = board.query(type="task", channel="general")

# Get a cell by ID
cell = board.get(task_id)

# Find all cells referencing a given ID
responses = board.refs(task_id)

# Create a result cell
result_id = board.result(
    from_agent="claude/architect",
    channel="general",
    contract_id=task_id,
    output="Implementation complete. All tests passing.",
    artifacts=["pr#1234"],
    metrics={"coverage": 0.92, "latency_ms": 45}
)

# Create feedback for a result
board.feedback(
    from_agent="user/qa",
    channel="general",
    result_id=result_id,
    contract_id=task_id,
    score=8,
    notes="Good quality. Minor docs issue.",
    tags=["review:approved"]
)

# Watch for new heartbeats on roster
def on_heartbeat(cell):
    print(f"Agent {cell.from_agent} status: {cell.data['status']}")

board.watch("roster", on_heartbeat, type_filter="heartbeat")

# Expire old cells (TTL > 0)
expired_count = board.expire()
```

### Cell Schema Reference

Every cell has this structure:

```python
@dataclass(frozen=True)
class Cell:
    id: str                          # "hive:" + SHA-256 hash[:16]
    v: int                           # Schema version (currently 1)
    type: str                        # "task" | "result" | "feedback" | "card" | "heartbeat" | etc.
    from_agent: str                  # "name/role" (e.g., "claude/architect")
    ts: str                          # ISO 8601 timestamp with timezone
    channel: str                     # "general" | "roster" | "inbox" | custom
    data: dict[str, Any]             # Type-specific payload
    refs: tuple[str, ...]            # Cell IDs this cell references (default: ())
    ttl: int                         # Time to live in seconds (0 = permanent)
    tags: tuple[str, ...]            # Free-form tags (e.g., "priority:high")
    sig: str | None                  # Optional signature (reserved for future)
```

**Standard cell types:**

- `task` — work item with title, spec, bounty, deadline, quality gates, race mode, auto-assign
- `result` — deliverable for a task (output, artifacts, metrics)
- `feedback` — evaluation of a result (score 0-10, notes, tags)
- `card` — agent capability advertisement (capabilities list, cost profile, models)
- `heartbeat` — agent alive signal (contract_id, progress %, status, TTL=120s)

### Common Mistakes to Avoid

1. **Don't mutate cells.** They're frozen. Create new ones instead.
2. **Don't pass lists to put() for refs/tags.** Convert to tuple first, or use make_cell() which handles it.
3. **Don't bypass HiveBoard.** Always use board.put()/board.get()/board.query(). Never call transports directly.
4. **Don't serialize Cell objects to JSON directly.** Use cell_to_dict(cell) → JSON, not dataclasses.asdict().
5. **Don't hardcode channel paths.** Use board's channels_dir parameter. Canonical path: C:/Users/Brady.EAGLE/.ai/channels/
6. **Don't skip tests when making changes.** Always run pytest --timeout=30 after edits.

## Dependencies

### Internal

- `transports/sqlite.py` — SQLite backend (queryable, indexed, TTL expiry)
- `transports/jsonl.py` — JSONL backend (write-only, sequential, append-only guarantee)

### External

- **Python stdlib only:** sqlite3, json, hashlib, dataclasses, datetime, threading
- **No pip dependencies required for core hive package**
- Dashboard and tests have their own requirements (see pyproject.toml)

## API Reference

### HiveBoard

**Constructor:**
```python
board = HiveBoard(db_path: str = "hive.db", channels_dir: str = "channels")
```

**Core methods:**
- `put(...) -> str` — Create and store a cell. Returns cell ID.
- `put_cell(cell: Cell) -> str` — Store a pre-built Cell. Returns cell ID.
- `get(cell_id: str) -> Cell | None` — Retrieve a cell by ID.
- `query(**kwargs) -> list[Cell]` — Find cells matching criteria. See SQLiteTransport for filter params.
- `refs(cell_id: str) -> list[Cell]` — Return all cells that reference the given ID.
- `expire() -> int` — Remove expired cells (TTL > 0 and age > TTL). Returns count removed.
- `watch(channel: str, callback, type_filter: str | None = None)` — Subscribe to new cells.

**Convenience methods:**
- `task(from_agent, channel, title, spec, bounty, deadline, quality_gates, race, auto_assign, refs, tags) -> str`
- `card(from_agent, capabilities, cost_profile, models, channel) -> str`
- `heartbeat(from_agent, contract_id, progress, status, channel) -> str`
- `result(from_agent, channel, contract_id, output, artifacts, metrics) -> str`
- `feedback(from_agent, channel, result_id, contract_id, score, notes, tags) -> str`

### Cell Factory Functions

**make_cell()** — Create Cell with auto-generated ID and timestamp
```python
cell = make_cell(
    type="task",
    from_agent="claude/architect",
    channel="general",
    data={"title": "Feature X", "bounty": 5},
    ts=None,  # Auto-generate if None
    refs=["cell1", "cell2"],  # List → tuple auto-conversion
    ttl=0,
    tags=["priority:high"],  # List → tuple auto-conversion
    sig=None
)
```

**cell_to_dict(cell: Cell) -> dict** — Serialize Cell to JSON-compatible dict
- Maps `from_agent` → `from` for protocol compatibility
- Converts tuples back to lists

**cell_from_dict(d: dict) -> Cell** — Deserialize dict to Cell
- Maps `from` → `from_agent`
- Handles missing optional fields with sensible defaults

### SQLiteTransport.query() Parameters

Full parameter list for `board.query()`:

```python
board.query(
    type: str | None = None,           # Filter by cell type
    channel: str | None = None,         # Filter by channel
    from_agent: str | None = None,      # Filter by sender
    after_ts: str | None = None,        # ISO timestamp (inclusive)
    before_ts: str | None = None,       # ISO timestamp (inclusive)
    tag: str | None = None,             # Match any tag (substring)
    limit: int | None = None,           # Max results
    order: str = "asc"                  # "asc" or "desc" by timestamp
)
```

<!-- MANUAL: -->
