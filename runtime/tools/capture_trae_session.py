#!/usr/bin/env python3
"""Read Trae's original clipboard token and verify its session before saving.

Double-click the first Agent avatar in the intended Trae CN conversation first.
Never prints the token or its component identifiers. Default is read-only.
"""

import argparse
from contextlib import redirect_stderr, redirect_stdout
from datetime import datetime, timezone
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


TOKEN = re.compile(
    r"[0-9a-f]{16}:[0-9a-f]{32}_(?P<first>[0-9a-f]{24})"
    r"\.(?P<second>[0-9a-f]{24})\.(?P<third>[0-9a-f]{24})"
    # Trae renamed the CN product label in 3.4 ("TraeCode CN" -> "TRAE CN"), so
    # only the label is case-insensitive; the rest stays exact.
    r":(?i:Trae(?:Code|Work)? CN)\.[0-9]+\.[0-9]+\.[0-9]+\.[^\r\n:]+"
    r"\.T\([0-9]{4}/[0-9]{1,2}/[0-9]{1,2} [0-9]{2}:[0-9]{2}:[0-9]{2}\)"
)


class CaptureError(ValueError):
    """A fixed-message error safe for CLI output."""


def default_counter_script():
    bundled = Path(__file__).with_name("count_trae_interactions.py")
    return bundled if bundled.is_file() else Path.home() / ".codex/skills/swe-task/scripts/count_trae_interactions.py"


def digest(data):
    return hashlib.sha256(data).hexdigest()


def path_digest(path):
    return digest(str(path).encode("utf-8"))


def read_json(path):
    if path.is_symlink() or not path.is_file():
        raise CaptureError("本地材料必须是普通文件")
    content = path.read_bytes()
    data = json.loads(content)
    if not isinstance(data, dict):
        raise CaptureError("本地材料必须是 JSON 对象")
    return data, digest(content)


def hash_log(path):
    if not path.is_file():
        raise CaptureError("日志不可读")
    result = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            result.update(block)
    return result.hexdigest()


def save_private_json(path, data):
    path = Path(path)
    if path.is_symlink():
        raise CaptureError("不能覆盖符号链接")
    fd, temporary = tempfile.mkstemp(prefix=".session-", dir=path.parent)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as output:
            os.fchmod(output.fileno(), 0o600)
            json.dump(data, output, ensure_ascii=False, indent=2)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def load_counter(path):
    spec = importlib.util.spec_from_file_location("trae_counter", path)
    if spec is None or spec.loader is None:
        raise CaptureError("轮数核验脚本不可用")
    counter = importlib.util.module_from_spec(spec)
    # Imported helpers may print raw identifiers on errors or during loading.
    with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
        try:
            spec.loader.exec_module(counter)
        except SystemExit:
            raise CaptureError("轮数核验脚本意外退出") from None
    return counter


def task_state_link(path, plan_path, workspace, update_rounds=False):
    state, state_sha256 = read_json(path)
    task_id = state.get("task_id")
    revision = state.get("revision")
    if (
        not isinstance(state.get("schema_version"), int)
        or state.get("schema_version") != 1
        or isinstance(state.get("schema_version"), bool)
        or not isinstance(task_id, str)
        or not task_id.strip()
        or len(task_id) > 128
        or any(ord(character) < 32 for character in task_id)
        or not isinstance(revision, int)
        or isinstance(revision, bool)
        or revision < 0
    ):
        raise CaptureError("任务状态格式不符合关联要求")
    stored_workspace = state.get("workspace")
    if not isinstance(stored_workspace, str) or not Path(stored_workspace).is_absolute():
        raise CaptureError("任务状态缺少绝对工作区路径")
    if Path(stored_workspace).resolve(strict=True) != workspace:
        raise CaptureError("任务状态工作区不一致")
    stored_plan = state.get("plan_path", "")
    if not isinstance(stored_plan, str):
        raise CaptureError("任务状态计划路径格式错误")
    # Init precedes the first BindPlan; an empty plan path is not yet bound.
    if stored_plan and (
        not Path(stored_plan).is_absolute()
        or Path(stored_plan).resolve(strict=True) != plan_path
    ):
        raise CaptureError("任务状态关联计划不一致")
    if update_rounds:
        if state.get("status") not in {"trae_running", "session_captured"}:
            raise CaptureError("只允许在会话运行或初次采集草稿阶段更新轮数")
        submission = state.get("submission", {})
        submission_id = submission.get("id", 0) if isinstance(submission, dict) else None
        if not isinstance(submission_id, int) or isinstance(submission_id, bool) or submission_id != 0:
            raise CaptureError("已有提交或无效提交记录不能更新轮数")
    return {
        "task_id": task_id,
        "revision": revision,
        "state_sha256": state_sha256,
        "workspace_sha256": path_digest(workspace),
        "plan_path_sha256": path_digest(plan_path),
    }


def verify_capture(plan, token, workspace, counter, roots, update=False, historical=False):
    match = TOKEN.fullmatch(token) if isinstance(token, str) else None
    if match is None or "..." in token or "\u2026" in token:
        raise CaptureError("不是已识别的完整 Trae CN 原始 token")
    payload = plan.get("payload")
    rounds = payload.get("effective_rounds") if isinstance(payload, dict) else None
    if update:
        existing_id = plan.get("existing_submission_id", 0)
        if not isinstance(existing_id, int) or isinstance(existing_id, bool) or existing_id != 0:
            raise CaptureError("已有提交或无效提交记录不能更新轮数")
        if not isinstance(payload, dict):
            raise CaptureError("提交材料缺少有效字段对象")
        rounds = None
    elif not isinstance(rounds, int) or isinstance(rounds, bool) or rounds <= 0:
        raise CaptureError("提交材料必须包含正整数有效轮数")
    workspace = Path(workspace).resolve(strict=True)
    if not workspace.is_dir():
        raise CaptureError("指定工作区不是目录")
    if historical:
        return verify_historical(token, workspace, counter, roots, match)
    with redirect_stdout(io.StringIO()), redirect_stderr(io.StringIO()):
        return verify_logs(token, workspace, counter, roots, match, rounds)


def verify_logs(token, workspace, counter, roots, match, rounds):
    try:
        logs = sorted({Path(log).resolve(strict=True) for log in counter.candidate_logs(roots)})
        if not logs:
            raise CaptureError("未找到真实日志")
        original_hashes = {log: hash_log(log) for log in logs}
        session_ids = counter.session_ids_in_token(token, logs)
        if not isinstance(session_ids, set) or len(session_ids) != 1:
            raise CaptureError("原串不能唯一对应真实日志")
        session_id = next(iter(session_ids))
        if session_id not in match.group("first", "second", "third"):
            raise CaptureError("日志会话不属于原串")
        expected = counter.discover_session(str(workspace), roots)
        if session_id != expected:
            raise CaptureError("原串会话与指定工作区不一致")
        plan_ids = set()
        matched_hashes = []
        matched_logs = []
        for log in logs:
            plans, turns, _ = counter.inspect_log(log, session_id)
            if not isinstance(plans, set) or not isinstance(turns, set):
                raise CaptureError("日志核验结果格式错误")
            if any(not isinstance(value, str) or not value for value in plans | turns):
                raise CaptureError("日志事件缺少完整标识")
            if plans or turns:
                matched_hashes.append(original_hashes[log])
                matched_logs.append(log)
                plan_ids.update(plans)
        if not plan_ids or (rounds is not None and rounds != len(plan_ids)):
            raise CaptureError("日志有效轮数与提交材料不一致")
        # Only the logs that carry this session's events define the snapshot.
        # Trae keeps appending to unrelated windows' renderer logs, which cannot
        # change the measured rounds or the session mapping, so requiring every
        # candidate log to stand still would fail verification for reasons that
        # have nothing to do with this task.
        if any(hash_log(log) != original_hashes[log] for log in matched_logs):
            raise CaptureError("核验期间日志发生变化，请重新核验")
        current_logs = sorted({Path(log).resolve(strict=True) for log in counter.candidate_logs(roots)})
        if current_logs != logs or counter.discover_session(str(workspace), roots) != session_id:
            raise CaptureError("核验期间日志集合或工作区会话发生变化，请重新核验")
        # A log that starts carrying this session's events mid-read would change
        # the count, so the set of contributing logs has to be stable as well.
        if [log for log in logs if any(counter.inspect_log(log, session_id)[:2])] != matched_logs:
            raise CaptureError("核验期间日志发生变化，请重新核验")
    except SystemExit:
        raise CaptureError("轮数核验脚本意外退出") from None
    return {
        "token_length": len(token),
        "token_sha256": digest(token.encode("utf-8")),
        "workspace_sha256": path_digest(workspace),
        "workspace_verified": True,
        "effective_round_count": len(plan_ids),
        "matching_log_count": len(matched_hashes),
        "matched_log_sha256": matched_hashes,
        "verified_at": datetime.now(timezone.utc).isoformat(),
    }


TOOLCALL_TAG = re.compile(r"toolcall-(?P<turn>[0-9a-f]{24})-(?P<call>[A-Za-z0-9_-]+)")
TOKEN_TIME = re.compile(
    r"\.T\((?P<year>\d{4})/(?P<month>\d{1,2})/(?P<day>\d{1,2}) "
    r"(?P<hour>\d{2}):(?P<minute>\d{2}):(?P<second>\d{2})\)$"
)


def token_timestamp(token):
    found = TOKEN_TIME.search(token)
    if found is None:
        raise CaptureError("Session 原串缺少执行时间")
    try:
        local = datetime(*(int(found.group(name)) for name in ("year", "month", "day", "hour", "minute", "second")))
    except ValueError:
        raise CaptureError("Session 原串执行时间无效") from None
    return int(local.timestamp())


def snapshot_repo(roots, session_id):
    for root in roots:
        candidate = Path(root) / "ModularData/ai-agent/snapshot" / session_id / "v2"
        if (candidate / ".git").exists():
            return candidate
    return None


def snapshot_tags(repo):
    result = subprocess.run(
        ["git", "-C", str(repo), "for-each-ref", "--format=%(taggerdate:unix) %(refname:short)", "refs/tags"],
        capture_output=True,
        text=True,
    )
    if result.returncode != 0:
        raise CaptureError("历史快照仓库不可读")
    stamps = {}
    for line in result.stdout.splitlines():
        parts = line.strip().split(" ", 1)
        if len(parts) != 2:
            continue
        stamp, tag = parts
        try:
            stamps[tag] = int(stamp)
        except ValueError:
            continue
    return stamps


def live_plan_round_count(token, counter, roots):
    """Best-effort PlanItemHandler count from logs that still exist.

    The snapshot repository tags tool calls, which is not the effective-round
    metric. When the renderer log is still present, prefer its plan-event count
    so a historical receipt cannot silently rewrite 110 rounds as 28 tool calls.
    """
    try:
        logs = sorted({Path(log).resolve(strict=True) for log in counter.candidate_logs(roots)})
        if not logs:
            return 0
        ids = counter.session_ids_in_token(token, logs)
        if not isinstance(ids, set) or len(ids) != 1:
            return 0
        session_id = next(iter(ids))
        plans = set()
        for log in logs:
            found, _, _ = counter.inspect_log(log, session_id)
            if isinstance(found, set):
                plans.update(found)
        return len(plans)
    except Exception:
        return 0


def verify_historical(token, workspace, counter, roots, match):
    """Verify a finished run from Trae's per-session snapshot repository.

    The renderer log that carried the live event stream is rotated away when
    Trae restarts, but the snapshot repository survives: it is keyed by the
    session id and tags every tool call. Effective rounds follow the shared
    tc-hook-kit rule - dedupe by tool_use_id - except that the exclusion list
    cannot be applied retroactively because these tags carry no tool name.
    """
    session_id = match.group("first")
    turn_id = match.group("third")
    repo = snapshot_repo(roots, session_id)
    if repo is None:
        raise CaptureError("未找到该会话的历史快照")
    stamps = snapshot_tags(repo)
    calls = {}
    call_stamps = []
    for tag, stamp in stamps.items():
        found = TOOLCALL_TAG.fullmatch(tag)
        if found and found.group("turn") == turn_id:
            calls[found.group("call")] = True
            call_stamps.append(stamp)
    if not calls:
        raise CaptureError("历史快照中没有可核实的工具调用")
    started = stamps.get(f"before-chat-turn-{turn_id}")
    if started is None:
        raise CaptureError("历史快照缺少会话起始标记")
    finished = stamps.get(f"after-chat-turn-{turn_id}")
    stamp = token_timestamp(token)
    # Trae's token timestamp can be the session start, the final turn, or the
    # moment the operator copied the avatar. Accept the whole session window
    # (plus a short start tolerance and a copying grace after it ends) instead
    # of demanding the timestamp equal the start within one minute.
    if stamp < started - 60 or (finished is not None and stamp > finished + 3600):
        raise CaptureError("历史快照与 Session 原串时间不一致")
    # The workspace store names only the last active session. A finished task
    # may have moved on to another conversation, so fall back to the renderer
    # log's Session fetched event, which couples the chat_session_id and the
    # local_folder. Only reject when neither binding points at this workspace.
    try:
        expected = counter.discover_session(str(workspace), roots)
    except Exception:
        expected = ""
    mapped = False
    if expected != session_id:
        try:
            mapped = counter.session_in_workspace(session_id, str(workspace), roots)
        except Exception:
            mapped = False
    if expected != session_id and not mapped:
        raise CaptureError("历史会话与指定工作区不一致")
    live_rounds = live_plan_round_count(token, counter, roots)
    effective_rounds = live_rounds or len(calls)
    return {
        "token_length": len(token),
        "token_sha256": digest(token.encode("utf-8")),
        "workspace_sha256": path_digest(workspace),
        "workspace_verified": True,
        "workspace_source": "workspace_storage_active_session" if expected == session_id else "workspace_chat_log_session_binding",
        "effective_round_count": effective_rounds,
        "rounds_source": "renderer_plan_events" if live_rounds else "trae_session_snapshot_toolcall_tags",
        "exclusion_applied": False,
        "session_source": "trae_session_snapshot",
        "chat_turns": 1 if f"after-chat-turn-{turn_id}" in stamps else 0,
        "snapshot_repo_sha256": path_digest(repo),
        "window_started_at": datetime.fromtimestamp(min(call_stamps), timezone.utc).isoformat(),
        "window_finished_at": datetime.fromtimestamp(max(call_stamps), timezone.utc).isoformat(),
        "matching_log_count": 0,
        "matched_log_sha256": [],
        "verified_at": datetime.now(timezone.utc).isoformat(),
    }


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--plan", required=True, type=Path)
    parser.add_argument("--workspace", required=True, type=Path)
    parser.add_argument("--counter-script", type=Path, default=default_counter_script())
    parser.add_argument("--apply", action="store_true", help="save verified original token to the local plan")
    parser.add_argument("--update-rounds", action="store_true", help="save actual unique plan-event count during initial clipboard capture in a running draft; requires --apply and --task-state")
    parser.add_argument("--from-plan", action="store_true", help="reverify an already captured token without reading the clipboard")
    parser.add_argument("--stdin", action="store_true", help="read a human-entered token from standard input without exposing it in arguments")
    parser.add_argument("--trae-root", type=Path, action="append", default=[], help="additional Trae application-support directory; repeatable")
    parser.add_argument("--task-state", type=Path, help="read-only association with the local Go workflow state")
    parser.add_argument("--historical", action="store_true", help="verify a finished run from Trae's per-session snapshot repository when the renderer log is gone")
    args = parser.parse_args(argv)
    if args.from_plan and args.stdin:
        raise CaptureError("Session 来源不能同时为计划和标准输入")
    if args.update_rounds and (not args.apply or args.from_plan or args.task_state is None):
        raise CaptureError("更新轮数必须使用初次剪贴板采集、--apply 与 --task-state")
    if args.plan.name == "submission-session-audit.json":
        raise CaptureError("提交计划不能使用审计摘要文件名")
    plan, plan_sha256 = read_json(args.plan)
    plan_path = args.plan.resolve(strict=True)
    workspace = args.workspace.resolve(strict=True)
    link = task_state_link(args.task_state, plan_path, workspace, update_rounds=args.update_rounds) if args.task_state else None
    if args.from_plan:
        token = plan.get("payload", {}).get("trae_session_id", "")
    elif args.stdin:
        raw = sys.stdin.buffer.read(1025)
        if len(raw) > 1024:
            raise CaptureError("Session 原串过长")
        token = raw.decode("utf-8")
    else:
        token = subprocess.check_output(["pbpaste"], stderr=subprocess.DEVNULL).decode("utf-8")
    counter = load_counter(args.counter_script)
    roots = tuple(counter.DEFAULT_TRAE_ROOTS) + tuple(path.expanduser().resolve() for path in args.trae_root)
    audit = verify_capture(plan, token, workspace, counter, roots, update=args.update_rounds, historical=args.historical)
    audit.update({
        "source": "historical_session_snapshot" if args.historical else ("previously captured plan token" if args.from_plan else ("human-entered local web form" if args.stdin else "Trae CN first Agent avatar double-click clipboard")),
        "applied": args.apply,
        "rounds_updated": args.update_rounds,
    })
    if link:
        audit["task_state"] = link
    if args.apply:
        if args.plan.with_name("submission-session-audit.json").is_symlink():
            raise CaptureError("不能覆盖符号链接")
        if digest(args.plan.read_bytes()) != plan_sha256:
            raise CaptureError("核验期间计划发生变化，请重新核验")
        if link and digest(args.task_state.read_bytes()) != link["state_sha256"]:
            raise CaptureError("核验期间任务状态发生变化，请重新核验")
        plan["payload"]["trae_session_id"] = token
        if args.update_rounds:
            plan["payload"]["effective_rounds"] = audit["effective_round_count"]
        save_private_json(args.plan, plan)
        save_private_json(args.plan.with_name("submission-session-audit.json"), audit)
    summary = {key: value for key, value in audit.items() if key != "task_state"}
    summary["task_state_linked"] = link is not None
    print(json.dumps(summary, ensure_ascii=False))
    return 0


def run(argv=None):
    try:
        return main(argv)
    except CaptureError as error:
        # CaptureError messages are fixed strings: no token fragment, path or
        # identifier can reach the caller, so the web layer can show the real
        # reason instead of one opaque sentence.
        print(f"session_capture=BLOCKED {error}")
        return 1
    except Exception:
        # Counter and OS exceptions can contain token fragments or local paths.
        print("session_capture=BLOCKED 未取得可与工作区及有效轮数对应的完整原串；无提交、无上传")
        return 1


if __name__ == "__main__":
    raise SystemExit(run())
