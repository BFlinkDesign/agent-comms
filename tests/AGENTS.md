<!-- Parent: ../AGENTS.md -->
<!-- Generated: 2026-03-02 | Updated: 2026-03-02 -->

# tests/ — HIVE Test Suite

## Purpose
145-test suite covering all HIVE protocol layers. Zero external APIs — all tests use
temp SQLite databases and temp dirs. Run with: python -m pytest tests/ --timeout=30 -q

## Key Files
|------|-------|---------------|
| `test_lifecycle.py` | 8 | End-to-end: full task flow submit→claim→work→complete |
| `test_sqlite_transport.py` | ~10 | SQLite WAL mode, thread-local connections, all 6 board ops |
| `test_board.py` | ~8 | HiveBoard facade: dual-write, query, watch, refs |
| `test_beliefs.py` | 8 | assert_belief, refute_belief, confirm_belief, get_active/refuted, audit |
| `test_memory.py` | 8 | record_trace, get_contract_trace, summarize_traces |
| `test_evolution.py` | 3 | high_failure_rate signal, refuted_beliefs signal, no-data case |
| `test_cell.py` | ~6 | Cell schema, content-addressed IDs, frozen invariant |
| `test_dag.py` | ~4 | DAG dependency resolution, ready-task detection |
| `test_leases.py` | ~3 | Race-safe lease acquisition, TTL expiry |
| `test_racing.py` | ~3 | Multi-agent fan-out |
| `test_reputation.py` | ~4 | Exponential decay scoring |
| `test_router.py` | ~4 | capability × reputation / (1+cost) routing |
| `test_stall_detector.py` | ~3 | Heartbeat monitoring, signal emission |
| `test_jsonl_transport.py` | ~8 | Append-only JSONL, channel file creation |
| `test_mcp_tools.py` | ~6 | MCP tool definitions and execute_tool dispatch |
| `test_runtime_hardening.py` | ~28 | Runtime config, channel validation, dashboard auth, shell safety |
| `test_lifecycle_reducer.py` | 19 | Canonical task lifecycle reducer and readiness |

## For AI Agents

### Running Tests
```bash
cd C:/tools/agent-comms
python -m pytest tests/ --timeout=30 -q          # all 145
python -m pytest tests/test_lifecycle.py -v       # specific file
python -m pytest tests/ --timeout=30 -q --tb=short  # with short tracebacks
```

### Test Conventions
- Every test uses tempfile.mkdtemp() — no shared state between tests
- No real APIs, no network calls, no paid services
- Tests are the source of truth for API contracts

### Never Delete Tests
Fix broken code, never delete or skip tests. A failing test is information.

## Dependencies
- pytest + pytest-timeout (see pyproject.toml)
- All hive.* modules under test
