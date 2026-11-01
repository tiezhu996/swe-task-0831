#!/usr/bin/env python3
"""Keep the table of GitHub projects that already had tasks, so they are never picked again.

A project counts as used once any task under a workflow root names its repo.
New batches only start on projects outside this table; the project a batch is
currently working on keeps going until its 10 tasks, on its existing screen
report. The table only grows: ``sync`` adds repos found in workflow roots,
``add`` records one by hand, and nothing here removes an entry.

    python3 scripts/used_projects.py sync
    python3 scripts/used_projects.py check --repo owner/name
    python3 scripts/used_projects.py add --repo owner/name --reason "..."
    python3 scripts/used_projects.py list
"""

from __future__ import annotations

import argparse
import json
import os
import re
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

TABLE = Path(__file__).resolve().parents[1] / "data" / "used-projects.json"
WORKFLOW_ROOT = Path.home() / "repositories/标注任务/swe/workflow"
# sync also reads archived tasks; screening only needs the live root since the table holds the rest.
SYNC_ROOTS = [WORKFLOW_ROOT, Path.home() / "repositories/标注任务/swe-archive"]
REPO_URL = re.compile(r"https://github\.com/([\w.-]+/[\w.-]+)")
REPO_NAME = re.compile(r"[\w.-]+/[\w.-]+")


class UsedProjectsError(Exception):
    pass


def normalize(repo: str) -> str:
    """owner/name, https://github.com/owner/name(.git) -> owner/name in lower case."""
    text = repo.strip().removeprefix("https://").removeprefix("github.com/").removesuffix("/").removesuffix(".git")
    if not REPO_NAME.fullmatch(text):
        raise UsedProjectsError(f"仓库名不合法：{repo}，应为 owner/name")
    return text.lower()


def load(path: Path = TABLE) -> dict:
    try:
        table = json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {"schema_version": 1, "projects": []}
    if not isinstance(table, dict) or not isinstance(table.get("projects"), list):
        raise UsedProjectsError(f"{path} 结构不对，应有 projects 列表")
    return table


def save(table: dict, path: Path = TABLE) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    table["projects"].sort(key=lambda row: row["key"])
    fd, temporary = tempfile.mkstemp(prefix=".used-projects-", dir=path.parent)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        stream.write(json.dumps(table, ensure_ascii=False, indent=2) + "\n")
    os.replace(temporary, path)


def scan_workflow(roots: list[Path]) -> dict[str, dict]:
    """Repos named in task-state.json under the roots, with their task names."""
    found: dict[str, dict] = {}
    for root in roots:
        if not root.is_dir():
            continue
        for state_path in sorted(root.glob("*/task-state.json")):
            try:
                text = state_path.read_text(encoding="utf-8")
            except OSError:
                continue
            for url in set(REPO_URL.findall(text)):
                name = url.removesuffix(".git")
                row = found.setdefault(name.lower(), {"repo": name, "tasks": []})
                if state_path.parent.name not in row["tasks"]:
                    row["tasks"].append(state_path.parent.name)
    return found


def lookup(repo: str, path: Path = TABLE, roots: list[Path] | None = None) -> dict | None:
    """The table entry or workflow tasks that make this repo used, else None."""
    key = normalize(repo)
    for row in load(path)["projects"]:
        if row["key"] == key:
            return {**row, "matched_by": "table"}
    hit = scan_workflow(roots if roots is not None else [WORKFLOW_ROOT]).get(key)
    if hit:
        return {"key": key, **hit, "matched_by": "workflow"}
    return None


def today() -> str:
    return datetime.now(timezone.utc).date().isoformat()


def cmd_sync(args: argparse.Namespace) -> dict:
    table = load(args.table)
    rows = {row["key"]: row for row in table["projects"]}
    added, updated = [], []
    for key, hit in scan_workflow(args.workflow_root).items():
        row = rows.get(key)
        if row is None:
            rows[key] = row = {"key": key, "repo": hit["repo"], "added_at": today(), "source": "workflow", "tasks": []}
            table["projects"].append(row)
            added.append(hit["repo"])
        new = [task for task in sorted(hit["tasks"]) if task not in row.setdefault("tasks", [])]
        if new and row["tasks"]:
            updated.append(hit["repo"])
        row["tasks"].extend(new)
    save(table, args.table)
    return {"table": str(args.table), "total": len(table["projects"]), "added": added, "updated": updated}


def cmd_add(args: argparse.Namespace) -> dict:
    if len(args.reason.strip()) < 4:
        raise UsedProjectsError("--reason 要写明为什么排除")
    key = normalize(args.repo)
    table = load(args.table)
    if any(row["key"] == key for row in table["projects"]):
        return {"table": str(args.table), "repo": args.repo, "action": "already_listed"}
    repo = args.repo.strip().removeprefix("https://").removeprefix("github.com/").removesuffix(".git")
    table["projects"].append({"key": key, "repo": repo, "added_at": today(), "source": "manual", "reason": args.reason.strip(), "tasks": []})
    save(table, args.table)
    return {"table": str(args.table), "repo": repo, "action": "added"}


def cmd_check(args: argparse.Namespace) -> dict:
    hit = lookup(args.repo, args.table, args.workflow_root)
    return {"repo": args.repo, "used": hit is not None, "evidence": hit}


def cmd_list(args: argparse.Namespace) -> dict:
    return {"table": str(args.table), **load(args.table)}


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--table", type=Path, default=TABLE)
    sub = parser.add_subparsers(dest="command", required=True)
    for name, help_text in (("sync", "把 workflow 里出现过的项目补进表"), ("check", "查一个项目是否用过，用过退出 1")):
        cmd = sub.add_parser(name, help=help_text)
        cmd.add_argument("--workflow-root", type=Path, action="append", help="可重复；默认标准 workflow 目录（sync 另加 swe-archive）")
        if name == "check":
            cmd.add_argument("--repo", required=True)
    add = sub.add_parser("add", help="手动排除一个项目")
    add.add_argument("--repo", required=True)
    add.add_argument("--reason", required=True)
    sub.add_parser("list", help="列出全部已用项目")
    args = parser.parse_args()
    if getattr(args, "workflow_root", None) is None and args.command in {"sync", "check"}:
        args.workflow_root = SYNC_ROOTS if args.command == "sync" else [WORKFLOW_ROOT]
    handlers = {"sync": cmd_sync, "add": cmd_add, "check": cmd_check, "list": cmd_list}
    try:
        result = handlers[args.command](args)
    except UsedProjectsError as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, ensure_ascii=False))
        return 2
    print(json.dumps({"ok": True, **result}, ensure_ascii=False, indent=2))
    return 1 if args.command == "check" and result["used"] else 0


if __name__ == "__main__":
    sys.exit(main())
