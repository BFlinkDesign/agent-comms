#!/usr/bin/env python3
"""Validated JSONL cell writer for shell scripts.

Called by comms.sh and agent-runner.sh instead of inline Python open() calls.
Validates the channel name, guards against path traversal and symlink redirection,
then writes one cell with O_NOFOLLOW on Linux/macOS to prevent TOCTOU races.

Usage:
    python shell_write.py <agent> <channel> <type> <msg> <data_json> <channels_dir>
Stdout: the generated cell ID.
Exit 1: validation failure or write error.
Exit 2: wrong number of arguments.
"""
from __future__ import annotations

import datetime
import json
import os
import re
import sys
import uuid

_CHANNEL_RE = re.compile(r"^[a-z0-9][a-z0-9_-]{0,63}$")


def main(argv: list[str]) -> int:
    if len(argv) != 7:
        print(
            "usage: shell_write.py <agent> <channel> <type> <msg> <data_json> <channels_dir>",
            file=sys.stderr,
        )
        return 2

    _, agent, channel, type_, msg, data_s, channels_dir = argv

    if not _CHANNEL_RE.match(channel):
        print(f"ERROR: invalid channel name: {channel!r}", file=sys.stderr)
        return 1

    # Resolve channels dir and build candidate path, then verify no traversal occurred.
    safe_dir = os.path.realpath(channels_dir)
    candidate = os.path.normpath(os.path.join(safe_dir, f"{channel}.jsonl"))
    expected = os.path.join(safe_dir, f"{channel}.jsonl")
    if candidate != expected:
        print("ERROR: path traversal detected", file=sys.stderr)
        return 1

    # Symlink check (defence-in-depth; O_NOFOLLOW below is the hard guard).
    if os.path.islink(candidate):
        print(f"ERROR: refusing to write — {candidate!r} is a symlink", file=sys.stderr)
        return 1

    try:
        data: object = json.loads(data_s)
    except json.JSONDecodeError as e:
        data = {"_parse_error": str(e), "_raw": data_s}

    cell_id = str(uuid.uuid4())
    obj = {
        "id": cell_id,
        "from": agent,
        "ts": datetime.datetime.now().astimezone().isoformat(),
        "channel": channel,
        "type": type_,
        "msg": msg,
        "data": data,
    }
    line = json.dumps(obj, ensure_ascii=False)

    os.makedirs(safe_dir, exist_ok=True)
    flags = os.O_WRONLY | os.O_CREAT | os.O_APPEND
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW

    try:
        fd = os.open(candidate, flags, 0o666)
        with os.fdopen(fd, "a", encoding="utf-8") as f:
            f.write(line + "\n")
    except OSError as e:
        print(f"ERROR: write failed: {e}", file=sys.stderr)
        return 1

    print(cell_id)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
