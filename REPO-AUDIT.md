# REPO-AUDIT.md

Read-only audit of `agent-comms` (a.k.a. `hive-protocol`).
Auditor: Claude Code. Date: 2026-05-18. Branch: `claude/audit-repo-sources-NFxqp`.
Tags: [VERIFIED] ran/read exact code · [INFERRED] reasoned from indirect evidence · [UNKNOWN] could not determine.

---

## PHASE 1 — SOURCES OF TRUTH

### What exists, by authority tier

**TIER 1 — Enforced + executable: NONE.** [VERIFIED]
- No `.github/` directory; no GitHub Actions/workflows. [VERIFIED] (`[ -d .github ]` → "NO")
- No `.gitlab-ci`, `Jenkinsfile`, `Makefile`, `tox.ini`, `noxfile.py`, `*.cfg` anywhere. [VERIFIED] (find returned nothing)
- No `.pre-commit-config.yaml`. [VERIFIED]
- No custom git hooks — `.git/hooks/` contains only `*.sample` files. [VERIFIED]
- Runtime assertions/contracts: the `hive` package performs cell-schema validation and lifecycle enforcement in code (Tier 1-like at runtime), but nothing *enforces* that this code is run or correct. Classified as Tier 2/3 below since no gate executes it. [INFERRED]
- **Conclusion: nothing is enforced. The de facto contract is "whatever the `main` branch currently does."** [VERIFIED]

**TIER 2 — Executable but unenforced:**
- `tests/` — 98 test functions across 16 files. [VERIFIED] (`grep -rE '^\s*def test_' tests/*.py | wc -l` → 98)
  Breakdown: test_sqlite_transport 17, test_cell 12, test_board 10, test_beliefs 8, test_lifecycle 8, test_memory 8, test_jsonl_transport 6, test_mcp_tools 6, test_leases 5, test_dag 4, test_evolution 3, test_reputation 3, test_router 3, test_stall_detector 3, test_racing 2. [VERIFIED]
- `pyproject.toml` declares `[tool.pytest.ini_options]` with `timeout=30`, `testpaths=["tests"]`. [VERIFIED] Tests are runnable but run by nothing automatically.
- Example/integration scripts: `comms.sh`, `agent-runner.sh`, `codex-wrap.py`, `dashboard/start.sh` — executable, not gated. [VERIFIED] (exist; behavior not yet checked)
- `tests/test_lifecycle.py` is described in commit `5422390` as "Full lifecycle integration test — 8 scenarios". [VERIFIED] (commit log + 8 test fns)

**TIER 3 — Declarative contracts:**
- `manifest.json` — machine-readable protocol def: `protocol: "agent-comms/2.0"`, identity format `agent/session`, message_schema, channel registry, agent roster. [VERIFIED]
- `PROTOCOL.md` cell schemas (JSON examples for task/claim/status/result/blocked cells, agent-card.json). Prose-embedded JSON, not a validatable schema file. [VERIFIED]
- `pyproject.toml` — package metadata: name `hive-protocol`, version `1.0.0`, requires-python `>=3.11`. [VERIFIED]
- `.mcp.json` — MCP server config (`python -m hive.mcp.server`, Windows paths). [VERIFIED]
- `org.json` — organizational/ownership config (not yet read in full). [VERIFIED exists]
- No JSON Schema / OpenAPI / protobuf / SQL DDL files found. [VERIFIED] (no schema files in tree)

**TIER 4 — Human prose, drift-prone:**
- `README.md` (quick-start, command table, message schema), `AGENTS.md` (root + 7 nested AGENTS.md in hive/, hive/*, tests/, channels/, dashboard/, docs/, docs/plans/), `PROTOCOL.md` (prose + schema), `FLEET-OPS.md` (post-mortem), `standards.md` (operating standards), `ROLE-PROMPTS.md`, `CODEX-PROMPT.md`, `RESEARCH.md`, `docs/plans/*.md` (2 design/plan docs). [VERIFIED exist]

**TIER 5 — Intent + history:**
- Git history: 16+ commits, conventional-commit style (`feat(hive):`, `fix:`, `docs:`). [VERIFIED] (`git log --oneline`)
- No `CHANGELOG`. [VERIFIED] (not in tree)
- No TODO/FIXME/HACK/XXX markers in any `.py` or `.sh` file. [VERIFIED] (grep returned nothing)
- "Known fleet lessons" / "Never-Again Rules" embedded in `standards.md` and root `AGENTS.md`. [VERIFIED]

### What is absent
- Any CI/CD. Any enforced gate. Any pre-commit. Any schema file. Any CHANGELOG. Any inline TODO debt markers. [VERIFIED]

### Highest-authority source present
Because Tier 1 is empty, the benchmark is **the Tier 2 test suite (`tests/`, 98 functions) plus the Tier 3 declarative contracts (`manifest.json`, `PROTOCOL.md` schemas, `pyproject.toml`)**. Where these disagree with each other or with the code, authority falls to *the code as exercised by the passing tests* — i.e. "whatever `main` currently does," since nothing else is enforced. The single highest *named* artifact the repo treats as canonical is **`PROTOCOL.md`** (root AGENTS.md calls it "Canonical A2A-aligned task lifecycle schema and cell format specification"), but it is only Tier 4 prose and already visibly conflicts with Tier 3 `manifest.json` (see Phase 5).

### Early signal (full treatment in Phase 5)
The repo carries **two overlapping protocol definitions**: `manifest.json` ("agent-comms/2.0", flat message schema, `clock-in`/`clock-out` types, identity `agent/session`) vs. `PROTOCOL.md`/`hive` ("HIVE Fleet Protocol v1.2", A2A 7-state lifecycle, `status`/`context_id` fields). `pyproject.toml` versions the package `1.0.0`. Three different version numbers (2.0 / v1.2 / 1.0.0) for the same system. [VERIFIED]

---

## PHASE 2 — RUN STATUS

**Environment:** Python 3.11.15 (`requires-python = ">=3.11"` satisfied [VERIFIED]). Linux; repo at `/home/user/agent-comms`. Repo is written for Windows + Git Bash (hardcoded `C:/` paths everywhere — see Phase 5). [VERIFIED]

**Entry points & exact commands:**
1. `hive` Python package (library) — `import hive` works with stdlib only. [VERIFIED] (imported every module successfully)
2. MCP server — `python -m hive.mcp.server [--db DB] [--channels CHANNELS]` (also `python -m hive.mcp`). `--help` exits 0. [VERIFIED] Stdio JSON-RPC loop (not started/handshaked in this audit — [UNKNOWN] beyond `--help`).
3. `comms.sh` — sourced bash CLI: `source comms.sh` then `comms <cmd>`. `bash -n comms.sh` passes. [VERIFIED syntax only]. Defaults `COMMS_DIR=C:/tools/agent-comms`, `CHANNELS_DIR=${COMMS_CHANNELS:-C:/Users/Brady.EAGLE/.ai/channels}`, `COMMS_AGENT=unknown`. [VERIFIED]
4. `agent-runner.sh <channel>` — dispatch loop. `bash -n` passes. [VERIFIED syntax only]
5. Dashboard — `bash dashboard/start.sh` → `pip install -r requirements.txt` (fastapi, uvicorn) then `python server.py`, serving `0.0.0.0:7842`. [VERIFIED code path] fastapi/uvicorn NOT preinstalled. [VERIFIED]

**Dependency resolution:** Core `hive` + tests: stdlib + `pytest`/`pytest-timeout` only (pytest installable via pip; network reachable). [VERIFIED] Dashboard: `fastapi`+`uvicorn` not present until `start.sh` installs them. [VERIFIED]

**Verdict: the repo RUNS** (Python package imports; 98/98 tests pass; MCP entry point responds; all scripts pass `bash -n`/`py_compile`). Proceeding past Phase 2. The bash bus and dashboard could not be exercised end-to-end here because their hardcoded `C:/...` paths do not exist on this Linux host — that is a portability defect, not a startup failure of the audited Python core. [VERIFIED core] / [INFERRED bash bus untested at runtime]

---

## PHASE 3 — STRUCTURAL MAP

**Two parallel, largely independent systems share this repo:**

### A. Python `hive` package (SQLite+JSONL coordination library)
| Module | Job (one line) |
|---|---|
| `hive/__init__.py` | Public API exports; declares `__version__ = "1.1.0"`. [VERIFIED] |
| `hive/cell.py` | Immutable `Cell` dataclass; `make_cell`, `cell_to_dict`, `cell_from_dict`; SHA-256 content-addressed ID. [VERIFIED] |
| `hive/board.py` | `HiveBoard` facade; dual-writes SQLite (queryable) + JSONL (write-only projection); convenience constructors task/card/heartbeat/result/feedback. [VERIFIED] |
| `hive/transports/sqlite.py` | Primary store; DDL, query/refs/expire/watch; 336 LOC. [VERIFIED] |
| `hive/transports/jsonl.py` | Append-only per-channel JSONL writer (write-only). [VERIFIED] |
| `hive/coordination/{router,reputation,leases,dag,racing,evolution,stall_detector,beliefs,memory}.py` | Coordination algorithms over a board. [VERIFIED] |
| `hive/mcp/{server,tools,__main__}.py` | JSON-RPC stdio MCP server exposing 12 tools. [VERIFIED] |

**Runtime call path (MCP entry):** `python -m hive.mcp` → `hive.mcp.server.run_server` → `_read_message` loop → `execute_tool(board, name, args)` → `HiveBoard` → `SQLiteTransport.put` + `JSONLTransport.put`. [VERIFIED]

**MCP tool surface (12):** `hive_put, hive_get, hive_query, hive_refs, hive_expire, hive_task, hive_card, hive_heartbeat, hive_feedback, hive_trace, hive_belief, hive_refute`. `execute_tool` dispatches only to `HiveBoard` + `beliefs` + `memory`. [VERIFIED]

**Import graph:** clean. No missing imports; no circular imports. `hive.coordination.router`→`reputation`; `evolution`→`beliefs`; `mcp.tools`→`beliefs,memory`. [VERIFIED]

**Dead code relative to runtime entry points:** `coordination/{dag, evolution, leases, racing, reputation, router, stall_detector}` are imported ONLY by the test suite (and the intra-package `router`→`reputation`, `evolution`→`beliefs` edges). None are reachable from the MCP server or `comms.sh`. They are exercised by tests but wired to no entry point. [VERIFIED imports] / [INFERRED "dead at runtime"]

### B. Bash bus (`comms.sh` + `agent-runner.sh`)
- `comms.sh` (990 LOC): `_comms_write` emits flat JSON `{id,from,ts,channel,type,msg,data}` to `${CHANNELS_DIR}/<ch>.jsonl`; reader prints `m.get('msg','')`. Subcommands: send/task/result/error/phone-home/handoff/ack/read/status/clock-in/clock-out/hire/roster/…, plus a `comms hive <put|get|query|task>` bridge that shells into the Python package. [VERIFIED]
- `agent-runner.sh` (422 LOC): poll loop; finds tagged SUBMITTED tasks; lease-claim; invokes `AGENT_CMD`; for `codex` pipes through hardcoded `C:/tools/agent-comms/codex-wrap.py`; posts result. [VERIFIED]
- `codex-wrap.py` (184 LOC): stdin→`codex` subprocess→strips ANSI/progress→prints summary. [VERIFIED]

**Coupling A↔B:** only via the optional `comms hive` bridge and the shared `channels/*.jsonl` directory. The two systems use **incompatible record schemas** for that shared directory (Phase 5). [VERIFIED]

**Flagged files:**
- Imported/referenced but **missing**: `.boot-gemini-backfill.sh` — listed in root `AGENTS.md` "Key Files" but does not exist (it is gitignored as a generated `.boot-*.sh`; documenting a generated artifact as a key file). [VERIFIED]
- Hardcoded absolute path to `C:/tools/agent-comms/codex-wrap.py` inside `agent-runner.sh` (line 268) — breaks if repo not at that path. [VERIFIED]
- Dead-at-runtime modules: see list above. [VERIFIED imports]

---

## PHASE 4 — VERIFICATION SURFACE

**Test suite:** `python -m pytest tests/ --timeout=30`
- **RESULT: 98 passed, 0 failed, 0 error, 0 skipped, 0 xfail, 0 xpass** (run twice; ~1.4–1.7s). [VERIFIED]
- No `@pytest.mark.skip/xfail`, no `pytest.skip/xfail` anywhere. [VERIFIED]
- Per-file test counts: sqlite_transport 17, cell 12, board 10, beliefs 8, lifecycle 8, memory 8, jsonl_transport 6, mcp_tools 6, leases 5, dag 4, evolution 3, reputation 3, router 3, stall_detector 3, racing 2 = 98. [VERIFIED]
- No individual failures to itemize (input/expected/actual): there are none. [VERIFIED]

**Linter:** NONE configured (no ruff/flake8/pylint/black/isort config or pyproject section). Not run because none exists. [VERIFIED]

**Type-checker:** NONE configured (no mypy/pyright config). Code uses `from __future__`-style PEP 604 unions; never type-checked. [VERIFIED]

**Golden/conformance/snapshot checks:** NONE found. [VERIFIED]

**Compile check:** `python -m compileall hive tests dashboard codex-wrap.py` → all OK. `bash -n comms.sh agent-runner.sh dashboard/start.sh` → all OK. [VERIFIED]

**Behavior with NO test coverage (explicit):**
- `comms.sh` — no automated test exercises any `comms` subcommand. [VERIFIED] (no shell tests in repo)
- `agent-runner.sh` — untested. [VERIFIED]
- `codex-wrap.py` — untested (no `test_codex*`). [VERIFIED]
- `dashboard/server.py` — untested (no `test_*dashboard*`/`test_server*`). [VERIFIED]
- `hive/mcp/server.py` transport loop (`_read_message`/`_write_message`/`run_server`) — `test_mcp_tools.py` covers `tools.py` (`get_tool_definitions`/`execute_tool`) only; the JSON-RPC stdio framing is uncovered. [INFERRED from test file names + tools-only imports]
- The PROTOCOL.md mandatory cell fields `msg`, `status`, `context_id` — **no test references a top-level `msg`/`status`/`context_id`**; `test_jsonl_transport` uses `type="msg"` (a type value) and `test_beliefs`/`test_lifecycle` use `data["status"]` (inside payload). The documented "7-state lifecycle" / non-empty-`msg` rule is untested and unmodeled. [VERIFIED]
- `cell_from_dict` requires `d["from"]`/`d["type"]`/`d["ts"]`/`d["channel"]`/`d["id"]`; ingesting a `comms.sh`-written cell (which has those) works, but no test ingests a bash-written record. [INFERRED]

---

## PHASE 5 — CONFLICT LEDGER

1. **Version identity — 6 disagreeing values.** `manifest.json` `"protocol":"agent-comms/2.0"` (T3) vs `PROTOCOL.md` "HIVE Fleet Protocol v1.2" (T4) vs `pyproject.toml` `version="1.0.0"` (T3) vs `hive/__init__.py` `__version__="1.1.0"` (T3 code) vs `docs/plans` "HIVE Protocol v1.0 … APPROVED" (T4) vs `dashboard/server.py` `version="1.0.0"` (T3 code). **Winner by authority: none is enforced; the running code asserts `1.1.0` (hive) / `1.0.0` (pyproject).** Conflict resolved only at Tier 3-code → repo has no single ground-truth version. [VERIFIED]

2. **Cell schema: `msg` field. CORE CONFLICT.** `manifest.json` message_schema (T3) and `PROTOCOL.md` (T4, "Empty msg field = protocol violation") and `README.md` (T4) all require a top-level **`msg`**. The `hive` `Cell` dataclass, `cell_to_dict`, and the SQLite DDL (T3 code, runtime-enforced) have **no `msg` column/field at all**. `comms.sh`'s reader prints `m.get('msg','')`. Therefore any cell written by the Python package into the shared `channels/*.jsonl` displays blank in `comms read`, despite README/board docstring claiming JSONL is "backward compatible with existing comms.sh readers." **Winner by authority: the SQLite DDL (Tier 3, runtime-enforced) — `msg` does not exist in the real system; manifest/PROTOCOL/README are wrong.** [VERIFIED]

3. **Cell schema: `status` / `context_id`.** `PROTOCOL.md` (T4) mandates `status` and `context_id` on task/result/etc. cells and a 7-state lifecycle (SUBMITTED→…→VERIFIED). The DDL/`Cell` (T3 code) have neither field; the only "lifecycle" in code is convenience constructors + `dag`/`stall_detector` heuristics; no state-machine enforcement exists. **Winner: code (T3). PROTOCOL.md's lifecycle is aspirational prose with no enforcement or test.** [VERIFIED]

4. **Cell schema: extra fields.** Code/DDL define `v, refs, ttl, tags, sig` (T3 code). `manifest.json`/`README` message_schema (T3/T4) omit all five. **Winner: code (T3, runtime).** Manifest is an incomplete contract. [VERIFIED]

5. **Task cell shape.** `PROTOCOL.md` task cell (T4): `data.{for_agent,depends_on,parts,skills_required}` + top-level `status`,`context_id`. `HiveBoard.task()` (T3 code): `data.{title,spec,bounty,race,auto_assign}` + optional `deadline,quality_gates`. Disjoint. **Winner: code (T3, the only thing that runs/tests).** [VERIFIED]

6. **`agent-runner.sh` exits on unset/`unknown` `COMMS_AGENT`.** Root `AGENTS.md` & `FLEET-OPS`-derived "Never-Again Rule" (T4/T5): "agent-runner.sh exits immediately otherwise." Code (T3): line 27 `COMMS_AGENT="${COMMS_AGENT:-unknown/runner}"` then proceeds; the only `exit 1` is for a missing channel arg. **Winner: code — the doc's claimed guard does not exist.** Conflict winner is code over Tier 4/5 → no real enforcement of identity. [VERIFIED]

7. **`agent-runner.sh` rejects `msg` < 20 chars.** `AGENTS.md` (T4): "Never post a cell with msg shorter than 20 characters … `agent-runner.sh` rejects them." Code (T3): no such rejection; only a non-blocking `log "WARNING …(< 30)"` on agent *output* length (lines 384-385) that *appends a warning string and posts anyway*. **Winner: code — no rejection exists; threshold/target in doc (20-char msg) does not match code (30-char output, warn-only).** [VERIFIED]

8. **`comms.sh` command set vs docs.** `README.md` (T4) lists `handoff/ack`; `PROTOCOL.md` (T4) lists `clock-in/clock-out/roster/hire/task-ref/expire/trace/belief/refute`; `standards.md` (T4) lists `clock-in/clock-out/phone-home`. `manifest.json` `message_schema.type` enum (T3) is `task|status|result|error|handoff|ack|phone-home|clock-in|clock-out` — but `PROTOCOL.md` mandates a `status` field absent from that enum and absent from code. Multiple T4 docs enumerate different "real commands"; not verified against `comms.sh`'s actual `case` arms in this audit ([UNKNOWN] for full reconciliation). The three docs disagree among themselves; **no Tier ≤3 source defines the command set → no ground truth for the CLI surface.** [VERIFIED docs disagree] / [UNKNOWN exact comms.sh arm list]

9. **Python version.** `docs/plans` (T4) "Python 3.13 (stdlib only)"; `pyproject.toml` (T3) `requires-python=">=3.11"`. **Winner: pyproject (T3).** Minor. [VERIFIED]

10. **Test count.** Root `AGENTS.md` (T4) "Full test suite — 98 tests" — matches actual 98 (T2). **No conflict** (recorded for completeness). `PROTOCOL.md` result-cell example shows `tests_passed:248` (illustrative, not a claim about this repo). [VERIFIED]

11. **Hardcoded environment paths vs portability claim.** `docs/plans` "Key constraints: Windows 11 + Git Bash" (T4) is honest, but `dashboard/server.py` (T3 code) hardcodes `C:/Users/Brady.EAGLE/.ai/channels` and `C:/tools/agent-comms/hive.db`; `comms.sh`/`agent-runner.sh` hardcode `C:/tools/agent-comms`; `agent-runner.sh` hardcodes `C:/tools/agent-comms/codex-wrap.py`. Root `AGENTS.md` says CHANNELS_DIR is `C:/Users/Brady.EAGLE/.ai/channels` "(canonical — NOT `channels/` in this repo)" yet `.mcp.json` passes `--channels C:/tools/agent-comms/channels` and the gitignored repo `channels/` exists. **Channels directory has 3 different "canonical" locations across `AGENTS.md`, `.mcp.json`, and `dashboard/server.py` (all Tier 3/4). No single ground truth.** [VERIFIED]

12. **`org.json` byte-order mark.** `org.json` (T3) begins with a UTF-8 BOM (U+FEFF) before `{`. Strict JSON parsers reject a leading BOM. No code reads `org.json` in this audit ([UNKNOWN] consumer), so impact unconfirmed, but the file is a malformed-per-RFC8259 declarative artifact. [VERIFIED BOM present] / [UNKNOWN consumer]

**Conflicts whose only "winner" is Tier 4/5 (i.e. NO real ground truth):** #1 (version), #6 (identity guard), #7 (msg-length rule), #8 (CLI command set), #11 (channels location). These five areas have no enforced contract — documentation asserts behavior the code does not implement, and nothing tests them.

---

## PHASE 6 — GAP LIST

1. No CI/CD; no pipeline runs the 98 tests. [VERIFIED]
2. No pre-commit config; no custom git hooks (only `.git/hooks/*.sample`). [VERIFIED]
3. No linter configured (ruff/flake8/pylint/black/isort absent). [VERIFIED]
4. No type-checker configured (mypy/pyright absent); code is fully unchecked. [VERIFIED]
5. No JSON Schema/OpenAPI/protobuf/formal schema file; cell contract exists only as Python DDL + prose. [VERIFIED]
6. `msg` field required by `manifest.json`, `PROTOCOL.md`, `README.md`; absent from `Cell`, `cell_to_dict`, and SQLite DDL. [VERIFIED]
7. `status` and `context_id` required by `PROTOCOL.md`; absent from code/DDL; no state-machine enforcement of the documented 7-state lifecycle. [VERIFIED]
8. `manifest.json`/`README` message_schema omit code fields `v,refs,ttl,tags,sig`. [VERIFIED]
9. Six conflicting version strings (2.0 / v1.2 / 1.0.0 pyproject / 1.1.0 hive / v1.0 plan / 1.0.0 dashboard). [VERIFIED]
10. `AGENTS.md` claim "agent-runner.sh exits immediately on unset/unknown COMMS_AGENT" is false; code defaults to `unknown/runner` and proceeds. [VERIFIED]
11. `AGENTS.md` claim "agent-runner.sh rejects msg < 20 chars" is false; only a warn-only check on agent output `< 30` chars exists. [VERIFIED]
12. `.boot-gemini-backfill.sh` listed in `AGENTS.md` Key Files but does not exist. [VERIFIED]
13. Hardcoded `C:/tools/agent-comms/codex-wrap.py` path in `agent-runner.sh:268`. [VERIFIED]
14. Hardcoded Windows paths in `dashboard/server.py` (`CHANNELS_DIR`, `DB_PATH`), `comms.sh` (`COMMS_DIR`, `CHANNELS_DIR`), `agent-runner.sh` (`COMMS_DIR`, `CHANNELS_DIR`). [VERIFIED]
15. Channels directory has 3 inconsistent "canonical" locations (`AGENTS.md` user `.ai/channels`, `.mcp.json` repo `channels`, repo `channels/` gitignored). [VERIFIED]
16. JSONL projection is not actually backward-compatible: hive-written lines lack `msg`, so `comms read` shows blank for them. [VERIFIED]
17. `coordination/{dag,evolution,leases,racing,reputation,router,stall_detector}` unreachable from any runtime entry point (MCP/comms.sh); test-only. [VERIFIED imports]
18. `comms.sh` has zero automated test coverage. [VERIFIED]
19. `agent-runner.sh` has zero automated test coverage. [VERIFIED]
20. `codex-wrap.py` has zero automated test coverage. [VERIFIED]
21. `dashboard/server.py` has zero automated test coverage. [VERIFIED]
22. `hive/mcp/server.py` JSON-RPC stdio loop (`_read_message`/`_write_message`/`run_server`) uncovered; only `tools.py` is tested. [INFERRED]
23. `org.json` begins with a UTF-8 BOM (invalid per RFC 8259 for strict parsers). [VERIFIED]
24. `COMMS_AGENT` defaults to `unknown` (`comms.sh`) / `unknown/runner` (`agent-runner.sh`) despite docs forbidding `unknown` identity. [VERIFIED]
25. Dashboard requires `fastapi`+`uvicorn` not declared in `pyproject.toml` (only in `dashboard/requirements.txt`); `start.sh` pip-installs at runtime. [VERIFIED]
26. Three Tier-4 docs (`README`, `PROTOCOL.md`, `standards.md`) enumerate mutually different "real" `comms` command sets. [VERIFIED]
27. `pyproject.toml` declares no build-system/dependencies; package is import-from-cwd only, not pip-installable as named `hive-protocol`. [VERIFIED]
28. Docs/plans state Python 3.13; `pyproject.toml` requires `>=3.11`. [VERIFIED]

--- END OF AUDIT ---
