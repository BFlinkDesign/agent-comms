"""Channel scanner for agent-runner.sh -- find claimable tasks.

Standalone stdlib-only script (no hive imports) so the runner can invoke it
directly: python runner_scan.py <channel.jsonl>

Emits one line per claimable task: <task_id>|<msg with | escaped as [pipe]>

A task is claimable when:
  1. no claim cell references it, AND
  2. no result/error cell references it, AND
  3. every task id in its data.depends_on has a result cell.

Rule 3 is the dependency enforcement PROTOCOL.md promises ("Checks
dependencies -- skips BLOCKED tasks") and the FLEET-OPS never-again rule
exists for: Codex once claimed TASK-3 before TASK-2 was even posted.
A dependency id that has no task cell in the channel counts as unsatisfied.
"""
import json
import sys


def scan(lines):
    """Yield (task_id, msg) for claimable tasks given JSONL lines."""
    tasks = {}        # task_id -> cell
    claimed = set()
    done = set()      # task ids with a result or error

    for line in lines:
        line = line.strip()
        if not line:
            continue
        try:
            cell = json.loads(line)
        except json.JSONDecodeError:
            continue

        ctype = cell.get("type", "")
        cid = cell.get("id", "")
        data = cell.get("data", {})
        if not isinstance(data, dict):
            data = {}

        if ctype == "task":
            tasks[cid] = cell
        elif ctype == "claim":
            tid = data.get("task_id", "")
            if tid:
                claimed.add(tid)
        elif ctype in ("result", "error"):
            tid = data.get("task_id", "")
            if tid:
                done.add(tid)

    for tid, cell in tasks.items():
        if tid in claimed or tid in done:
            continue
        deps = cell.get("data", {})
        deps = deps.get("depends_on", []) if isinstance(deps, dict) else []
        if not isinstance(deps, list):
            deps = []
        if any(dep not in done for dep in deps):
            continue  # blocked -- runner re-checks on a later poll
        yield tid, cell.get("msg", "")


def main(argv):
    if len(argv) != 2:
        print("usage: runner_scan.py <channel.jsonl>", file=sys.stderr)
        return 1
    with open(argv[1], encoding="utf-8") as f:
        for tid, msg in scan(f):
            print(f"{tid}|{msg.replace('|', '[pipe]')}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
