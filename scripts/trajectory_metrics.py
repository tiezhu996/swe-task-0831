#!/usr/bin/env python3
"""Measure where a Trae session spent its rounds, and archive the raw session log.

Every effective round is one ``PlanItemHandler New plan item created`` event.
Events without ``toolCallName`` are model reasoning steps; the rest are tool
calls. Across the 2026-10 sessions roughly two thirds of rounds were reasoning,
so rounds track the number of tool calls, which in turn come from scattered
edit sites (Edit) and run-and-fix loops (Edit -> Shell -> Edit).

Trae rotates its renderer logs, so archive the session lines right after the
run; later analysis then reads the archived copy with ``--log``.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import sys
from collections import Counter, defaultdict
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
from count_trae_interactions import DEFAULT_TRAE_ROOTS, candidate_logs, discover_session


PLAN_ITEM = re.compile(
    r'\[PlanItemHandler\] New plan item created .*?"sessionId":"(?P<session>[^"]+)"'
    r'.*?"planItemId":"(?P<plan>[^"]+)"(?:.*?"toolCallName":"(?P<tool>[^"]*)")?'
)
SESSION_FIELD = re.compile(r'"(?:sessionId|chat_session_id|session_id)"\s*:\s*"([^"]+)"')
TIMESTAMP = re.compile(r"^(\d{4}-\d{2}-\d{2}T[\d:.]+[+-]\d{2}:\d{2})")
SEQUENCE_CODE = {
    "Read": "R",
    "Edit": "E",
    "Write": "W",
    "Shell": "S",
    "Grep": "G",
    "Glob": "G",
    "LS": "L",
    "TodoWrite": "T",
    "GetDiagnostics": "D",
}
ARCHIVE_NAME = "trae-renderer-session.private.log"
METRICS_NAME = "trajectory-metrics.json"


def read_events(logs: list[Path]) -> dict[str, list[tuple[str, str, str]]]:
    """Return {session: [(timestamp, plan_id, tool), ...]} with plan IDs de-duplicated."""
    sessions: dict[str, dict[str, tuple[str, str, str]]] = defaultdict(dict)
    for log in logs:
        try:
            handle = log.open("r", encoding="utf-8", errors="replace")
        except OSError:
            continue
        with handle:
            for line in handle:
                match = PLAN_ITEM.search(line)
                if not match:
                    continue
                stamp = TIMESTAMP.match(line)
                plan = match.group("plan")
                sessions[match.group("session")].setdefault(
                    plan, (stamp.group(1) if stamp else "", plan, match.group("tool") or "")
                )
    return {session: sorted(items.values()) for session, items in sessions.items()}


def metrics(events: list[tuple[str, str, str]]) -> dict:
    tools = [tool for _, _, tool in events if tool]
    counts = Counter(tools)
    sequence = "".join(SEQUENCE_CODE.get(tool, "?") for tool in tools)
    writes = {"E", "W"}
    # A run-and-fix loop: an edit, then a shell run, then another edit.
    loops = 0
    state = 0
    for code in sequence:
        if code in writes:
            if state == 2:
                loops += 1
            state = 1
        elif code == "S" and state >= 1:
            state = 2
    first_write = next((i for i, code in enumerate(sequence) if code in writes), len(sequence))
    rounds = len(events)
    return {
        "effective_round_count": rounds,
        "reasoning_rounds": rounds - len(tools),
        "tool_calls": len(tools),
        "tool_counts": dict(counts.most_common()),
        "edit_calls": counts["Edit"] + counts["Write"],
        "shell_calls": counts["Shell"],
        "edit_shell_edit_loops": loops,
        "exploration_calls_before_first_edit": first_write,
        "sequence": sequence,
        "started_at": events[0][0] if events else "",
        "ended_at": events[-1][0] if events else "",
    }


def session_lines(logs: list[Path], session: str) -> list[str]:
    seen: set[str] = set()
    lines: list[str] = []
    for log in logs:
        try:
            handle = log.open("r", encoding="utf-8", errors="replace")
        except OSError:
            continue
        with handle:
            for line in handle:
                if session in SESSION_FIELD.findall(line) and line not in seen:
                    seen.add(line)
                    lines.append(line if line.endswith("\n") else line + "\n")
    return sorted(lines, key=lambda item: (TIMESTAMP.match(item).group(1) if TIMESTAMP.match(item) else "", item))


def write_private(path: Path, data: bytes) -> None:
    if path.exists():
        if path.read_bytes() == data:
            return
        raise FileExistsError(f"{path} 已存在且内容不同，不覆盖")
    path.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "wb") as handle:
        handle.write(data)


def archive(logs: list[Path], session: str, result: dict, directory: Path) -> dict:
    data = "".join(session_lines(logs, session)).encode("utf-8")
    log_path = directory / ARCHIVE_NAME
    write_private(log_path, data)
    receipt = {
        **result,
        "archived_log": {"path": str(log_path), "sha256": hashlib.sha256(data).hexdigest(), "bytes": len(data)},
    }
    write_private(directory / METRICS_NAME, (json.dumps(receipt, ensure_ascii=False, indent=2) + "\n").encode("utf-8"))
    return receipt


def task_rounds(task_dir: Path) -> int | None:
    try:
        value = json.loads((task_dir / "task-state.json").read_text(encoding="utf-8"))["trae"]["effective_rounds"]
    except (OSError, KeyError, TypeError, json.JSONDecodeError):
        return None
    return value if isinstance(value, int) else None


def scan(root: Path) -> list[dict]:
    """Calibration table from archived session logs under workflow/<task>/."""
    rows = []
    for task_dir in sorted(p for p in root.iterdir() if p.is_dir()):
        logs = sorted(task_dir.glob("**/*.private.log"))
        recorded = task_rounds(task_dir)
        if not logs:
            continue
        sessions = read_events(logs)
        # Archives may hold neighbouring sessions; the task's own session is the one
        # whose round count matches task-state, otherwise report it as ambiguous.
        matching = [s for s, ev in sessions.items() if recorded is not None and len(ev) == recorded]
        row: dict = {"task": task_dir.name, "recorded_rounds": recorded}
        if len(matching) == 1:
            row.update(metrics(sessions[matching[0]]))
        else:
            row["error"] = f"未能唯一匹配会话：{len(matching)} 个会话与记录轮数一致"
        rows.append(row)
    return rows


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    source = parser.add_mutually_exclusive_group(required=True)
    source.add_argument("--workspace", help="Trae 源码工作区绝对路径，按 lastActiveSessionId 定位会话")
    source.add_argument("--session-id", help="Trae 短 chat session ID")
    source.add_argument("--scan-workflow", type=Path, help="workflow 根目录，汇总各任务已归档日志")
    parser.add_argument("--log", type=Path, action="append", default=[], help="改读指定日志（例如已归档的私有日志）；可重复")
    parser.add_argument("--archive-dir", type=Path, help="把本会话日志和度量写入该目录（0600，不覆盖）")
    args = parser.parse_args()

    if args.scan_workflow:
        print(json.dumps(scan(args.scan_workflow), ensure_ascii=False, indent=2))
        return 0

    logs = list(args.log) or candidate_logs(tuple(DEFAULT_TRAE_ROOTS))
    try:
        session = args.session_id or discover_session(args.workspace, tuple(DEFAULT_TRAE_ROOTS))
    except ValueError as error:
        print(json.dumps({"error": str(error)}, ensure_ascii=False), file=sys.stderr)
        return 2
    events = read_events(logs).get(session, [])
    if not events:
        print(json.dumps({"session_id": session, "error": "日志中没有该会话的 PlanItemHandler 事件"}, ensure_ascii=False), file=sys.stderr)
        return 3
    result = {"session_id": session, **metrics(events)}
    if args.archive_dir:
        try:
            result = archive(logs, session, result, args.archive_dir)
        except FileExistsError as error:
            print(json.dumps({"error": str(error)}, ensure_ascii=False), file=sys.stderr)
            return 4
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
