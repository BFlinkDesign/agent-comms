# HIVE Gap-Fit Analysis

Date: 2026-05-21

## Purpose

This document captures the approved gap-fit analysis sprint for the HIVE /
agent-comms repository. It compares the current repository against current
agent-system best practices and identifies the work needed to move from a
trusted local coordination experiment to a robust, portable, agent-native
control plane.

The analysis used four lenses:

1. Current repository reality
2. 2026 standards and best practices
3. Architecture and protocol alignment
4. Security threat model

## Executive summary

HIVE already has strong foundations: immutable-ish content-addressed cells,
SQLite plus JSONL persistence, coordination modules, MCP tools, a FastAPI
dashboard, and a broad Python test suite. The main issue is that the system is
not yet one coherent protocol surface.

Two write paths coexist:

- `comms.sh` and `agent-runner.sh` write legacy UUID JSONL records directly.
- `HiveBoard` writes HIVE cells to SQLite and projects them to JSONL.

The docs describe an A2A-inspired public task lifecycle, but the shell runner,
dashboard, MCP server, and HIVE Python layer do not yet enforce one shared
schema, lifecycle, path config, validation model, or security boundary.

The highest-risk gaps are:

1. Split write paths and split schemas
2. Missing write-time validation
3. Missing dependency enforcement in the runner
4. Unauthenticated control-plane writes and identity spoofing
5. Path traversal and dashboard exposure risks
6. MCP/A2A/OpenAPI/observability gaps against current standards
7. Hardcoded Windows paths and weak package/CI reproducibility

## Recommended follow-up implementation/review roster

| Agent role | Use for | Suggested subagent / fleet role |
| --- | --- | --- |
| Repo research | Map current code/docs/tests and stale assumptions | `explore`, `ce-repo-research-analyst`, `gemini/researcher` |
| Standards research | Track MCP, A2A, OTel GenAI, OpenAPI/Arazzo, AsyncAPI, packaging, FastAPI security | `ce-best-practices-researcher`, `ce-web-researcher` |
| Protocol alignment architect | Decide canonical schema, lifecycle, adapters, compatibility matrix | `ce-architecture-strategist`, `ai-architect`, `claude/architect` |
| Security threat model | Identity, ACLs, tool poisoning, path traversal, dashboard auth, runner execution gating | `ce-security-sentinel`, `ce-security-reviewer` |
| API contract designer | OpenAPI, Arazzo workflows, AsyncAPI channel docs, `llms.txt`, stable error contracts | `API Readiness Analyzer`, `ce-api-contract-reviewer` |
| Reliability reviewer | JSONL append semantics, SQLite projection, leases, replay, compaction, idempotency | `ce-reliability-reviewer`, `ce-data-integrity-guardian` |
| Testing / CI / deployer | Lockfiles, CI, clean install, dashboard/CLI/MCP smoke tests | `codex/deployer`, `ce-testing-reviewer`, `ci-watcher` |
| Docs / knowledge refresh | README, protocol docs, role prompts, setup docs, current best-practice references | `docs-researcher`, `gemini/researcher` |
| Orchestration | Agent-card publication, fleet roster policy, routing adoption | `openclaw/orchestrator`, `claude/architect` |

## Gap-fit matrix

Priorities are grouped by urgency, not by one-PR scope. P0 is split into
execution bands below so the first implementation step does not accidentally
bundle protocol migration, auth, and dashboard exposure into one change.

- **P0a**: path/config alignment, channel validation, and safe live-path gates
- **P0b**: canonical schema/profile, unified writer, legacy import, and repair
- **P0c**: identity/signature, ACLs, trusted runner execution, and sandboxing
- **P1**: lifecycle/dashboard/API/MCP modernization after P0 decisions
- **P2**: interoperability specs, docs restructure, release hardening

| Priority | Current state | Target state | Gap | First fix | Suggested implementers/reviewers |
| --- | --- | --- | --- | --- | --- |
| P0a | `.mcp.json`, dashboard, comms, runner, and docs disagree on channel paths | Single env-first config for channels, DB, host, and port | MCP can write/read a repo channel directory while live fleet uses canonical `.ai/channels` | Centralize config: `HIVE_CHANNELS_DIR`, `HIVE_DB_PATH`, `HIVE_HOST`, `HIVE_PORT`; fail startup if components disagree; keep MCP write tools read-only/disabled until trust gates exist | `codex/deployer`, `ce-security-sentinel` |
| P0a | Channel names are concatenated into paths in JSONL transport, dashboard, and shell scripts | Channel IDs validated at ingress and resolved under a canonical root | Path traversal can read/write outside channel dir; validation in projection alone would be too late | Enforce `^[a-z0-9][a-z0-9_-]{0,63}$` before any transport write; verify resolved path remains under channels dir; dashboard validation is defensive read-side validation only | `ce-security-sentinel`, `codex/deployer` |
| P0a | Any local writer/MCP client can forge `from_agent`, claims, cards, feedback, and results | Shared-path rollout cannot grant new write reach before trust controls exist | Aligning MCP to the live path before gating writes could expand blast radius | Add a temporary live-path safety gate: MCP write tools require trusted identity or remain disabled/read-only; runners ignore unsigned/untrusted executable tasks | `ce-security-sentinel`, `claude/architect` |
| P0b | `comms.sh` and `agent-runner.sh` write UUID JSONL directly; `HiveBoard` writes HIVE cells to SQLite plus JSONL | One canonical write path with SQLite as queryable source and JSONL as projection/import format | Shell traffic bypasses HIVE, so SQLite, MCP, and dashboard can split from live fleet state | Route shell and runner writes through a shared Python writer only after legacy import/replay and projection repair exist | `claude/architect`, `codex/deployer` |
| P0b | Legacy JSONL uses top-level `msg`; HIVE JSONL projection uses cell fields without top-level `msg` | One versioned HIVE envelope plus compatibility fields for existing readers | Mixed channels can break dashboard/comms readability and task correlation | Define a Task Protocol Profile; add compatibility mapping for `msg`, `refs`, `context_id`, and `depends_on` | `claude/architect` |
| P0b | Write paths accept malformed or short messages; docs say short cells are rejected | Layered validation that respects both core HIVE cells and message/task profiles | A naive universal `msg` validator would reject valid structured HIVE cells | Split validation into core Cell validation and Task Protocol Profile validation; writer validates dependency field shape/resolvability, while runner/lifecycle enforces readiness | `claude/architect`, `codex/deployer` |
| P0b | Runner claims tasks by JSONL append order and does not inspect dependencies | Runner only claims ready tasks and emits blocked status for unmet deps | Current DAG helper only sees `task -> contract -> result`, not legacy direct `result.data.task_id` | Build lifecycle/readiness reducer covering direct result refs, legacy task IDs, and HIVE contract results before wiring runner claims | `claude/architect`, `codex/deployer` |
| P0b | JSONL projection is write-only and dual-write is sequential, not transactional | Replay/repair/projector tooling with idempotency before unified writer migration | Partial writes and duplicate projections can increase during migration | Promote DB-vs-JSONL repair, import, and dedupe into the unified-writer phase; test SQLite-write/JSONL-fail and JSONL-existing/SQLite-empty | `ce-reliability-reviewer`, `codex/deployer` |
| P0c | Any local writer/MCP client can forge `from_agent`, claims, cards, feedback, and results | Server-side identity with signing/auth and channel/type ACLs | Identity spoofing and routing/reputation poisoning | Write identity/authorization spec: key issuance/storage, signed fields, replay defense, rotation/revocation, and actor-by-transition matrix | `ce-security-sentinel`, `claude/architect` |
| P0c | Any channel writer can create a task executed by runners in high-autonomy modes | Signed task execution from trusted orchestrators only | JSONL write access becomes autonomous command/tool execution | Define runner execution policy: trusted task verification, allowed tools, approval gates, isolated workdir, env allowlist, network/time/resource limits, audit logs, fail-closed behavior | `ce-security-sentinel`, `codex/deployer` |
| P0c | HIVE IDs exclude `refs`, `tags`, `ttl`, and `sig`; reads do not recompute IDs | Signature and ID verification cover every load-bearing field | Attackers or import adapters can alter DAG/context/security fields without changing IDs | Decide Cell ID/signature v2 or require v1 adapters to include normalized refs/context in `data`; add collision and verification tests | `claude/architect`, `ce-security-sentinel` |
| P1 | `PROTOCOL.md` lifecycle is aspirational; runner/dashboard use `task -> claim -> result`; HIVE tests use `task -> contract -> result -> feedback` | Append-only lifecycle reducer with explicit state machine | No single authoritative task state; validation could freeze the wrong semantics | Add `task_state(task_id)` reducer and event-to-state test vectors before lifecycle transition validation | `claude/architect` |
| P1 | `context_id` exists in docs but is not operational | Context is a first-class workflow/session correlation ID | "Sprint", "session", "workflow", and "conversation" can mean different granularities | Define `context_id` as one workflow/session thread; sprints contain multiple contexts; map it to A2A context and OTel conversation IDs | `claude/architect`, `gemini/researcher` |
| P1 | Agent Cards exist as HIVE cells and docs, but no discovery/sync path publishes local cards | Agent capabilities are discoverable and routable | Router cannot rely on real agent capability data | Add `hive cards sync` from local cards/org metadata; publish card cells on clock-in | `openclaw/orchestrator`, `gemini/researcher` |
| P1 | Dashboard binds `0.0.0.0`, has wildcard CORS with credentials, no auth, and hardcoded Windows paths | Local-only default, explicit CORS, and required auth for non-loopback/tunnel exposure | Operational data leaks if exposed through LAN/tunnel | Bind `127.0.0.1` by default; use env-read config; fail startup for public bind without auth; no wildcard CORS with credentials | `ce-security-sentinel`, `codex/deployer` |
| P1 | Dashboard renders channel names through inline handlers and parses tasks with `TASK-N` text heuristics | Safe DOM rendering and structured task identifiers | Stored XSS and unreliable task display | Remove inline event handlers; use `textContent`/listeners; add CSP; key tasks on cell IDs/refs/data task IDs | `ce-security-sentinel`, `codex/deployer` |
| P1 | MCP server is hand-rolled, advertises an older protocol version, returns JSON text, and lacks output schemas/annotations | Current MCP tool contracts with structured output, annotations, pagination, modern transports | Lower interoperability and higher tool-poisoning risk | Add `outputSchema`, `structuredContent`, tool annotations, provenance labels, bounded limits; evaluate official MCP Python SDK/FastMCP | `claude/architect`, `gemini/researcher` |
| P1 | MCP tools return attacker-controlled cell content as text | Tool output separates trusted metadata from untrusted content | Tool-output poisoning can steer agents | Return `trusted_metadata` plus quoted `untrusted_content`; include source/channel/author/signature status; add adversarial MCP tests | `ce-security-sentinel`, `claude/architect` |
| P1 | FastAPI endpoints return untyped dicts/lists | Pydantic response models and stable OpenAPI contract | Weak agent/API consumer contract | Add response models, error schemas, examples, and stable operation IDs | `ce-api-contract-reviewer`, `claude/architect` |
| P1 | No OTel instrumentation for MCP, Board, runner, or dashboard | OTel GenAI/MCP-compatible spans, metrics, and correlation IDs | Poor replay, debugging, performance analysis, and eval feedback | Instrument board `put/get/query`, MCP `tools/call`, task lifecycle, runner claims/results; map `context_id` to conversation ID | `ce-reliability-reviewer`, `codex/deployer` |
| P1 | Tests cover Python core but not shell, runner, dashboard, config, or full integration | Test matrix covers core plus live ops paths | Highest-risk operational layer can regress silently | Add integration tests: comms write -> board import/query -> dashboard parse -> MCP query; add dashboard `TestClient` tests | `ce-testing-reviewer`, `codex/deployer` |
| P1 | No CI or reproducible dependency strategy | Clean install/build/test on supported Python versions with lockfile | Environment setup is manual and fragile | Add build backend, dependency groups/extras, lockfile, Ruff, type checking, and GitHub Actions | `codex/deployer` |
| P2 | Results have `artifacts: list[str]` but no artifact identity, MIME type, checksum, or retention | Structured artifact cells with lineage | Outputs are hard for agents/tools to consume reliably | Add `artifact` cells with `kind`, `uri`, `sha256`, `mime`, `size`, `summary`, refs to result/task | `claude/architect`, `codex/deployer` |
| P2 | Human-readable protocol docs only | OpenAPI, AsyncAPI, Arazzo workflows, and `llms.txt` | Agents cannot consume workflows/contracts mechanically | Publish OpenAPI for dashboard, AsyncAPI for channels, Arazzo task workflows, and `llms.txt` doc index | `API Readiness Analyzer`, `docs-researcher` |
| P2 | Docs are useful but drifted across README, PROTOCOL, AGENTS, FLEET-OPS, historical plans | Diataxis docs, ADRs, protocol compatibility table, current command reference | Agents follow stale or contradictory instructions | Restructure docs; mark historical plans clearly; regenerate MCP and CLI references from code | `docs-researcher`, `gemini/researcher` |
| P2 | Supply-chain posture is light; dashboard start installs latest deps | Locked build-time installation and release metadata | Unpinned runtime installs can pull unexpected packages | Pin via lockfile, install during setup, add package metadata and dependency audit | `codex/deployer` |

## Architecture decisions needed

These decisions should be made before broad implementation:

1. **Canonical protocol authority**
   - Recommended: HIVE Cell remains the canonical envelope.
   - Add a versioned Task Protocol Profile on top of Cell.
   - Keep legacy JSONL compatibility as an adapter, not as a peer protocol.

2. **Task lifecycle vocabulary**
   - Decide whether the primary public lifecycle is A2A-inspired
     `submitted/working/blocked/complete/failed/canceled/verified` or the
     existing HIVE `task/contract/result/feedback` flow.
   - Recommended: expose A2A-inspired lifecycle externally; keep contract/result
     events as internal or compatibility events when needed.

3. **Dependency representation**
   - Recommended: canonical dependencies are HIVE `refs`.
   - Legacy `data.depends_on` is accepted by adapters and normalized to refs.

4. **Context representation**
   - Recommended: store `data.context_id` and also tag cells with
     `context:<id>` for efficient query and compatibility with tag-based tools.
   - `context_id` should identify one workflow/session thread. A sprint or
     larger project contains multiple contexts. This maps naturally to A2A
     context IDs and to OpenTelemetry conversation/workflow correlation IDs.

5. **Identity and trust model**
   - Recommended: callers do not self-assert trusted identity for
     control-plane cells.
   - Use signatures or broker-mediated identity plus channel/type ACLs.
   - Before implementation, define key issuance/storage, signed fields,
     timestamp/nonce replay defense, rotation/revocation, and an
     actor-by-transition matrix for submit, claim, block, complete, verify,
     cancel, and card publication.

6. **MCP strategy**
   - Decide whether zero-dependency MCP remains a hard constraint.
   - If not, evaluate the official MCP Python SDK / FastMCP for compliance,
     schemas, annotations, and transport support.

7. **Signed envelope and cell identity**
   - Decide whether to introduce Cell ID/signature v2 that hashes and verifies
     every load-bearing field: `type`, `from`, `ts`, `channel`, `data`, `refs`,
     `tags`, `ttl`, and signature metadata.
   - If Cell v1 remains, adapters must include normalized dependency/context
     fields inside `data` so refs/tags changes do not collide under the current
     ID scheme.

8. **Secrets and execution policy**
   - Define how HMAC keys, dashboard tokens, and MCP credentials are loaded,
     rotated, redacted, and separated by environment.
   - Define runner execution boundaries: tool allowlists, dangerous-action
     approval, workdir ownership/cleanup, environment allowlist, network
     policy, time/resource limits, audit logging, and fail-closed behavior.

## Suggested implementation sequence

1. **P0a: Config, path alignment, and safe live-path gates**
   - Add shared env-first settings.
   - Align `.mcp.json`, dashboard, comms, runner, MCP defaults, JSONL
     transport, and HiveBoard around those settings.
   - Keep MCP write tools read-only/disabled on the live bus until the minimal
     trust gate exists.
   - Add startup checks that detect split-brain config.

2. **Decision gate: task lifecycle and legacy mapping**
   - Approve the canonical Task Protocol Profile.
   - Approve legacy UUID JSONL -> HIVE cell mapping.
   - Approve `context_id`, dependency, and artifact compatibility rules.

3. **P0b: Canonical schema, compatibility matrix, and lifecycle reducer**
   - Define the Task Protocol Profile.
   - Map legacy JSONL to HIVE cells.
   - Add the lifecycle/readiness reducer before enforcing lifecycle transitions.
   - Document what is normative vs historical.

4. **P0b: Unified writer, import, repair, and layered validation**
   - Add legacy JSONL import/replay and DB-vs-JSONL repair/dedupe first.
   - Route shell/runner writes through a shared Python writer after import and
     repair paths exist.
   - Enforce core Cell validation separately from Task Protocol Profile
     validation. Writer validates dependency shape/resolvability; runner and
     lifecycle reducer enforce readiness and emit `blocked`.

5. **Decision gate: identity, signing, and runner trust boundary**
   - Approve the signed-envelope design.
   - Approve channel/type ACLs and actor-by-transition matrix.
   - Approve runner execution policy before accepting live executable tasks
     from newly aligned write paths.

6. **P0c: Runner safety and dependency enforcement**
   - Require trusted/signed executable tasks.
   - Add ready/blocked handling.
   - Preserve context IDs and refs on claim/status/result.

7. **Dashboard hardening and structured task model**
   - Env settings, localhost default, required auth for non-loopback bind, safe
     DOM rendering, and CSP.
   - Replace `TASK-N` heuristics with lifecycle reducer output.

8. **Decision gate: MCP SDK/FastMCP**
   - Decide whether the zero-dependency MCP implementation remains a
     requirement or whether SDK/FastMCP compatibility justifies a dependency.

9. **MCP contract modernization**
   - Add output schemas, structured content, annotations, limits, provenance,
     and conformance smoke tests.

10. **Packaging, CI, and test matrix**
   - Complete `pyproject.toml`.
   - Add lock/dependency groups.
   - Add tests for CLI, runner, dashboard, MCP, and integration flow.

11. **Observability and agent-ready specs**
   - OTel spans/metrics.
   - OpenAPI, AsyncAPI, Arazzo, and `llms.txt`.

12. **Docs and knowledge refresh**
   - Diataxis docs.
   - ADRs.
   - Current command reference and protocol compatibility table.

## External standards to track

- MCP 2025-06-18+ lifecycle, tools, transports, authorization, structured
  outputs, annotations, and resources
- A2A Agent Cards, Tasks, Messages, Parts, Artifacts, context IDs, streaming,
  push notifications, and task cancellation/subscription
- OpenTelemetry GenAI and MCP semantic conventions
- OpenAPI 3.2 for REST contracts
- AsyncAPI 3.0 for channel/event contracts
- Arazzo 1.1 for multi-step API workflows
- Diataxis documentation structure
- `llms.txt` for agent-readable documentation indexes
- OWASP MCP tool-poisoning mitigations

## Implementation status (updated 2026-05-22)

See `docs/PROJECT_STATE.md` for the live project snapshot.

### Completed on branch `cursor/gap-fit-analysis-2e94`

| Item | Status |
| --- | --- |
| P0a shared config (`HIVE_CHANNELS_DIR`, `HIVE_DB_PATH`, dashboard host/port) | Done |
| P0a channel name validation at ingress | Done |
| P0a path traversal / symlink guards | Done |
| P0a dashboard localhost default + token auth for non-loopback | Done |
| P0a runtime hardening tests | Done (28 tests) |
| P0b lifecycle/readiness reducer | Done (`hive.coordination.lifecycle`) |
| P0b DAG wired to lifecycle reducer | Done |
| P0b runner `depends_on` gate (JSONL) | Done |
| P0b lifecycle reducer tests | Done (19 tests) |

**Test count:** 145 passing.

### Still open (ranked)

1. P0b — Legacy JSONL import/repair and dedupe
2. P0b — Unified Python writer for `comms.sh` and `agent-runner.sh`
3. P0b — Task Protocol Profile and layered write validation
4. P0c — MCP write gate, identity/signing, runner trust boundary
5. P1 — Dashboard structured task model via lifecycle reducer
6. P1 — MCP output schemas and tool-poisoning mitigations
7. P1 — CI, lockfile, integration test matrix

### Original first PR scope (historical)

The first implementation PR was intentionally narrow and did not migrate the
full write path. That scope is now **complete** except MCP write gating (still
open under P0c).
