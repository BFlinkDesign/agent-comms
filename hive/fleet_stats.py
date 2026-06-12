"""Fleet statistics from channel JSONL files.

Standalone stdlib-only script (no hive imports) backing `comms perf` and
`comms fire` -- one scanner instead of two divergent inline-python copies
in comms.sh.

Usage:
  python fleet_stats.py perf <channels_dir> [agent_filter]
  python fleet_stats.py fire-report <channels_dir> <agent_id>
  python fleet_stats.py fire-json <channels_dir> <agent_id>
"""
import glob
import json
import os
import sys
from typing import Any


def _blank() -> dict[str, Any]:
    return {
        "tasks": 0,
        "results": 0,
        "errors": 0,
        "handoffs_sent": 0,
        "handoffs_recv": 0,
        "phone_homes": 0,
        "first_seen": "",
        "last_seen": "",
        "channels": set(),
        "messages": [],
    }


_TYPE_COUNTERS = {
    "task": "tasks",
    "result": "results",
    "error": "errors",
    "handoff": "handoffs_sent",
    "ack": "handoffs_recv",
    "phone-home": "phone_homes",
}


def collect(channels_dir: str, agent_filter: str = "") -> dict[str, dict[str, Any]]:
    """Per-agent activity stats across all channels.

    agent_filter is a substring match on the cell's `from` field
    (matching the historical comms.sh behavior).
    """
    stats: dict[str, dict[str, Any]] = {}
    for f in sorted(glob.glob(os.path.join(channels_dir, "*.jsonl"))):
        with open(f, encoding="utf-8") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    m = json.loads(line)
                except json.JSONDecodeError:
                    continue
                agent = m.get("from", "?")
                if agent_filter and agent_filter not in agent:
                    continue
                s = stats.setdefault(agent, _blank())
                ts = m.get("ts", "")
                ch = m.get("channel", "")
                if not s["first_seen"] or ts < s["first_seen"]:
                    s["first_seen"] = ts
                if not s["last_seen"] or ts > s["last_seen"]:
                    s["last_seen"] = ts
                if ch and ch != "roster":
                    s["channels"].add(ch)
                counter = _TYPE_COUNTERS.get(m.get("type", ""))
                if counter:
                    s[counter] += 1
                s["messages"].append(m.get("msg", "")[:80])
    return stats


def _success_rate(results: int, errors: int) -> str:
    total = results + errors
    return f"{(results / total * 100):.0f}%" if total > 0 else "n/a"


def _aggregate(stats: dict[str, dict[str, Any]]) -> dict[str, Any]:
    """Merge per-agent stats into one summary (used by fire's substring match)."""
    agg = _blank()
    for s in stats.values():
        for key in _TYPE_COUNTERS.values():
            agg[key] += s[key]
        agg["channels"] |= s["channels"]
        agg["messages"].extend(s["messages"])
        if not agg["first_seen"] or (s["first_seen"] and s["first_seen"] < agg["first_seen"]):
            agg["first_seen"] = s["first_seen"]
        if s["last_seen"] > agg["last_seen"]:
            agg["last_seen"] = s["last_seen"]
    return agg


def print_perf(channels_dir: str, agent_filter: str = "") -> None:
    stats = collect(channels_dir, agent_filter)
    if not stats:
        print("  (no data — agents need to use the bus first)")
        return
    print("=== Agent Performance ===")
    print()
    for agent in sorted(stats):
        s = stats[agent]
        channels = ", ".join(sorted(s["channels"])) if s["channels"] else "(none)"
        print(f"  {agent}")
        print(f"    Results: {s['results']}  Errors: {s['errors']}  Success rate: {_success_rate(s['results'], s['errors'])}")
        print(f"    Tasks requested: {s['tasks']}  Handoffs: sent={s['handoffs_sent']} recv={s['handoffs_recv']}")
        print(f"    Phone-homes: {s['phone_homes']}  Channels: {channels}")
        print(f"    Active: {s['first_seen'][:19]} to {s['last_seen'][:19]}")
        print()


def print_fire_report(channels_dir: str, agent_id: str) -> None:
    s = _aggregate(collect(channels_dir, agent_id))
    channels = ", ".join(sorted(s["channels"])) if s["channels"] else "(none)"
    print(f"  Results: {s['results']}  Errors: {s['errors']}  Success rate: {_success_rate(s['results'], s['errors'])}")
    print(f"  Tasks taken: {s['tasks']}  Channels: {channels}")
    if s["messages"]:
        print("  Last messages:")
        for msg in s["messages"][-5:]:
            print(f"    - {msg}")


def fire_summary(channels_dir: str, agent_id: str) -> dict[str, Any]:
    s = _aggregate(collect(channels_dir, agent_id))
    return {
        "results": s["results"],
        "errors": s["errors"],
        "tasks": s["tasks"],
        "success_rate": _success_rate(s["results"], s["errors"]),
        "channels": sorted(s["channels"]),
    }


def main(argv: list[str]) -> int:
    if len(argv) < 3:
        print(__doc__, file=sys.stderr)
        return 1
    mode, channels_dir = argv[1], argv[2]
    arg = argv[3] if len(argv) > 3 else ""
    if mode == "perf":
        print_perf(channels_dir, arg)
    elif mode == "fire-report":
        print_fire_report(channels_dir, arg)
    elif mode == "fire-json":
        print(json.dumps(fire_summary(channels_dir, arg)))
    else:
        print(f"unknown mode: {mode}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
