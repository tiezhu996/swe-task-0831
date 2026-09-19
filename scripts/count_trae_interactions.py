#!/usr/bin/env python3
"""Locate a Trae session locally and count its trace interactions.

``effective_round_count`` is the spreadsheet-valid count: unique
PlanItemHandler creation events whose sessionId exactly matches the resolved
Trae chat session. ``completed_model_turn_count`` is deliberately reported as
diagnostic data only; it counts stream-level turn IDs and must not replace the
effective-round count.
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sqlite3
import sys
from pathlib import Path
from urllib.parse import unquote


DEFAULT_TRAE_ROOTS = (
    Path.home() / "Library/Application Support/Trae CN",
    Path.home() / "Library/Application Support/Trae",
    Path.home() / "Library/Application Support/TRAE SOLO CN",
)
PLAN_EVENT = re.compile(
    r'\[PlanItemHandler\] New plan item created .*?"sessionId":"(?P<session>[^"]+)".*?'
    r'"planItemId":"(?P<plan>[^"]+)"'
)
TURN_ID = re.compile(r'"(?:turn_id|turnId)":"(?P<turn>[^"]+)"')
SESSION_DONE = re.compile(r'\[DoneHandler\].*?"status":"completed"')
TOKEN_CANDIDATE = re.compile(r'(?<![0-9a-f])[0-9a-f]{24}(?![0-9a-f])')


def normalized_path(value: str) -> str:
    return os.path.normcase(os.path.realpath(os.path.expanduser(value)))


def discover_session(workspace: str, roots: tuple[Path, ...]) -> str:
    expected = normalized_path(workspace)
    matches: list[Path] = []
    for root in roots:
        storage_root = root / "User/workspaceStorage"
        if not storage_root.is_dir():
            continue
        for metadata in storage_root.glob("*/workspace.json"):
            try:
                folder = json.loads(metadata.read_text(encoding="utf-8")).get("folder", "")
            except (OSError, json.JSONDecodeError):
                continue
            if folder.startswith("file://") and normalized_path(unquote(folder[7:])) == expected:
                matches.append(metadata.parent / "state.vscdb")

    session_ids: set[str] = set()
    for database in matches:
        if not database.is_file():
            continue
        try:
            with sqlite3.connect(f"file:{database}?mode=ro", uri=True) as connection:
                row = connection.execute(
                    "SELECT value FROM ItemTable WHERE key = ?",
                    ("ai-chat-v2.lastActiveSessionId",),
                ).fetchone()
        except sqlite3.Error:
            continue
        if row and isinstance(row[0], str) and row[0]:
            session_ids.add(row[0])

    if len(session_ids) != 1:
        found = ", ".join(sorted(session_ids)) or "none"
        raise ValueError(f"could not resolve one session from workspace; found: {found}")
    return session_ids.pop()


def candidate_logs(roots: tuple[Path, ...]) -> list[Path]:
    logs: list[Path] = []
    for root in roots:
        log_root = root / "logs"
        if log_root.is_dir():
            logs.extend(log_root.glob("*/window*/renderer.log"))
    return logs


def session_ids_in_token(token: str, logs: list[Path]) -> set[str]:
    """Return token fragments that Trae logs explicitly label as a session."""
    candidates = set(TOKEN_CANDIDATE.findall(token))
    if not candidates:
        raise ValueError("no 24-character Trae ID found in session token")
    found: set[str] = set()
    patterns = {
        candidate: re.compile(
            rf'"(?:chat_session_id|session_id|sessionId)":"{re.escape(candidate)}"'
            rf'|sessionId:\s*{re.escape(candidate)}'
        )
        for candidate in candidates
    }
    for log in logs:
        try:
            with log.open("r", encoding="utf-8", errors="replace") as handle:
                for line in handle:
                    for candidate, pattern in patterns.items():
                        if pattern.search(line):
                            found.add(candidate)
        except OSError:
            continue
    if len(found) != 1:
        matches = ", ".join(sorted(found)) or "none"
        raise ValueError(f"could not resolve one chat_session_id from session token; found: {matches}")
    return found


def inspect_log(log: Path, session_id: str) -> tuple[set[str], set[str], bool]:
    plan_ids: set[str] = set()
    turn_ids: set[str] = set()
    session_completed = False
    try:
        with log.open("r", encoding="utf-8", errors="replace") as handle:
            for line in handle:
                if session_id not in line:
                    continue
                plan_match = PLAN_EVENT.search(line)
                if plan_match and plan_match.group("session") == session_id:
                    plan_ids.add(plan_match.group("plan"))
                turn_match = TURN_ID.search(line)
                if turn_match:
                    turn_ids.add(turn_match.group("turn"))
                if SESSION_DONE.search(line):
                    session_completed = True
    except OSError:
        pass
    return plan_ids, turn_ids, session_completed


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--workspace", help="absolute local project path")
    group.add_argument("--session-id", help="Trae chat session ID")
    group.add_argument("--session-token", help="raw Trae telemetry/runtime identifier containing a session ID")
    parser.add_argument(
        "--trae-root",
        action="append",
        default=[],
        help="additional Trae application-support directory; repeatable",
    )
    args = parser.parse_args()

    roots = tuple(DEFAULT_TRAE_ROOTS) + tuple(Path(item).expanduser() for item in args.trae_root)
    logs = candidate_logs(roots)
    try:
        if args.session_token:
            session_id = session_ids_in_token(args.session_token, logs).pop()
            session_resolution = "verified chat_session_id extracted from session token"
        elif args.session_id:
            session_id = args.session_id
            session_resolution = "explicit --session-id"
        else:
            session_id = discover_session(args.workspace, roots)
            session_resolution = "workspaceStorage state.vscdb lastActiveSessionId"
    except ValueError as error:
        print(json.dumps({"error": str(error)}, ensure_ascii=False), file=sys.stderr)
        return 2

    observations = []
    for log in logs:
        plans, turns, completed = inspect_log(log, session_id)
        if plans or turns:
            observations.append((log, plans, turns, completed))
    if not observations:
        print(json.dumps({"session_id": session_id, "error": "no matching Trae renderer log found"}, ensure_ascii=False), file=sys.stderr)
        return 3

    # The same stream may be mirrored in several renderer logs. IDs make a union
    # safe while retaining steps that occurred after a log rotation or app restart.
    plan_ids: set[str] = set()
    turn_ids: set[str] = set()
    session_completed = False
    source_logs: list[str] = []
    for log, plans, turns, completed in observations:
        source_logs.append(str(log))
        plan_ids.update(plans)
        turn_ids.update(turns)
        session_completed = session_completed or completed
    print(json.dumps({
        "session_id": session_id,
        "session_resolution": session_resolution,
        "log_files": source_logs,
        "effective_round_count": len(plan_ids),
        # Kept for consumers of earlier versions of this script.
        "all_interaction_count": len(plan_ids),
        "observed_turn_id_count": len(turn_ids),
        "completed_model_turn_count": len(turn_ids) if session_completed else 0,
        "session_completed": session_completed,
        "effective_round_source": "unique PlanItemHandler New plan item created events with an exact sessionId match across all matching renderer logs",
        "all_interaction_source": "compatibility alias of effective_round_count",
        "model_turn_source": "diagnostic only: unique turn_id/turnId events across matching renderer logs, accepted as completed only when the Session has a DoneHandler status=completed event; do not use as effective_round_count",
    }, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
