#!/usr/bin/env python3
"""
codex-wrap.py -- Wrapper for codex CLI invoked by agent-runner.sh

Reads a task prompt from stdin, runs `codex "<prompt>"`, captures
stdout+stderr, strips progress bars and ANSI noise, then prints a
clean formatted result summary to stdout.

Usage (called by agent-runner.sh):
    echo "run pytest and report results" | python C:/tools/agent-comms/codex-wrap.py

Exit codes:
    0 -- codex completed (output always produced, even on codex errors)
    Non-zero -- wrapper itself failed (e.g. codex not found, stdin unreadable)
"""

import re
import subprocess
import sys

# ---------------------------------------------------------------------------
# Output cleaning
# ---------------------------------------------------------------------------

# ANSI escape codes (color, cursor movement, etc.)
ANSI_ESCAPE = re.compile(r"\x1b\[[0-9;]*[mABCDEFGHJKSTfhilmnprsu]")

# Progress bar patterns common in codex CLI output:
#   [=====>    ] 47%
#   Downloading: 100%|####| 12.3M/12.3M
#   spinner chars on their own line: \ | / -
PROGRESS_BAR = re.compile(
    r"("
    r"\[=*>?\s*\]\s*\d+%"          # [====>   ] 47%
    r"|Downloading.*\d+%.*"        # Downloading: 100%|####|...
    r"|\d+%\|[#=\-]+\|.*"          # 47%|####| 12.3/12.3M
    r"|^\s*[\\|/\-]\s*$"           # bare spinner char
    r"|\r[^\n]*"                   # carriage-return overwrites (inline progress)
    r")",
    re.IGNORECASE,
)

# Lines that are just whitespace or single non-printable chars
BLANK_OR_NOISE = re.compile(r"^\s*$")


def clean_output(raw: str) -> str:
    """Strip ANSI codes, progress bars, and noise from codex output."""
    # Remove ANSI escapes first
    text = ANSI_ESCAPE.sub("", raw)

    # Split on \r\n or \n uniformly
    lines = text.replace("\r\n", "\n").replace("\r", "\n").split("\n")

    clean_lines = []
    for line in lines:
        # Strip progress bar patterns within the line
        line = PROGRESS_BAR.sub("", line).rstrip()
        # Keep non-blank lines
        if not BLANK_OR_NOISE.match(line):
            clean_lines.append(line)

    return "\n".join(clean_lines)


# ---------------------------------------------------------------------------
# Test count extraction
# ---------------------------------------------------------------------------

def extract_test_counts(text: str) -> str | None:
    """
    Try to pull pytest/unittest summary from output.
    Returns a short string like "47 passed, 3 failed, 1 error" or None.
    """
    # pytest short summary: "47 passed in 3.2s" or "3 failed, 44 passed in 2.1s"
    # Also handles: "FAILED", "ERROR", "passed", "failed", "error", "warning"
    patterns = [
        # pytest: "N passed", "N failed", "N error", etc. on the summary line
        re.compile(
            r"((?:\d+\s+(?:passed|failed|error|warning|skipped)"
            r"(?:,\s*)?)+(?:\s+in\s+[\d.]+s)?)",
            re.IGNORECASE,
        ),
        # unittest: "Ran N tests in Xs"
        re.compile(r"Ran\s+(\d+)\s+tests?\s+in\s+[\d.]+s", re.IGNORECASE),
        # OK / FAILED short forms
        re.compile(r"^(OK|FAILED)\s*(\(.*?\))?", re.MULTILINE | re.IGNORECASE),
    ]

    for pat in patterns:
        m = pat.search(text)
        if m:
            return m.group(0).strip()

    return None


# ---------------------------------------------------------------------------
# Result formatter
# ---------------------------------------------------------------------------

def format_result(prompt: str, raw_output: str, returncode: int) -> str:
    """
    Produce a clean, channel-ready result summary.

    Format:
        [codex-wrap] STATUS | <what ran> | <test counts or output excerpt> | <exit code>
    """
    cleaned = clean_output(raw_output)

    status = "OK" if returncode == 0 else f"EXIT {returncode}"

    # Try to pull test counts for a compact summary line
    test_summary = extract_test_counts(cleaned)

    # Build the prompt excerpt (first 80 chars of prompt, stripped of newlines)
    prompt_excerpt = prompt.replace("\n", " ").strip()[:80]
    if len(prompt.strip()) > 80:
        prompt_excerpt += "..."

    # Output excerpt: last 600 chars of cleaned output (most useful for result cells)
    if len(cleaned) > 600:
        output_excerpt = "[...truncated...]\n" + cleaned[-600:]
    else:
        output_excerpt = cleaned if cleaned else "(no output)"

    lines = [
        f"[codex-wrap] {status} | prompt: {prompt_excerpt}",
    ]
    if test_summary:
        lines.append(f"[codex-wrap] test counts: {test_summary}")
    lines.append("[codex-wrap] output:")
    lines.append(output_excerpt)

    return "\n".join(lines)


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

def main() -> int:
    # Read prompt from stdin
    try:
        prompt = sys.stdin.read().strip()
    except Exception as e:
        print(f"[codex-wrap] ERROR: could not read prompt from stdin: {e}", file=sys.stderr)
        return 1

    if not prompt:
        print("[codex-wrap] ERROR: empty prompt on stdin — nothing to run", file=sys.stderr)
        return 1

    # Invoke codex
    try:
        proc = subprocess.run(
            ["codex", prompt],
            capture_output=True,
            text=True,
            encoding="utf-8",
            errors="replace",
            timeout=300,  # 5-minute hard cap
        )
        raw = proc.stdout + proc.stderr
        returncode = proc.returncode
    except FileNotFoundError:
        raw = "[codex-wrap] ERROR: 'codex' binary not found in PATH"
        returncode = 127
    except subprocess.TimeoutExpired:
        raw = "[codex-wrap] ERROR: codex timed out after 300s"
        returncode = 124
    except Exception as e:
        raw = f"[codex-wrap] ERROR: codex invocation failed: {e}"
        returncode = 1

    # Format and print clean result
    result = format_result(prompt, raw, returncode)
    print(result)
    return 0


if __name__ == "__main__":
    sys.exit(main())
