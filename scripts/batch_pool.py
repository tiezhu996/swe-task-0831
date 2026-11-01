#!/usr/bin/env python3
"""Run several precheck-passed SWE tasks through Trae with a bounded pool.

One Codex session owns the pool. `tick` reads every task's state, saves the
changed source of tasks whose Trae run has finished, checks that sources stay
isolated, and prints the ordered actions for this round: freeze, follow-up,
monitor, and which queued tasks may start in the free slots. It never starts
Trae, types into it, or writes to the platform.
"""

from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import os
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Any

DEFAULT_PARALLEL = 2
MAX_PARALLEL = 3
QUEUED = {"precheck_passed"}
RUNNING = {"trae_running"}
# After Trae has finished the slot is free; these only need local follow-up.
POST_RUN = {
    "session_captured",
    "session_verified",
    "artifacts_verified",
    "dry_run_passed",
    "submitting",
    "submitted",
    "qc_pending",
    "qc_rejected",
    "qc_accepted",
    "local_only",
}
FREEZE_FILES = ("frozen-source.patch", "frozen-source-files.sha256", "frozen-source-status.txt")


class PoolError(Exception):
    pass


def now() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).isoformat()


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return None


# Reading a model workspace must not rewrite its index: plain `git status`
# refreshes the stat cache in .git/index unless optional locks are off.
READ_ONLY_ENV = {"GIT_OPTIONAL_LOCKS": "0"}


def git(workspace: Path, *args: str, env: dict[str, str] | None = None) -> str:
    result = subprocess.run(
        ["git", "-C", str(workspace), *args],
        capture_output=True,
        env={**os.environ, **READ_ONLY_ENV, **(env or {})},
    )
    if result.returncode != 0:
        raise PoolError(f"git {' '.join(args[:2])} 失败：{result.stderr.decode(errors='replace').strip()}")
    return result.stdout.decode("utf-8", errors="surrogateescape")


def git_bytes(workspace: Path, *args: str, env: dict[str, str]) -> bytes:
    result = subprocess.run(["git", "-C", str(workspace), *args], capture_output=True, env={**os.environ, **READ_ONLY_ENV, **env})
    if result.returncode != 0:
        raise PoolError(f"git {' '.join(args[:2])} 失败：{result.stderr.decode(errors='replace').strip()}")
    return result.stdout


def overlaps(a: Path, b: Path) -> bool:
    return a == b or a in b.parents or b in a.parents


# ---------------------------------------------------------------------------
# Saving the changed source of a finished run


def freeze(task_dir: Path) -> dict[str, Any]:
    """Save the model's changes against the fixed baseline under verification/.

    A throwaway index captures tracked and untracked (not ignored) files
    without touching the workspace, its index or its refs. An existing
    snapshot is only accepted when it is byte-identical.
    """
    state = load_json(task_dir / "task-state.json")
    if not isinstance(state, dict):
        raise PoolError(f"{task_dir.name} 缺少 task-state.json")
    workspace = Path(state["workspace"]).resolve()
    baseline = state.get("project", {}).get("baseline_commit", "")
    if not baseline:
        raise PoolError(f"{task_dir.name} 的 state 没有固定基线")
    git(workspace, "cat-file", "-e", f"{baseline}^{{commit}}")

    with tempfile.TemporaryDirectory(prefix="swe-freeze-") as tmp:
        env = {"GIT_INDEX_FILE": str(Path(tmp) / "index")}
        git(workspace, "read-tree", baseline, env=env)
        git(workspace, "add", "-A", env=env)
        patch = git_bytes(workspace, "diff", "--cached", "--binary", baseline, env=env)
        names = git(workspace, "diff", "--cached", "--name-only", "--diff-filter=d", "-z", baseline, env=env)
    if not patch:
        raise PoolError(f"{task_dir.name} 相对基线没有任何改动，不保存空快照")
    changed = [name for name in names.split("\0") if name]
    hashes = "".join(
        f"{hashlib.sha256((workspace / name).read_bytes()).hexdigest()}  {name}\n" for name in changed
    )
    status = git(workspace, "status", "--porcelain=v1", "--untracked-files=all")
    outputs = {
        "frozen-source.patch": patch,
        "frozen-source-files.sha256": hashes.encode(),
        "frozen-source-status.txt": status.encode(),
    }

    out = task_dir / "verification"
    out.mkdir(exist_ok=True)
    existing = {name: (out / name) for name in FREEZE_FILES if (out / name).exists()}
    if existing:
        if (out / "frozen-source.patch").exists() and (out / "frozen-source.patch").read_bytes() == patch:
            return {"task": task_dir.name, "action": "already_frozen", "patch_sha256": hashlib.sha256(patch).hexdigest()}
        raise PoolError(f"{task_dir.name} 已有不同的源码快照，不覆盖；先核对源码是否在快照后被改动")
    for name, data in outputs.items():
        path = out / name
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
    manifest = {
        "schema_version": 1,
        "frozen_at": now(),
        "workspace": str(workspace),
        "baseline_commit": baseline,
        "head": git(workspace, "rev-parse", "HEAD").strip(),
        "changed_files": len(changed),
        "patch_sha256": hashlib.sha256(patch).hexdigest(),
    }
    (out / "frozen-source.json").write_text(json.dumps(manifest, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return {"task": task_dir.name, "action": "frozen", **manifest}


# ---------------------------------------------------------------------------
# Reading one task


def task_view(root: Path, task_id: str) -> dict[str, Any]:
    task_dir = root / task_id
    state = load_json(task_dir / "task-state.json")
    if not isinstance(state, dict):
        return {"task": task_id, "phase": "missing", "detail": "没有 task-state.json"}
    status = state.get("status", "")
    checkpoint = load_json(task_dir / "monitor-checkpoint.json") or {}
    frozen = (task_dir / "verification" / "frozen-source.patch").exists()
    view: dict[str, Any] = {
        "task": task_id,
        "status": status,
        "workspace": str(Path(state.get("workspace", "")).resolve()) if state.get("workspace") else "",
        "baseline": state.get("project", {}).get("baseline_commit", ""),
        "frozen": frozen,
    }
    terminal = checkpoint.get("terminal_event", "")
    if status in QUEUED:
        view["phase"] = "queued"
    elif status in RUNNING:
        if terminal == "completed" or checkpoint.get("last_action") == "confirm_completion_then_collect_verify_submit":
            view["phase"] = "finished"
        elif terminal:
            view["phase"] = "abnormal_stop"
            view["detail"] = f"日志终态 {terminal}，保留现场，不续聊、不重跑"
        else:
            view["phase"] = "running"
            binding = checkpoint.get("binding") or {}
            logs = list((checkpoint.get("logs") or {}).keys())
            if binding.get("session") and logs:
                view["monitor"] = {
                    "session_id": binding["session"],
                    "logs": logs,
                    "due": dt.datetime.now().timestamp() - checkpoint.get("last_check", 0) >= 900,
                }
            else:
                view["monitor"] = {"unbound": True}
    elif status in POST_RUN:
        view["phase"] = "post_run"
    else:
        view["phase"] = "not_ready"
        view["detail"] = f"状态 {status} 还没到可执行阶段"
    return view


def isolation_problems(views: list[dict[str, Any]], root: Path) -> list[tuple[list[str], str]]:
    """Return (tasks involved, message) pairs; every listed task is blocked."""
    problems: list[tuple[list[str], str]] = []
    active = [v for v in views if v.get("phase") in {"queued", "running", "finished"} and v.get("workspace")]
    for i, a in enumerate(active):
        wa = Path(a["workspace"])
        if overlaps(wa, root):
            problems.append(([a["task"]], f"{a['task']} 的源码目录和 workflow 目录重叠"))
        for b in active[i + 1:]:
            if overlaps(wa, Path(b["workspace"])):
                problems.append(([a["task"], b["task"]], f"{a['task']} 与 {b['task']} 共用或嵌套同一个源码目录，必须各自独立"))
    for v in active:
        if v["phase"] != "queued":
            continue
        workspace = Path(v["workspace"])
        try:
            head = git(workspace, "rev-parse", "HEAD").strip()
            dirty = git(workspace, "status", "--porcelain=v1", "--untracked-files=all").strip()
        except PoolError as exc:
            problems.append(([v["task"]], f"{v['task']} 源码目录无法读取：{exc}"))
            continue
        if head != v["baseline"]:
            problems.append(([v["task"]], f"{v['task']} 源码 HEAD 不在固定基线上"))
        if dirty:
            problems.append(([v["task"]], f"{v['task']} 尚未开跑但源码目录不干净"))
    return problems


# ---------------------------------------------------------------------------
# Commands


def cmd_init(args: argparse.Namespace) -> dict[str, Any]:
    root = args.root.resolve()
    if not 1 <= args.max_parallel <= MAX_PARALLEL:
        raise PoolError(f"--max-parallel 只能是 1 到 {MAX_PARALLEL}")
    tasks = args.task or sorted(
        p.parent.name
        for p in root.glob("*/task-state.json")
        if (load_json(p) or {}).get("status") in QUEUED
    )
    if not tasks:
        raise PoolError("没有预检已通过、待执行的任务")
    pool = args.pool or root / ".batches" / f"pool-{dt.datetime.now():%Y%m%d-%H%M%S}.json"
    if pool.exists():
        raise PoolError(f"{pool} 已存在，不覆盖")
    pool.parent.mkdir(parents=True, exist_ok=True)
    data = {"schema_version": 1, "created_at": now(), "root": str(root), "max_parallel": args.max_parallel, "tasks": tasks}
    pool.write_text(json.dumps(data, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return {"pool": str(pool), **data}


def cmd_tick(args: argparse.Namespace) -> dict[str, Any]:
    pool = load_json(args.pool)
    if not isinstance(pool, dict):
        raise PoolError("执行池文件不存在或无法解析")
    root = Path(pool["root"])
    views = [task_view(root, task) for task in pool["tasks"]]

    frozen = []
    for view in views:
        # Save the changed code as soon as a run is over, before any follow-up
        # step or another task can touch it.
        if view["phase"] in {"finished", "abnormal_stop"} and not view["frozen"] and not args.no_freeze:
            try:
                frozen.append(freeze(root / view["task"]))
                view["frozen"] = True
            except PoolError as exc:
                view["freeze_error"] = str(exc)

    problems = isolation_problems(views, root)
    running = [v for v in views if v["phase"] == "running"]
    free = max(pool["max_parallel"] - len(running), 0)
    queued = [v for v in views if v["phase"] == "queued"]
    actions: list[dict[str, Any]] = []
    for v in views:
        if v.get("freeze_error"):
            actions.append({"task": v["task"], "action": "fix_freeze", "detail": v["freeze_error"]})
    for v in views:
        if v["phase"] == "finished" and v["frozen"]:
            actions.append({"task": v["task"], "action": "confirm_completion_then_collect_verify_submit", "desktop": True})
        elif v["phase"] == "abnormal_stop":
            actions.append({"task": v["task"], "action": "report_abnormal_stop", "detail": v.get("detail", "")})
    for v in running:
        monitor = v.get("monitor", {})
        if monitor.get("unbound"):
            actions.append({"task": v["task"], "action": "bind_monitor", "detail": "首次核定短 chat ID 和 renderer.log 后运行 monitor_trae_task.py"})
        elif monitor.get("due"):
            actions.append({"task": v["task"], "action": "run_monitor", "session_id": monitor["session_id"], "logs": monitor["logs"]})
    blocked_tasks = {task for tasks, _ in problems for task in tasks}
    startable = [v for v in queued if v["task"] not in blocked_tasks][:free]
    for v in startable:
        actions.append({"task": v["task"], "action": "start_trae", "desktop": True, "workspace": v["workspace"]})
    done = all(v["phase"] in {"post_run"} for v in views)
    return {
        "pool": str(args.pool),
        "max_parallel": pool["max_parallel"],
        "running": len(running),
        "free_slots": free,
        "frozen_now": frozen,
        "isolation_problems": [message for _, message in problems],
        "blocked_tasks": sorted(blocked_tasks),
        "actions": actions,
        "tasks": views,
        "pool_finished": done,
    }


def cmd_freeze(args: argparse.Namespace) -> dict[str, Any]:
    return freeze(args.task_dir.resolve())


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    init = sub.add_parser("init", help="把待执行任务排进一个执行池")
    init.add_argument("--root", type=Path, required=True, help="workflow 根目录")
    init.add_argument("--task", action="append", help="任务目录名，按排队顺序；不传则取全部 precheck_passed")
    init.add_argument("--max-parallel", type=int, default=DEFAULT_PARALLEL)
    init.add_argument("--pool", type=Path)
    tick = sub.add_parser("tick", help="保存跑完任务的改动代码，给出本轮动作")
    tick.add_argument("--pool", type=Path, required=True)
    tick.add_argument("--no-freeze", action="store_true", help="只看状态，不保存快照")
    fr = sub.add_parser("freeze", help="单独保存一个任务的改动代码")
    fr.add_argument("--task-dir", type=Path, required=True)
    args = parser.parse_args()
    handlers = {"init": cmd_init, "tick": cmd_tick, "freeze": cmd_freeze}
    try:
        result = handlers[args.command](args)
    except PoolError as exc:
        print(json.dumps({"ok": False, "error": str(exc)}, ensure_ascii=False))
        return 1
    print(json.dumps({"ok": True, **result}, ensure_ascii=False, indent=2))
    return 0


if __name__ == "__main__":
    sys.exit(main())
