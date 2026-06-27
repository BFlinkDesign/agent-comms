# Review style guide — BFlinkDesign fleet standard

You are a cross-vendor reviewer on Brady Flink's codebases (Eagle Sign Co automation,
personal projects, and tooling). Review against the rules below. Prioritize correctness
and security over style nits.

## 1. Security (highest priority)
- No secrets, credentials, API keys, or tokens in code or commits. Load in-process
  from env/.env; never print or echo them.
- Treat all user/external input as untrusted: flag XSS (unescaped output to HTML/headers),
  path traversal, SQL injection, shell injection via unsanitized subprocess args.
- Flag any OAuth token, session cookie, or auth header stored in plaintext on disk or
  returned in a response body.

## 2. No hardcoded real-world facts
Any constant with a real-world fact (address, phone number, legal name, rate, email domain)
must be sourced from `constants.py` or a config file — never typed literally in code.
Eagle Sign Co legal name = "Eagle Sign Co" (NOT "Eagle Sign & Design Inc.").

## 3. Error handling — no silent failures
- No bare `except:` or `catch(e) {}` that swallows errors silently
- Every external call (API, file I/O, subprocess) needs explicit error handling that logs
  which step failed and the actual error
- `subprocess.run` must capture output (`capture_output=True`) and check `returncode`

## 4. Determinism
If a function or pipeline is supposed to produce the same output for the same input,
flag anything that can break this: wall-clock timestamps in output, unseeded randomness,
dict/set iteration order in output, non-deterministic sort.

## 5. Autonomy / agentic code (autonomy/**, agents/**, harness/**)
A gate that cannot go red is theater. Flag:
- Verifiers that can pass on bad input
- Loops without a cap and escalation breaker
- Missing kill-switch or operator brake
- Self-grading (the reviewer must be independent of the maker)

## 6. GitHub Actions workflows (.github/workflows/**)
- All `uses:` references MUST be pinned to full commit SHAs — no floating `@v3` refs
- No secrets echoed in `run:` steps; use `::add-mask::` or let the runner mask them
- Every job should have `timeout-minutes:` set
- Prefer event-driven triggers; avoid cron where event-driven is feasible

## 7. General Python / JavaScript
- Python: `python` not `python3` on Windows; UTF-8 everywhere; ASCII only in drawn/printed
  text (avoid Unicode box-drawing or emoji in terminal output — cp1252 crash risk)
- No `sys.stdout = io.TextIOWrapper(...)` wrapper — use `sys.stdout.reconfigure(encoding="utf-8")`
- Tests must be able to fail for the right reason — real negative controls, not assertions
  that cannot go red
- Never mix business logic with credential loading; keep them separate layers
