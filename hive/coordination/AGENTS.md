<!-- Parent: ../AGENTS.md -->
<!-- Generated: 2026-03-02 | Updated: 2026-03-02 -->

# hive/coordination/ — Stateless Coordination Functions

## Purpose
All coordination logic as pure functions that accept HiveBoard — no daemons, no background
processes, no shared state outside the Board. Every module is independently importable.

## Key Files
| File | Description |
|------|-------------|
| `router.py` | Capability-based routing: scores agents by `capability_overlap × reputation / (1+cost)` |
| `reputation.py` | Exponential decay scoring from feedback cells — recent feedback weighted more heavily |
| `leases.py` | Advisory file locks with TTL — race-safe task claiming without a central server |
| `stall_detector.py` | Heartbeat monitoring — emits signal cells when agents go silent |
| `dag.py` | Task dependency resolution — finds ready tasks via lifecycle reducer |
| `lifecycle.py` | Canonical task state reducer — maps events to A2A lifecycle states |
| `racing.py` | Multi-agent fan-out — sends same task to N agents, first valid result wins |
| `evolution.py` | Feedback loop analysis — detects high failure rates and refuted beliefs, emits signals |
| `memory.py` | Episodic trace storage — records HOW problems were solved (steps + outcome) |
| `beliefs.py` | Auditable priors — agents assert beliefs before acting; can be refuted/confirmed |

## Module Details

### router.py
**Capability-based task routing.** Matches task requirements against agent capability cards.

**Key function:** `route_task(board, task) -> list[(agent_id, score)]`
- Extracts `required_capabilities` from task cell
- Queries all capability cards from board
- Scores each agent: `capability_overlap × reputation / (1 + cost)`
  - `capability_overlap`: fraction of required skills agent possesses
  - `reputation`: decayed score from recent feedback (via reputation module)
  - `cost`: agent's declared output cost profile
- Returns agents sorted by score (descending)

### reputation.py
**Reputation scoring with exponential decay.** Recent feedback matters more than old feedback.

**Key function:** `reputation(board, agent_id, capability=None) -> float`
- Default score: 5.0 (neutral, no history)
- Decay factor: 0.95 per feedback (older = lower weight)
- Algorithm:
  1. Find all contracts where `agent == agent_id`
  2. Collect all feedback cells referencing those contracts
  3. Sort feedback by timestamp (newest first)
  4. Apply weights: `[0.95^0, 0.95^1, 0.95^2, ...]`
  5. Return weighted average of feedback scores
- Optional `capability` filter: only count feedback tagged with that capability

### beliefs.py
**Auditable agent priors.** Agents write belief cells before acting; Critic agents can mark them refuted/confirmed.

**Key function:** `assert_belief(board, *, from_agent, channel, claim, confidence=0.7, evidence=None, refs=None, tags=None) -> belief_id`
- Records explicit prior: "I believe X is true"
- Stores confidence (0.0-1.0) and optional evidence list
- Can reference other cells via `refs` parameter
- Tags enable filtering by domain/capability
- Belief lifecycle: asserted → refuted (or confirmed) → evolution module emits signal

Refuted beliefs trigger `evolution.py` to emit improvement signals automatically.

## For AI Agents

### Architecture Pattern
All functions follow this signature:
```python
def function_name(board: HiveBoard, *, keyword_args) -> result
```

**Rules:**
- Never instantiate coordination modules as objects
- Never store state outside the Board
- Board is the single source of truth for all coordination state
- All results are expressed as new cells on the board

### Router Formula
```
Score = capability_match × reputation / (1 + cost)
```

Where:
- `capability_match`: fraction of required skills agent has (0.0-1.0)
- `reputation`: exponential-decay weighted score (5.0 baseline, typically 0.0-10.0)
- `cost`: output token cost from agent's capability card (lower = higher priority)

Example: Agent with 80% capability overlap, 7.2 reputation, 0.5 cost:
```
Score = 0.8 × 7.2 / (1 + 0.5) = 5.76 / 1.5 = 3.84
```

### Belief Lifecycle
```
1. Agent: assert_belief("I believe X causes error 1102")
   ↓
2. Agent: attempts solution based on belief
   ↓
3a. If successful → confirm_belief(belief_id)
    ↓ evolution.py: reinforces agent confidence

3b. If fails → Critic: refute_belief(belief_id, "X does not cause 1102")
    ↓ evolution.py: emits signal to adjust agent reasoning
```

Refuted beliefs are automatically detected and trigger improvement signals.

### Adding New Coordination Logic
1. Create `new_module.py` with pure functions only
2. First argument: `board: HiveBoard`
3. Write results via `board.put()` to store on board
4. Add tests in `tests/test_new_module.py`
5. Wire into `hive/mcp/tools.py` if exposing to MCP agents
6. Document signature and side effects in docstring

### Common Patterns

**Query for specific cells:**
```python
cards = board.query(type="card")  # Capability declarations
contracts = board.query(type="contract")  # Work assignments
feedback = board.query(type="feedback")  # Agent reviews
```

**Find cells referencing a contract:**
```python
refs = board.refs(contract.id)  # All feedback for this contract
```

**Access cell data:**
```python
agent_id = card.from_agent  # Who emitted this cell
capabilities = card.data.get("capabilities", [])  # Task skills
cost = card.data.get("cost_profile", {}).get("output", 1)  # Token cost
```

**Emit coordination result:**
```python
result_cell = board.put(
    type="routing_decision",
    from_agent="coordinator",
    data={
        "task_id": task.id,
        "ranked_agents": [(agent, score), ...],
        "recommendation": "agent_123"
    },
    tags=["routing", "task_id"]
)
```

## Dependencies
### Internal
- `hive.board.HiveBoard` — all functions accept board as first parameter
- `hive.cell.Cell` — units of coordination state
### External
- Python stdlib only (no external dependencies)

## Testing
All modules include unit tests in `tests/test_*.py` with fixtures for:
- Empty board (no agents, no feedback)
- Single agent (isolated case)
- Multiple agents with conflicting reputation
- Feedback with stale timestamps
- Malformed capability cards

Run: `pytest tests/ -v` (timeout: 30s)
