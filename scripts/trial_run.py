#!/usr/bin/env python3
"""Rehearse a frozen prompt on a cheap model before it ever reaches TraeCN.

TraeCN is expensive and the platform only accepts sessions above 100 effective
rounds, while sessions far above that waste money. This script runs the same
prompt on the same baseline with Claude Code pointed at a cheap
Anthropic-compatible backend (DeepSeek by default), measures how much work the
model did, and converts that into a predicted Trae round count using a
calibration fitted on tasks that already ran in Trae.

Subcommands:

  run        execute the prompt N times in throwaway copies of the baseline
  confirm    read-only rehearsal of TraeCN's confirmation step: does the model
             plan new pages, endpoints, commands or settings the task never asked for
  calibrate  fit trial metrics against real Trae rounds of finished tasks
  check      the precheck gate: both rehearsals exist for this exact prompt and passed

Trials never touch the task's source workspace. Each run extracts the baseline
with ``git archive`` into a neutral throwaway directory (no task name in the
path) and runs Claude Code under ``sandbox-exec`` with reads and writes of the
task tree, skills and Claude/Codex homes denied: on 2026-10-30 an unsandboxed
smoke run walked up from its workspace into ``sources/`` and read Trae's
finished answer. Outbound traffic goes through a dead proxy except the model
backend and package registries, so upstream code cannot be fetched either.
Results land in ``--out-dir`` (under ``trials/``); nothing from a trial may be
copied into a Trae workspace.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import hashlib
import http.client
import json
import os
import re
import shutil
import statistics
import subprocess
import sys
import time
import urllib.request
import uuid
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace
from urllib.parse import urlparse

DEFAULT_ENV_FILE = Path.home() / ".config" / "deepseek.env"
DEFAULT_MODEL = "deepseek-flash"
DEFAULT_WINDOW = (110, 200)
# 2026-10-30 校准（21 道已执行题）：试跑工具调用分不出 >200 轮的题（89 轮的 5126 也有 285 次），
# 两道 >200 轮的题试跑都改了 14 个以上源码文件，其余 19 道只有 151 轮的 5108 到了这个数。
# 工具调用上限只管成本：21 道题单次最多 356 次。
SPRAWL_SOURCE_FILES = 14
DEFAULT_MAX_TOOL_CALLS = 400
# 同一题两次试跑的改动源码文件最多差 7 个（5127：4 对 11），第一次 ≤10 或 ≥18 时第二次改不了铺开熔断的结论，
# 只有 11-17 之间才跑第二次。
SINGLE_RUN_DECISIVE = (10, 18)
TRAP_STATUSES = ("hit_then_fixed", "fixed_upfront", "missed", "unclear")
SWE_ROOT = Path.home() / "repositories" / "标注任务" / "swe"
DEFAULT_CALIBRATION = SWE_ROOT / "trials" / "calibration.json"
WORK_ROOT = Path.home() / ".cache" / "swe-trial-ws"
HOME = Path.home()
# The model process may neither read nor write these: task sources, rubrics and
# answers, the skills, the user's own Claude/Codex state and private notes.
SANDBOX_DENY = [
    HOME / "repositories", HOME / ".codex", HOME / ".claude", HOME / "Documents",
    HOME / "projects", HOME / "no_cloud", HOME / ".config" / "deepseek.env",
]
# Inside the denied home: toolchains and package caches the model needs to set up
# the project the same way it would in Trae (Trae sessions built their own .venv).
HOME_ALLOW = [
    HOME / ".local", HOME / ".cache" / "uv", HOME / ".cache" / "pip", HOME / "go",
    HOME / "Library" / "Caches" / "go-build", HOME / ".npm", HOME / ".cargo", HOME / ".rustup",
    HOME / ".gradle", HOME / ".m2", HOME / ".nvm", HOME / ".pyenv",
]
DEAD_PROXY = "http://127.0.0.1:9"
# Reachable despite the dead proxy: dependency installs only.
REGISTRY_HOSTS = [
    "pypi.org", "files.pythonhosted.org", "pypi.tuna.tsinghua.edu.cn", "mirrors.aliyun.com",
    "proxy.golang.org", "sum.golang.org", "goproxy.cn", "goproxy.io", "storage.googleapis.com",
    "registry.npmjs.org", "registry.npmmirror.com", "repo.maven.apache.org", "repo1.maven.org",
    "crates.io", "index.crates.io", "static.crates.io",
]
PASS_ENV_PREFIXES = ("UV_", "PIP_", "GO", "npm_config_", "CARGO_", "RUSTUP_", "JAVA_", "MAVEN_")
# Tools that a Trae session does not have, or that would let the trial reach
# outside the workspace (upstream PRs, sub-agents, background jobs).
DISALLOWED_TOOLS = [
    "WebSearch", "WebFetch", "Task", "Agent", "Workflow", "SendMessage", "ListAgents",
    "CronCreate", "CronDelete", "CronList", "Monitor", "ScheduleWakeup", "TaskStop",
    "EnterWorktree", "ExitWorktree", "Skill", "ToolSearch", "ReportFindings",
]
SEQUENCE_CODE = {
    "Read": "R", "Edit": "E", "MultiEdit": "E", "Write": "W", "NotebookEdit": "E",
    "Bash": "S", "Grep": "G", "Glob": "G", "LS": "L", "TodoWrite": "T",
}
# Step counts describe how the cheap model worked; changed files and diff size
# describe the task itself and may transfer better to Trae. calibrate tries all.
METRIC_KEYS = ["assistant_steps", "tool_calls", "reasoning_steps", "edit_calls", "duration_min",
               "changed_source_files", "diff_lines"]
CONFIRM_PROMPT = """下面是同事发来的一条开发需求。先不要改任何文件，只阅读仓库，弄清楚你会怎么做。

需求原文：
<<<
{prompt}
>>>

读完后只输出一个 JSON 对象，不要输出别的内容：
{{
  "summary": "一句话说你打算做什么",
  "user_sees_result_in": ["用户在哪里看到效果，写现有的功能、命令或页面"],
  "new_pages": ["你打算新增的页面或界面，没有就留空"],
  "new_endpoints": ["你打算新增的对外接口或路由"],
  "new_commands": ["你打算新增的命令或子命令"],
  "new_settings": ["你打算新增的配置项"],
  "other_features": ["需求里没直接提、但你觉得顺手该做的功能"],
  "open_questions": ["开工前你想先问同事确认的问题"]
}}"""


def now() -> str:
    return datetime.now(timezone.utc).astimezone().isoformat(timespec="seconds")


def load_env_file(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value.strip().strip('"').strip("'")
    return values


def backend_env(env_file: Path, model: str, config_dir: Path) -> dict[str, str]:
    values = load_env_file(env_file)
    base = values.get("DEEPSEEK_BASE_URL") or values.get("ANTHROPIC_BASE_URL")
    key = values.get("DEEPSEEK_API_KEY") or values.get("ANTHROPIC_API_KEY")
    if not base or not key:
        raise SystemExit(f"{env_file} 缺少 BASE_URL 或 API_KEY")
    passthrough = {k: v for k, v in os.environ.items() if k.startswith(PASS_ENV_PREFIXES)}
    no_proxy = ["127.0.0.1", "localhost", "::1", urlparse(base).hostname or ""] + REGISTRY_HOSTS
    return {
        **passthrough,
        "HOME": str(Path.home()),
        "PATH": os.environ.get("PATH", "/usr/bin:/bin"),
        "LANG": os.environ.get("LANG", "en_US.UTF-8"),
        "HTTPS_PROXY": DEAD_PROXY, "HTTP_PROXY": DEAD_PROXY, "ALL_PROXY": DEAD_PROXY,
        "https_proxy": DEAD_PROXY, "http_proxy": DEAD_PROXY, "all_proxy": DEAD_PROXY,
        "NO_PROXY": ",".join(no_proxy), "no_proxy": ",".join(no_proxy),
        # A private config dir keeps the user's global CLAUDE.md, hooks, skills
        # and memory out of the trial.
        "CLAUDE_CONFIG_DIR": str(config_dir),
        "ANTHROPIC_BASE_URL": base,
        "ANTHROPIC_API_KEY": key,
        "ANTHROPIC_MODEL": model,
        "ANTHROPIC_SMALL_FAST_MODEL": model,
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "DISABLE_AUTOUPDATER": "1",
    }


def guard_out_dir(out_dir: Path) -> None:
    resolved = out_dir.resolve()
    for forbidden in (SWE_ROOT / "sources", SWE_ROOT / "workflow"):
        try:
            resolved.relative_to(forbidden.resolve())
        except ValueError:
            continue
        raise SystemExit(f"试跑目录不能放在 {forbidden} 下，会污染正式工作区或被当成任务扫描")


def extract_baseline(source: Path, commit: str, workdir: Path) -> None:
    """Fresh tree at the baseline with a one-commit history, so `git diff` works
    but no later upstream history (which might contain the answer) is visible."""
    workdir.mkdir(parents=True)
    archive = subprocess.run(["git", "-C", str(source), "archive", "--format=tar", commit],
                             check=True, capture_output=True).stdout
    subprocess.run(["tar", "-x", "-C", str(workdir)], input=archive, check=True)
    git = ["git", "-C", str(workdir), "-c", "user.name=trial", "-c", "user.email=trial@localhost",
           "-c", "core.autocrlf=false", "-c", "core.safecrlf=false"]
    subprocess.run(git + ["init", "-q"], check=True)
    subprocess.run(git + ["add", "-A"], check=True)
    # Neutral message: a baseline hash in the log let a trial look the project up online.
    subprocess.run(git + ["commit", "-q", "-m", "init"], check=True)


def sandbox_profile(path: Path, scratch: Path) -> Path:
    """Deny the whole home directory and shared temp dirs, then re-allow only
    toolchains, package caches and this run's own scratch. Later rules win in
    SBPL. The profile is written outside the scratch so the model cannot read it.

    The home directory is denied wholesale, not just the task tree: a 2026-10-30
    trial with no ready environment spent dozens of steps running `find /` and
    browsing ~/bin looking for an installed copy of the project."""
    deny = list(SANDBOX_DENY) + [HOME, Path("/private/tmp"), Path("/private/var/folders")]
    deny_rules = " ".join(f'(subpath "{p}")' if p.is_dir() or not p.suffix else f'(literal "{p}")' for p in deny)
    allow = [p for p in HOME_ALLOW if p.exists()] + [scratch]
    allow_rules = " ".join(f'(subpath "{p}")' for p in allow)
    path.write_text(
        "(version 1)\n(allow default)\n"
        f"(deny file-read* {deny_rules})\n(deny file-write* {deny_rules})\n"
        f"(allow file-read* file-write* {allow_rules})\n"
        # git, realpath and toolchains stat the parents; metadata only, listing stays denied.
        f'(allow file-read-metadata (literal "{HOME}") (literal "{WORK_ROOT.parent}") (literal "{WORK_ROOT}"))\n',
        encoding="utf-8",
    )
    return path


def new_scratch() -> Path:
    """Neutral per-run directory: the path says nothing about task or project."""
    scratch = WORK_ROOT / uuid.uuid4().hex[:12]
    scratch.mkdir(parents=True)
    return scratch


def changed_files(workdir: Path) -> list[str]:
    out = subprocess.run(["git", "-C", str(workdir), "status", "--porcelain", "-uall"],
                         capture_output=True, text=True).stdout
    files = []
    for line in out.splitlines():
        path = line[3:].split(" -> ")[-1].strip('"')
        if "/.venv/" in f"/{path}" or "__pycache__" in path or path.endswith(".pyc"):
            continue
        files.append(path)
    return sorted(files)


def diff_lines(workdir: Path) -> int:
    subprocess.run(["git", "-C", str(workdir), "add", "-A", "-N"], capture_output=True)
    out = subprocess.run(["git", "-C", str(workdir), "diff", "--numstat"],
                         capture_output=True, text=True).stdout
    total = 0
    for line in out.splitlines():
        added, removed, path = (line.split("\t") + ["", "", ""])[:3]
        if ".venv/" in path:
            continue
        total += int(added) if added.isdigit() else 0
        total += int(removed) if removed.isdigit() else 0
    return total


def stream_metrics(events: list[dict]) -> dict:
    tools: list[str] = []
    thinking = text = 0
    usage = {"input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 0}
    result = None
    final_text = ""
    for event in events:
        if event.get("type") == "assistant":
            for block in event.get("message", {}).get("content", []):
                kind = block.get("type")
                if kind == "tool_use":
                    tools.append(block.get("name", "?"))
                elif kind == "thinking":
                    thinking += 1
                elif kind == "text" and block.get("text", "").strip():
                    text += 1
        elif event.get("type") == "result":
            result = event
    if result:
        final_text = (result or {}).get("result") or ""
        for key in usage:
            usage[key] = int((result.get("usage") or {}).get(key) or 0)
    sequence = "".join(SEQUENCE_CODE.get(tool, "?") for tool in tools)
    loops = state = 0
    for code in sequence:  # same run-and-fix definition as trajectory_metrics.py
        if code in "EW":
            loops += state == 2
            state = 1
        elif code == "S" and state >= 1:
            state = 2
    counts: dict[str, int] = {}
    for tool in tools:
        counts[tool] = counts.get(tool, 0) + 1
    return {
        "assistant_steps": len(tools) + thinking + text,
        "reasoning_steps": thinking + text,
        "tool_calls": len(tools),
        "tool_counts": dict(sorted(counts.items(), key=lambda item: -item[1])),
        "edit_calls": sum(1 for code in sequence if code in "EW"),
        "shell_calls": sequence.count("S"),
        "edit_shell_edit_loops": loops,
        "sequence": sequence,
        "api_turns": (result or {}).get("num_turns"),
        "usage": usage,
        "final_text": final_text[-1500:],
        "final_text_full": final_text,
        "is_error": bool((result or {}).get("is_error")),
    }


def run_claude(prompt: str, workdir: Path, run_dir: Path, env: dict[str, str], model: str,
               max_tool_calls: int, timeout_min: float, read_only: bool = False) -> tuple[list[dict], str]:
    disallowed = list(DISALLOWED_TOOLS)
    if read_only:
        disallowed += ["Edit", "MultiEdit", "Write", "NotebookEdit", "Bash"]
    scratch = workdir.parent
    profile = sandbox_profile(run_dir / "sandbox.sb", scratch)
    tmp = scratch / "tmp"
    tmp.mkdir(exist_ok=True)
    # Claude Code otherwise writes under the shared /tmp/claude-<uid>, which also
    # holds the operator's own session files.
    env = {**env, "TMPDIR": str(tmp), "TMP": str(tmp), "TEMP": str(tmp),
           "CLAUDE_CODE_TMPDIR": str(tmp), "CLAUDE_TMPDIR": str(tmp),
           # zsh writes here-documents under $TMPPREFIX (default /tmp/zsh).
           "TMPPREFIX": str(tmp / "zsh"),
           # ~/.gitconfig and ~/.config are inside the denied home.
           "GIT_CONFIG_GLOBAL": str(scratch / "gitconfig"), "XDG_CONFIG_HOME": str(scratch / "xdg")}
    (scratch / "gitconfig").write_text("[user]\n\tname = dev\n\temail = dev@localhost\n", encoding="utf-8")
    cmd = ["sandbox-exec", "-f", str(profile), shutil.which("claude") or "claude", "-p", "--model", model, "--output-format", "stream-json", "--verbose",
           "--dangerously-skip-permissions", "--strict-mcp-config",
           "--disallowedTools", ",".join(disallowed)]
    events: list[dict] = []
    stop_reason = "finished"
    raw = (run_dir / "events.private.jsonl").open("w", encoding="utf-8")
    os.chmod(raw.name, 0o600)
    stderr = (run_dir / "stderr.log").open("w", encoding="utf-8")
    # Own process group, so the breaker also stops the shells and test runs it spawned.
    proc = subprocess.Popen(cmd, cwd=workdir, env=env, stdin=subprocess.PIPE,
                            stdout=subprocess.PIPE, stderr=stderr, text=True, start_new_session=True)
    assert proc.stdin and proc.stdout
    proc.stdin.write(prompt)
    proc.stdin.close()
    deadline = time.monotonic() + timeout_min * 60
    tool_calls = 0
    secret = env.get("ANTHROPIC_API_KEY", "")
    for line in proc.stdout:
        # The model can print its own environment; never keep the key on disk.
        raw.write(line.replace(secret, "<redacted>") if secret else line)
        try:
            event = json.loads(line)
        except json.JSONDecodeError:
            continue
        events.append(event)
        if event.get("type") == "assistant":
            tool_calls += sum(1 for b in event.get("message", {}).get("content", [])
                              if b.get("type") == "tool_use")
        if tool_calls > max_tool_calls:
            stop_reason = "max_tool_calls"
            break
        if time.monotonic() > deadline:
            stop_reason = "timeout"
            break
    try:
        os.killpg(proc.pid, 9)
    except (ProcessLookupError, PermissionError):
        # macOS answers EPERM, not ESRCH, once the group has already exited.
        pass
    proc.wait()
    raw.close()
    stderr.close()
    return events, final_stop_reason(events, stop_reason, f"exit_{proc.returncode}")


def final_stop_reason(events: list[dict], stop_reason: str, no_result: str) -> str:
    if stop_reason != "finished":
        return stop_reason
    result = next((e for e in events if e.get("type") == "result"), None)
    if not result:
        return no_result
    # Backend failures (e.g. 402 Insufficient Balance on 10-30) still end with a result event.
    return "api_error" if result.get("is_error") else "finished"


def contract_files(contract: Path | None) -> set[str]:
    if not contract or not contract.exists():
        return set()
    data = json.loads(contract.read_text(encoding="utf-8"))
    sites = (data.get("depth") or {}).get("edit_sites") or []
    return {str(site.get("location", "")).rsplit(":", 1)[0] for site in sites if site.get("location")}


def is_test_or_doc(path: str) -> bool:
    lower = path.lower()
    return (lower.startswith(("tests/", "test/", "docs/", "doc/", "changelog")) or "/tests/" in lower
            or "_test." in lower or "test_" in Path(lower).name or lower.endswith(".md"))


def one_run(index: int, args, prompt: str, contract_set: set[str]) -> dict:
    run_dir = args.out_dir / f"run-{index}"
    if run_dir.exists():
        raise SystemExit(f"{run_dir} 已存在，换一个 --out-dir 或删除旧试跑")
    run_dir.mkdir(parents=True)
    scratch = new_scratch()
    workdir = scratch / "ws"
    config_dir = scratch / "cfg"
    config_dir.mkdir()
    extract_baseline(args.source, args.commit, workdir)
    env = backend_env(args.env_file, args.model, config_dir)
    started = time.monotonic()
    started_at = now()
    events, stop_reason = run_claude(prompt, workdir, run_dir, env, args.model,
                                     args.max_tool_calls, args.timeout_min)
    return finish_run(index, run_dir, events, stop_reason, scratch, started_at,
                      (time.monotonic() - started) / 60, args, contract_set)


def finish_run(index: int, run_dir: Path, events: list[dict], stop_reason: str, scratch: Path,
               started_at: str | None, duration_min: float, args, contract_set: set[str]) -> dict:
    workdir = scratch / "ws"
    result = stream_metrics(events)
    files = changed_files(workdir)
    off_contract = [f for f in files if contract_set and f not in contract_set and not is_test_or_doc(f)]
    result.update({
        "run": index,
        "started_at": started_at,
        "ended_at": now(),
        "duration_min": round(duration_min, 1),
        "stop_reason": stop_reason,
        "changed_files": files,
        "diff_lines": diff_lines(workdir),
        "changed_source_files": sum(1 for f in files if not is_test_or_doc(f)),
        "off_contract_source_files": off_contract,
    })
    result["scratch"] = str(scratch)
    (run_dir / "trial-metrics.json").write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n",
                                                encoding="utf-8")
    if not args.keep_workspace:
        shutil.rmtree(scratch, ignore_errors=True)
    return result


def recover_run(run_dir: Path, args, contract_set: set[str]) -> dict:
    """Rebuild trial-metrics.json for a run whose driver died after the model finished.

    Needs the run's events and its leftover workspace; the model is not called again.
    """
    events = []
    for line in (run_dir / "events.private.jsonl").read_text(encoding="utf-8").splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    init = next((e for e in events if e.get("type") == "system" and e.get("cwd")), None)
    result = next((e for e in events if e.get("type") == "result"), None)
    if not init or not Path(init["cwd"]).is_dir():
        raise SystemExit(f"{run_dir} 的工作区已经不在，只能重跑")
    tool_calls = sum(1 for e in events if e.get("type") == "assistant"
                     for b in e.get("message", {}).get("content", []) if b.get("type") == "tool_use")
    stop_reason = final_stop_reason(events, "max_tool_calls" if tool_calls > args.max_tool_calls else "finished",
                                    "exit_unknown")
    duration = (result or {}).get("duration_ms")
    if duration is None:
        duration = ((run_dir / "events.private.jsonl").stat().st_mtime - (run_dir / "sandbox.sb").stat().st_mtime) * 1000
    index = int(run_dir.name.split("-", 1)[1])
    return finish_run(index, run_dir, events, stop_reason, Path(init["cwd"]).parent, None,
                      duration / 60000, args, contract_set)


def load_calibration(path: Path) -> dict | None:
    if path.exists():
        return json.loads(path.read_text(encoding="utf-8"))
    return None


def predict(calibration: dict, runs: list[dict]) -> dict:
    metric = calibration["metric"]
    values = [r[metric] for r in runs if r.get(metric) is not None]
    value = statistics.median(values)
    slope, intercept = calibration["slope"], calibration["intercept"]
    rounds = slope * value + intercept
    err = calibration["loo_mae"]
    return {"metric": metric, "metric_median": value, "predicted_trae_rounds": round(rounds),
            "band": [round(rounds - err), round(rounds + err)]}


def gate_mode(band_accuracy: float | None) -> str:
    """How hard trial_run.py check may lean on the round prediction (2026-10-30, user-approved).

    hard: out of window blocks. advisory: only a clearly-out band blocks, the rest warns.
    breaker_only: the prediction only warns; the tool-call breaker and confirm still block.
    """
    if band_accuracy is None:
        return "breaker_only"
    if band_accuracy >= 0.8:
        return "hard"
    return "advisory" if band_accuracy >= 0.6 else "breaker_only"


def hard_stop(runs: list[dict]) -> str | None:
    """Signals that block in every gate mode, independent of the round prediction."""
    if any(r.get("stop_reason") == "max_tool_calls" for r in runs):
        return "触发工具调用熔断：项目或需求太大，按窗口上限之外处理"
    files = [r["changed_source_files"] for r in runs
             if r.get("stop_reason") == "finished" and r.get("changed_source_files") is not None]
    if files and statistics.median(files) >= SPRAWL_SOURCE_FILES:
        return (f"试跑改动的源码文件中位数 {statistics.median(files):g} 个，达到 {SPRAWL_SOURCE_FILES}："
                "历史上超过 200 轮的题都是这样铺开的，收窄范围或拆成两道")
    return None


def verdict(prediction: dict | None, runs: list[dict], window: tuple[int, int]) -> tuple[str, list[str]]:
    reasons = []
    stop = hard_stop(runs)
    if stop:
        return "too_large", [stop]
    if any(r["stop_reason"] not in ("finished", "max_tool_calls") for r in runs):
        reasons.append("有试跑没有正常结束（超时、后端报错或进程异常），先看 stderr.log 再判断")
        return "inconclusive", reasons
    if not prediction:
        reasons.append("还没有校准文件，只记录原始指标，不出判定")
        return "uncalibrated", reasons
    rounds = prediction["predicted_trae_rounds"]
    if rounds < window[0]:
        reasons.append(f"换算后约 {rounds} 轮，低于窗口下限 {window[0]}")
        return "too_small", reasons
    if rounds > window[1]:
        reasons.append(f"换算后约 {rounds} 轮，高于窗口上限 {window[1]}")
        return "too_large", reasons
    return "in_window", [f"换算后约 {rounds} 轮，落在 {window[0]}-{window[1]}"]


def single_run_decisive(run: dict) -> bool:
    if run.get("stop_reason") == "max_tool_calls":
        return True
    files = run.get("changed_source_files")
    return (run.get("stop_reason") == "finished" and files is not None
            and (files <= SINGLE_RUN_DECISIVE[0] or files >= SINGLE_RUN_DECISIVE[1]))


def read_events(path: Path) -> list[dict]:
    events = []
    for line in path.read_text(encoding="utf-8").splitlines():
        try:
            events.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return events


def result_text(content) -> str:
    if isinstance(content, list):
        return "\n".join(c.get("text", "") for c in content if isinstance(c, dict))
    return str(content or "")


def compact_trajectory(events: list[dict], limit: int = 60000) -> str:
    """One line per step, with edit snippets and command output tails, for the trap judge."""
    lines, step = [], 0
    for event in events:
        if event.get("type") == "assistant":
            for block in event.get("message", {}).get("content", []):
                if block.get("type") == "text" and block.get("text", "").strip():
                    lines.append(f"[说明] {block['text'].strip()[:300]}")
                elif block.get("type") == "tool_use":
                    step += 1
                    i, name = block.get("input") or {}, block.get("name", "?")
                    if name == "Bash":
                        detail = str(i.get("command", ""))[:300]
                    elif name in ("Edit", "MultiEdit"):
                        detail = (f"{i.get('file_path', '')}\n  - {str(i.get('old_string', ''))[:200]}"
                                  f"\n  + {str(i.get('new_string', ''))[:300]}")
                    elif name == "Write":
                        detail = f"{i.get('file_path', '')}\n  + {str(i.get('content', ''))[:300]}"
                    else:
                        detail = str(i.get("file_path") or i.get("pattern") or i)[:200]
                    lines.append(f"[{step}] {name}: {detail}")
        elif event.get("type") == "user":
            for block in event.get("message", {}).get("content", []) or []:
                if isinstance(block, dict) and block.get("type") == "tool_result":
                    out = result_text(block.get("content")).strip()
                    if out and (block.get("is_error") or lines and lines[-1].startswith(f"[{step}] Bash")):
                        tag = "失败" if block.get("is_error") else "输出"
                        lines.append(f"  -> {tag}: {out[-300:]}")
    text = "\n".join(lines)
    if len(text) > limit:  # keep the start (plan) and the end (test-fix cycles)
        text = text[: limit * 3 // 10] + "\n……（中间省略）……\n" + text[-limit * 7 // 10:]
    return text


# deepseek-flash thinks first; with a small budget the thinking eats it all and no text comes back.
def call_backend(env_file: Path, model: str, prompt: str, max_tokens: int = 16000, thinking: bool = True) -> str:
    values = load_env_file(env_file)
    base = values.get("DEEPSEEK_BASE_URL") or values.get("ANTHROPIC_BASE_URL")
    key = values.get("DEEPSEEK_API_KEY") or values.get("ANTHROPIC_API_KEY")
    if not base or not key:
        raise SystemExit(f"{env_file} 缺少 BASE_URL 或 API_KEY")
    # Streamed: a long silent thinking phase made the non-streaming reply drop (IncompleteRead, 10-30).
    payload = {"model": model, "max_tokens": max_tokens, "stream": True,
               "messages": [{"role": "user", "content": prompt}]}
    if not thinking:
        payload["thinking"] = {"type": "disabled"}
    body = json.dumps(payload).encode()
    request = urllib.request.Request(base.rstrip("/") + "/v1/messages", data=body, headers={
        "x-api-key": key, "anthropic-version": "2023-06-01", "content-type": "application/json"})
    for attempt in range(2):
        try:
            parts = []
            with urllib.request.urlopen(request, timeout=300) as response:
                for raw in response:
                    line = raw.decode("utf-8", "replace").strip()
                    if not line.startswith("data:"):
                        continue
                    try:
                        event = json.loads(line[5:])
                    except json.JSONDecodeError:
                        continue
                    delta = event.get("delta") or {}
                    if event.get("type") == "content_block_delta" and delta.get("type") == "text_delta":
                        parts.append(delta.get("text", ""))
            return "".join(parts)
        except (OSError, http.client.HTTPException):
            if attempt:
                raise
    return ""


def contract_traps(contract: Path | None) -> list[dict]:
    if not contract or not contract.exists():
        return []
    data = json.loads(contract.read_text(encoding="utf-8"))
    return [t for t in (data.get("depth") or {}).get("stop_traps") or [] if isinstance(t, dict)]


def judge_traps(run_dir: Path, task_prompt: str, traps: list[dict], args) -> dict | None:
    """Ask the cheap model whether each stop trap made this run go back and rework."""
    out = run_dir / "trap-judgement.json"
    digest = hashlib.sha256(task_prompt.encode()).hexdigest()
    if out.exists():
        cached = json.loads(out.read_text(encoding="utf-8"))
        if cached.get("prompt_sha256") == digest and len(cached.get("traps", [])) == len(traps):
            return cached
    events = read_events(run_dir / "events.private.jsonl")
    listing = "\n".join(f"{n}. 看起来做完了：{t.get('looks_done', '')}；本地这样能暴露：{t.get('exposed_by', '')}"
                        for n, t in enumerate(traps, 1))
    question = (
        "下面是一个编程代理按题面改代码的执行轨迹。出题人预设了几个“停止陷阱”：做到某一步看起来已经完成，"
        "其实还有问题，按“本地这样能暴露”的办法就能发现。逐条判断这次执行里每个陷阱属于哪种情况：\n"
        "- hit_then_fixed：代理先做到了“看起来做完了”那一步，之后跑测试、脚本或自查发现问题，再返工改对；\n"
        "- fixed_upfront：代理一开始就把这一点考虑进去了，没有经历“以为做完、发现问题、返工”；\n"
        "- missed：到结束时最终改动仍停在“看起来做完了”的状态；\n"
        "- unclear：轨迹里看不出来。\n"
        '只输出 JSON：{"traps": [{"index": 1, "status": "...", "evidence": "引用轨迹步骤编号，一句话"}]}\n\n'
        f"## 题面\n{task_prompt}\n\n## 停止陷阱\n{listing}\n\n## 轨迹\n{compact_trajectory(events)}"
    )
    # With thinking on, the 10-30 backfill got no text at all in 7 of 16 runs: the whole budget went
    # to thinking. Retry once with thinking off rather than lose the judgement.
    parsed, reply, thinking = {}, "", True
    for thinking in (True, False):
        try:
            reply = call_backend(args.env_file, args.model, question, thinking=thinking)
        except Exception as exc:  # the judge is advisory; never fail the trial over it
            print(f"{run_dir.name} 陷阱评审失败：{exc}", file=sys.stderr)
            continue
        parsed = parse_json_object(reply) or {"traps": loose_trap_statuses(reply)}
        if parsed.get("traps"):
            break
    if not parsed.get("traps"):
        raw = run_dir / "trap-judgement.raw.txt"
        raw.write_text(reply, encoding="utf-8")
        os.chmod(raw, 0o600)
        print(f"{run_dir.name} 陷阱评审没有可解析的结果（原文在 {raw.name}），下次运行会重试", file=sys.stderr)
        return None
    by_index = {item.get("index"): item for item in parsed.get("traps", []) if isinstance(item, dict)}
    judged = []
    for n in range(1, len(traps) + 1):
        item = by_index.get(n) or {}
        status = item.get("status") if item.get("status") in TRAP_STATUSES else "unclear"
        judged.append({"index": n, "status": status, "evidence": str(item.get("evidence", ""))[:300]})
    result = {"schema_version": 1, "created_at": now(), "prompt_sha256": digest, "model": args.model,
              "thinking": thinking, "traps": judged}
    out.write_text(json.dumps(result, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return result


def loose_trap_statuses(reply: str) -> list[dict]:
    """Fallback when the judge's JSON is malformed, e.g. unescaped quotes inside evidence."""
    found = re.findall(r'"index"\s*:\s*(\d+)\s*,\s*"status"\s*:\s*"(\w+)"', reply)
    return [{"index": int(index), "status": status, "evidence": "（JSON 不合法，只取了状态）"}
            for index, status in found]


def trap_summary(judgements: list[dict], traps: list[dict]) -> dict:
    per_trap = [[j["traps"][n]["status"] for j in judgements] for n in range(len(traps))]
    warnings = []
    if judgements and all(s == "unclear" for statuses in per_trap for s in statuses):
        warnings.append("陷阱评审从轨迹里看不出结论，人工翻一下 events.private.jsonl")
    elif judgements and not any("hit_then_fixed" in statuses for statuses in per_trap):
        warnings.append("没有一个停止陷阱让试跑经历“以为做完、发现问题、返工”：陷阱太浅或本地暴露不出来，"
                        "加不了轮数，回 ROUND_DEPTH_DESIGN.md 第 3 节重设")
    for n, statuses in enumerate(per_trap, 1):
        if statuses and all(s == "missed" for s in statuses):
            warnings.append(f"停止陷阱 {n} 每次试跑都没被发现：题面或本地测试没给线索，TraeCN 可能停在半成品")
    return {"per_trap": per_trap,
            "hit_then_fixed": sum(s == "hit_then_fixed" for statuses in per_trap for s in statuses),
            "judged_runs": len(judgements), "warnings": warnings}


def refresh_calibration(args) -> None:
    """Refit when an executed task or a trial is newer than calibration.json."""
    trials_root, workflow_root = SWE_ROOT / "trials", SWE_ROOT / "workflow"
    inputs = []
    for trial in trials_root.glob("*/trial-run.json"):
        if trial.parent.name.startswith("."):
            continue
        inputs.append(trial)
        state = workflow_root / trial.parent.name / "task-state.json"
        if state.exists():
            inputs.append(state)
    if not inputs:
        return
    if args.calibration.exists() and args.calibration.stat().st_mtime >= max(p.stat().st_mtime for p in inputs):
        return
    try:
        cmd_calibrate(SimpleNamespace(trials_root=trials_root, workflow_root=workflow_root, model=args.model,
                                      window=args.window, out=args.calibration, quiet=True))
    except SystemExit as exc:
        print(f"自动校准跳过：{exc}", file=sys.stderr)


def cmd_run(args) -> int:
    guard_out_dir(args.out_dir)
    args.out_dir.mkdir(parents=True, exist_ok=True)
    prompt = args.prompt.read_text(encoding="utf-8").strip()
    contract_set = contract_files(args.contract)
    if args.recover:
        for run_dir in sorted(args.out_dir.glob("run-*")):
            if not (run_dir / "trial-metrics.json").exists():
                recover_run(run_dir, args, contract_set)
    else:
        indices = list(range(args.first_run, args.first_run + args.runs))
        if args.adaptive and len(indices) == 2:
            first = one_run(indices[0], args, prompt, contract_set)
            if not single_run_decisive(first):
                one_run(indices[1], args, prompt, contract_set)
        else:
            with concurrent.futures.ThreadPoolExecutor(max_workers=args.parallel) as pool:
                for future in [pool.submit(one_run, i, args, prompt, contract_set) for i in indices]:
                    future.result()
    # Include earlier runs in the same directory so a second batch extends the first.
    runs = [json.loads(p.read_text(encoding="utf-8"))
            for p in sorted(args.out_dir.glob("run-*/trial-metrics.json"))]
    refresh_calibration(args)
    calibration = load_calibration(args.calibration)
    prediction = predict(calibration, runs) if calibration and runs else None
    window = tuple(args.window)
    status, reasons = verdict(prediction, runs, window)
    drift = sorted({f for r in runs for f in r["off_contract_source_files"]})
    if drift:
        reasons.append(f"改了合同必改位置以外的源码文件 {len(drift)} 个，检查题面是否让模型多做了页面、接口或别的业务")
    traps = contract_traps(args.contract)
    judgements = []
    if traps and not args.no_trap_judge:
        for run in runs:
            run_dir = args.out_dir / f"run-{run['run']}"
            if run.get("stop_reason") == "finished" and (run_dir / "events.private.jsonl").exists():
                judged = judge_traps(run_dir, prompt, traps, args)
                if judged:
                    judgements.append(judged)
    stop_traps = trap_summary(judgements, traps) if judgements else None
    summary = {
        "schema_version": 1,
        "created_at": now(),
        "prompt": str(args.prompt),
        "prompt_sha256": hashlib.sha256(args.prompt.read_bytes()).hexdigest(),
        "source_commit": args.commit,
        "model": args.model,
        "window": list(window),
        "calibration": str(args.calibration) if calibration else None,
        "runs": [{k: r.get(k) for k in METRIC_KEYS + ["run", "stop_reason", "edit_shell_edit_loops",
                                                        "diff_lines", "usage"]} for r in runs],
        "prediction": prediction,
        "off_contract_source_files": drift,
        "verdict": status,
        "gate_mode": gate_mode(calibration.get("loo_band_accuracy") if calibration else None),
        "reasons": reasons,
        "stop_traps": stop_traps,
        "warnings": (stop_traps or {}).get("warnings", []),
    }
    (args.out_dir / "trial-run.json").write_text(json.dumps(summary, ensure_ascii=False, indent=2) + "\n",
                                                 encoding="utf-8")
    print(json.dumps(summary, ensure_ascii=False, indent=2))
    return 0 if status == "in_window" else 1


def parse_json_object(text: str) -> dict | None:
    start, end = text.find("{"), text.rfind("}")
    if start < 0 or end <= start:
        return None
    try:
        return json.loads(text[start:end + 1])
    except json.JSONDecodeError:
        return None


def cmd_confirm(args) -> int:
    guard_out_dir(args.out_dir)
    run_dir = args.out_dir / "confirm"
    if run_dir.exists():
        raise SystemExit(f"{run_dir} 已存在")
    run_dir.mkdir(parents=True)
    scratch = new_scratch()
    workdir = scratch / "ws"
    config_dir = scratch / "cfg"
    config_dir.mkdir()
    extract_baseline(args.source, args.commit, workdir)
    prompt = CONFIRM_PROMPT.format(prompt=args.prompt.read_text(encoding="utf-8").strip())
    env = backend_env(args.env_file, args.model, config_dir)
    events, stop_reason = run_claude(prompt, workdir, run_dir, env, args.model, 80, args.timeout_min,
                                     read_only=True)
    shutil.rmtree(scratch, ignore_errors=True)
    final = stream_metrics(events)["final_text_full"]
    plan = parse_json_object(final)
    contract = json.loads(args.contract.read_text(encoding="utf-8")) if args.contract else {}
    allowed = contract.get("effect") or {}
    allowed_new = set(allowed.get("allowed_new", []))
    errors, warnings = [], []
    if plan is None:
        errors.append("模型没有给出可解析的 JSON，重跑一次")
        plan = {}
    for key, label in (("new_pages", "page"), ("new_endpoints", "endpoint"),
                       ("new_commands", "command"), ("new_settings", "setting")):
        items = [i for i in plan.get(key) or [] if str(i).strip()]
        if items and label not in allowed_new:
            errors.append(f"模型打算新增{key}: {items}；合同 effect.allowed_new 没有 {label}，"
                          "TraeCN 二次确认时大概率会问要不要做，题面要把效果落点说清楚")
    if [i for i in plan.get("other_features") or [] if str(i).strip()]:
        warnings.append(f"模型想顺手做: {plan['other_features']}")
    if [i for i in plan.get("open_questions") or [] if str(i).strip()]:
        warnings.append(f"模型开工前想问: {plan['open_questions']}；每个问题都可能变成 TraeCN 的确认项")
    report = {"schema_version": 1, "created_at": now(), "stop_reason": stop_reason,
              "prompt_sha256": hashlib.sha256(args.prompt.read_bytes()).hexdigest(),
              "plan": plan, "effect": allowed, "errors": errors, "warnings": warnings}
    (args.out_dir / "trial-confirm.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n",
                                                     encoding="utf-8")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 1 if errors else 0


def fit(points: list[tuple[float, float]]) -> tuple[float, float]:
    xs, ys = [p[0] for p in points], [p[1] for p in points]
    mx, my = statistics.mean(xs), statistics.mean(ys)
    var = sum((x - mx) ** 2 for x in xs)
    slope = sum((x - mx) * (y - my) for x, y in points) / var if var else 0.0
    return slope, my - slope * mx


def cmd_calibrate(args) -> int:
    rows = []
    for trial in sorted(args.trials_root.glob("*/trial-run.json")):
        task = trial.parent.name
        if task.startswith("."):  # voided trials, e.g. .void-* from unsandboxed runs
            continue
        state = args.workflow_root / task / "task-state.json"
        if not state.exists():
            continue
        rounds = (json.loads(state.read_text(encoding="utf-8")).get("trae") or {}).get("effective_rounds")
        runs = [json.loads(p.read_text(encoding="utf-8"))
                for p in sorted(trial.parent.glob("run-*/trial-metrics.json"))]
        runs = [r for r in runs if r.get("stop_reason") == "finished"]
        if not rounds or not runs:
            continue
        row = {"task": task, "trae_rounds": rounds, "runs": len(runs)}
        for key in METRIC_KEYS:
            values = [r[key] for r in runs if r.get(key) is not None]
            row[key] = statistics.median(values)
            row[key + "_spread"] = (max(values) - min(values)) if len(values) > 1 else None
        rows.append(row)
    if len(rows) < 6:
        raise SystemExit(f"只有 {len(rows)} 个可用任务，至少要 6 个才校准")
    window = tuple(args.window)
    candidates = []
    for key in METRIC_KEYS:
        points = [(row[key], row["trae_rounds"]) for row in rows]
        errors, hits = [], 0
        for i, (x, y) in enumerate(points):
            slope, intercept = fit(points[:i] + points[i + 1:])
            guess = slope * x + intercept
            errors.append(abs(guess - y))
            band = lambda v: 0 if v < 100 else (2 if v > window[1] else 1)
            hits += band(guess) == band(y)
        slope, intercept = fit(points)
        ys = [p[1] for p in points]
        xs = [p[0] for p in points]
        corr = statistics.correlation(xs, ys) if len(set(xs)) > 1 else 0.0
        candidates.append({"metric": key, "slope": round(slope, 4), "intercept": round(intercept, 2),
                           "pearson_r": round(corr, 3), "loo_mae": round(statistics.mean(errors), 1),
                           "loo_band_accuracy": round(hits / len(points), 2)})
    candidates.sort(key=lambda c: (c["loo_mae"], -c["loo_band_accuracy"]))
    best = dict(candidates[0])
    local_window = [round((w - best["intercept"]) / best["slope"], 1) if best["slope"] else None for w in window]
    tried = {row["task"] for row in rows} | {p.parent.name for p in args.trials_root.glob("*/trial-run.json")}
    missing = sorted(state.parent.name for state in args.workflow_root.glob("*/task-state.json")
                     if state.parent.name not in tried
                     and (json.loads(state.read_text(encoding="utf-8")).get("trae") or {}).get("effective_rounds"))
    out = {"schema_version": 1, "created_at": now(), "model": args.model, "window": list(window),
           **best, "gate_mode": gate_mode(best["loo_band_accuracy"]), "local_window": local_window, "candidates": candidates, "rows": rows,
           "executed_without_trial": missing}
    args.out.parent.mkdir(parents=True, exist_ok=True)
    # Atomic: trial runs refresh this file and may do so from several processes at once.
    tmp = args.out.with_name(f".{args.out.name}.{os.getpid()}.tmp")
    tmp.write_text(json.dumps(out, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    os.replace(tmp, args.out)
    if getattr(args, "quiet", False):
        print(f"已重新校准：{len(rows)} 道题，{best['metric']}，误差 {best['loo_mae']} 轮，门禁 {out['gate_mode']}",
              file=sys.stderr)
    else:
        print(json.dumps({k: v for k, v in out.items() if k != "rows"}, ensure_ascii=False, indent=2))
    return 0


def cmd_check(args) -> int:
    """Gate used by precheck_gate.py; it never starts a model run."""
    digest = hashlib.sha256(args.prompt.read_bytes()).hexdigest()
    errors, warnings = [], []
    run_path, confirm_path = args.trial_dir / "trial-run.json", args.trial_dir / "trial-confirm.json"
    if not run_path.exists():
        errors.append(f"缺 {run_path}，先运行 trial_run.py run")
    else:
        trial = json.loads(run_path.read_text(encoding="utf-8"))
        runs = trial.get("runs", [])
        finished = [r for r in runs if r.get("stop_reason") == "finished"]
        needed = 1 if len(runs) == 1 and single_run_decisive(runs[0]) else 2
        if trial.get("prompt_sha256") != digest:
            errors.append("trial-run.json 不是这份题面的试跑，题面改过后要重新试跑")
        if len(runs) < needed:
            errors.append(f"至少试跑 2 次：第一次改动源码文件在 {SINGLE_RUN_DECISIVE[0] + 1}-{SINGLE_RUN_DECISIVE[1] - 1} 个之间，"
                          "单次结果定不了是否铺开")
        status, mode = trial.get("verdict"), trial.get("gate_mode", "hard")
        message = f"试跑判定为 {status}（门禁 {mode}）：{'；'.join(trial.get('reasons', []))}"
        band = (trial.get("prediction") or {}).get("band") or [None, None]
        window = trial.get("window") or list(DEFAULT_WINDOW)
        breaker = hard_stop(trial.get("runs", [])) is not None
        clearly_out = band[0] is not None and (band[1] < 100 or band[0] > window[1])
        if breaker or status == "inconclusive":
            errors.append(message)
        elif status != "in_window" and (mode == "hard" or (mode == "advisory" and clearly_out)):
            errors.append(message)
        elif status != "in_window":
            warnings.append(message + "；换算误差大，这里只提示，结合合同和探针题判断")
        if not errors and len(finished) < needed:
            errors.append(f"正常结束的试跑不足 {needed} 次")
        warnings += [f"停止陷阱：{w}" for w in trial.get("warnings", [])]
    if not confirm_path.exists():
        errors.append(f"缺 {confirm_path}，先运行 trial_run.py confirm")
    else:
        confirm = json.loads(confirm_path.read_text(encoding="utf-8"))
        if confirm.get("prompt_sha256") != digest:
            errors.append("trial-confirm.json 不是这份题面的预演，题面改过后要重新预演")
        errors += [f"二次确认预演: {e}" for e in confirm.get("errors", [])]
    print(json.dumps({"passed": not errors, "errors": errors, "warnings": warnings}, ensure_ascii=False, indent=2))
    return 1 if errors else 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = parser.add_subparsers(dest="command", required=True)

    def common(p):
        p.add_argument("--source", type=Path, required=True, help="包含基线 commit 的 Git 仓库（只读）")
        p.add_argument("--commit", required=True)
        p.add_argument("--prompt", type=Path, required=True)
        p.add_argument("--out-dir", type=Path, required=True, help="试跑目录，不能在 sources/ 或 workflow/ 下")
        p.add_argument("--contract", type=Path, help="design-contract.json，用来比对改动范围和 effect")
        p.add_argument("--env-file", type=Path, default=DEFAULT_ENV_FILE)
        p.add_argument("--model", default=DEFAULT_MODEL)
        p.add_argument("--timeout-min", type=float, default=75)

    run = sub.add_parser("run", help="在一次性基线副本里完整执行题面")
    common(run)
    run.add_argument("--runs", type=int, default=2)
    run.add_argument("--first-run", type=int, default=1, help="追加试跑时从这个编号开始")
    run.add_argument("--parallel", type=int, default=2)
    run.add_argument("--recover", action="store_true",
                     help="不调模型：用已有事件日志和留下的工作区补写指标与汇总")
    run.add_argument("--max-tool-calls", type=int, default=DEFAULT_MAX_TOOL_CALLS, help="熔断：超过即判太大")
    run.add_argument("--calibration", type=Path, default=DEFAULT_CALIBRATION)
    run.add_argument("--window", type=int, nargs=2, default=list(DEFAULT_WINDOW))
    run.add_argument("--keep-workspace", action="store_true")
    run.add_argument("--no-adaptive", dest="adaptive", action="store_false",
                     help="两次都跑；默认第一次改动源码文件 ≤10 或 ≥18 时省掉第二次")
    run.add_argument("--no-trap-judge", action="store_true", help="不评审停止陷阱")
    run.set_defaults(func=cmd_run)

    confirm = sub.add_parser("confirm", help="只读预演 TraeCN 的二次确认")
    common(confirm)
    confirm.set_defaults(func=cmd_confirm)

    cal = sub.add_parser("calibrate", help="用已执行任务的 Trae 轮数拟合换算关系")
    cal.add_argument("--trials-root", type=Path, default=SWE_ROOT / "trials")
    cal.add_argument("--workflow-root", type=Path, default=SWE_ROOT / "workflow")
    cal.add_argument("--model", default=DEFAULT_MODEL)
    cal.add_argument("--window", type=int, nargs=2, default=list(DEFAULT_WINDOW))
    cal.add_argument("--out", type=Path, default=DEFAULT_CALIBRATION)
    cal.set_defaults(func=cmd_calibrate)

    check = sub.add_parser("check", help="预检门禁：核对本题面的试跑与二次确认预演结果")
    check.add_argument("--trial-dir", type=Path, required=True)
    check.add_argument("--prompt", type=Path, required=True)
    check.set_defaults(func=cmd_check)

    args = parser.parse_args()
    # Every path is used from inside the trial workspace, so make them absolute.
    for name in ("source", "prompt", "out_dir", "contract", "env_file", "calibration", "trial_dir",
                 "trials_root", "workflow_root", "out"):
        if isinstance(getattr(args, name, None), Path):
            setattr(args, name, getattr(args, name).expanduser().resolve())
    return args.func(args)


if __name__ == "__main__":
    sys.exit(main())
