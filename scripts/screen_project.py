#!/usr/bin/env python3
"""Screen one GitHub project at a fixed baseline commit before designing a task on it.

Gates (thresholds calibrated on the 2026-10 history, see references/PROJECT_SELECTION.md):

- issues: informational only. The count is how many entries the feature-overlap ledger
  must classify (check_issue_feature_overlap.py); it no longer blocks a project;
- maturity: enough non-test source and test files, repo older than a year at the baseline,
  still pushed recently; a small or very young repo has too few places a change must touch;
- license, language;
- external_deps: hardware, media or ML runtimes the model cannot exercise offline;
- history: the same repo's past tasks in the local workflow root;
- used: a repo that already had any task (data/used-projects.json or the workflow root)
  is never picked again; a batch's own project keeps its first screen report;
- container: the baseline builds in a container, then its test suite runs with
  ``--network none``. Only this gate proves verification works; a Dockerfile alone does not.

The report's ``baseline_health`` block can be copied into ``depth.baseline_health``.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import shlex
import subprocess
import sys
import tempfile
from datetime import datetime, timezone
from pathlib import Path

import used_projects


META_QUERY = """
query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    nameWithOwner url isArchived isFork stargazerCount createdAt pushedAt
    primaryLanguage { name }
    licenseInfo { spdxId }
    issues(states: [OPEN, CLOSED]) { totalCount }
    openIssues: issues(states: OPEN) { totalCount }
  }
}
"""

SOURCE_EXT = {
    "Go": (".go",),
    "Python": (".py",),
    "TypeScript": (".ts", ".tsx", ".vue", ".js", ".jsx"),
    "JavaScript": (".js", ".jsx", ".ts", ".tsx", ".vue"),
    "Java": (".java", ".kt"),
    "Kotlin": (".kt", ".java"),
    "Rust": (".rs",),
}
TEST_FILE = re.compile(
    r"(_test\.go|\.test\.[jt]sx?|\.spec\.[jt]sx?|(^|/)test_[^/]*\.py|_test\.py|Test\.java|Tests?\.kt)$"
    r"|(^|/)(tests?|__tests__|src/test)/"
)
EXCLUDED_DIR = re.compile(r"(^|/)(vendor|node_modules|dist|build|third_party|examples?)/")
GENERATED = re.compile(r"(\.pb\.go|_gen\.go|\.min\.js|\.d\.ts)$")
DOCKER_FILE = re.compile(r"(^|/)(Dockerfile[^/]*|(docker-)?compose[^/]*\.ya?ml)$", re.IGNORECASE)
COMPOSE_FILE = re.compile(r"(^|/)(docker-)?compose[^/]*\.ya?ml$", re.IGNORECASE)
SERVICE_IMAGES = re.compile(r"image:\s*['\"]?(redis|valkey|postgres|mysql|mariadb|mongo|minio|nats|kafka|rabbitmq|etcd)", re.IGNORECASE)
# 模型在本机碰不到的硬件、媒体和推理运行时；3370 castor 命中 ffmpeg 397 处。
EXTERNAL_TERMS = (
    "ffmpeg", "gstreamer", "whisper", "chromecast", "dlna", "upnp", "airplay",
    "cuda", "onnx", "tensorflow", "pytorch", "v4l2", "bluetooth", "libusb", "serialport",
)
EXTERNAL_MIN_HITS = 20
SECONDS_PER_DAY = 86400


def run(command, runner, timeout=120, **kwargs):
    return runner(command, capture_output=True, text=True, timeout=timeout, check=False, **kwargs)


def parse_time(value: str) -> datetime:
    return datetime.fromisoformat(value.replace("Z", "+00:00"))


def fetch_meta(repo: str, runner) -> dict:
    owner, _, name = repo.partition("/")
    if not owner or not re.fullmatch(r"[A-Za-z0-9_.-]+", name or ""):
        raise ValueError("--repo 必须是 owner/name")
    command = ["gh", "api", "graphql", "-f", "query=" + META_QUERY, "-F", f"owner={owner}", "-F", f"name={name}"]
    response = run(command, runner, timeout=60)
    if response.returncode != 0:
        # 只读查询，偶发网络错误重试一次。
        response = run(command, runner, timeout=60)
    if response.returncode != 0:
        raise ValueError("GitHub 只读查询失败，请检查 gh 登录状态与网络")
    data = json.loads(response.stdout).get("data", {}).get("repository")
    if not data:
        raise ValueError(f"GitHub 上找不到仓库 {repo}")
    return data


def tree_files(source: Path, commit: str, runner) -> list[str]:
    response = run(["git", "-C", str(source), "ls-tree", "-r", "--name-only", commit], runner)
    if response.returncode != 0:
        raise ValueError(f"无法读取基线 {commit} 的文件树")
    return [line for line in response.stdout.splitlines() if line]


def commit_time(source: Path, commit: str, runner) -> datetime:
    response = run(["git", "-C", str(source), "log", "-1", "--format=%cI", commit], runner)
    if response.returncode != 0:
        raise ValueError(f"无法读取基线 {commit} 的提交时间")
    return parse_time(response.stdout.strip())


def show(source: Path, commit: str, path: str, runner) -> str:
    response = run(["git", "-C", str(source), "show", f"{commit}:{path}"], runner)
    return response.stdout if response.returncode == 0 else ""


def analyze_tree(files: list[str], language: str) -> dict:
    extensions = SOURCE_EXT.get(language)
    if extensions is None:
        raise ValueError(f"暂不支持的语言 {language}，可选: {', '.join(sorted(SOURCE_EXT))}")
    code = [f for f in files if f.endswith(extensions) and not EXCLUDED_DIR.search(f) and not GENERATED.search(f)]
    tests = [f for f in code if TEST_FILE.search(f)]
    docker = [f for f in files if DOCKER_FILE.search(f)]
    return {
        "source_files": len(code) - len(tests),
        "test_files": len(tests),
        "docker_files": docker,
        "root_docker": [f for f in docker if "/" not in f],
        "compose_files": [f for f in docker if COMPOSE_FILE.search(f)],
        "ci_workflows": len([f for f in files if f.startswith(".github/workflows/")]),
        "frontend_files": len([f for f in files if f.endswith((".vue", ".tsx", ".jsx", ".svelte")) and not EXCLUDED_DIR.search(f)]),
        "non_test_source": [f for f in code if not TEST_FILE.search(f)],
    }


def external_hits(source: Path, commit: str, runner) -> dict[str, int]:
    pattern = "|".join(EXTERNAL_TERMS)
    response = run(
        ["git", "-C", str(source), "grep", "-ohiE", pattern, commit, "--", "."],
        runner,
        timeout=300,
    )
    counts: dict[str, int] = {}
    for word in response.stdout.splitlines():
        word = word.lower()
        if word in EXTERNAL_TERMS:
            counts[word] = counts.get(word, 0) + 1
    return counts


def history(workflow_root: Path, repo: str) -> list[dict]:
    target = f"github.com/{repo}".lower()
    rows = []
    if not workflow_root.is_dir():
        return rows
    for state_path in sorted(workflow_root.glob("*/task-state.json")):
        try:
            text = state_path.read_text(encoding="utf-8")
            state = json.loads(text)
        except (OSError, json.JSONDecodeError):
            continue
        urls = {u.lower().removesuffix(".git") for u in re.findall(r"https://(github\.com/[\w.-]+/[\w.-]+)", text)}
        if target not in urls:
            continue
        rounds = (state.get("trae") or {}).get("effective_rounds")
        rows.append({"task": state_path.parent.name, "effective_rounds": rounds if isinstance(rounds, int) else None})
    return rows


def gate(status: str, detail: str, **data) -> dict:
    return {"status": status, "detail": detail, **data}


def go_image(source: Path, commit: str, runner) -> str:
    match = re.search(r"(?m)^go\s+(\d+\.\d+)", show(source, commit, "go.mod", runner))
    if not match:
        raise ValueError("基线没有 go.mod 的 go 版本，需显式传 --image")
    return f"golang:{match.group(1)}"


def write_log(out_dir: Path, name: str, response) -> dict:
    data = (response.stdout or "") + (response.stderr or "")
    path = out_dir / name
    path.write_text(data, encoding="utf-8")
    raw = data.encode("utf-8")
    return {"path": str(path), "sha256": hashlib.sha256(raw).hexdigest(), "bytes": len(raw)}


def failing_tests(log: str) -> list[str]:
    names = re.findall(r"(?m)^\s*--- FAIL: (\S+)", log)
    packages = re.findall(r"(?m)^FAIL\s+(\S+)\s", log)
    return sorted(set(names)) + sorted(set(f"package {p}" for p in packages))


def has_go_generate(source: Path, commit: str, runner) -> bool:
    response = run(["git", "-C", str(source), "grep", "-l", "^//go:generate", commit, "--", "*.go"], runner)
    return response.returncode == 0 and bool(response.stdout.strip())


def ci_test_commands(source: Path, commit: str, files: list[str], runner) -> list[str]:
    """项目 CI 里实际跑的测试命令；默认命令和它不一致时以它为准（634 Gokapi 需要 go generate 和 test 构建标签）。"""
    commands = []
    for path in files:
        if not path.startswith(".github/workflows/"):
            continue
        for line in show(source, commit, path, runner).splitlines():
            match = re.match(r"\s*(?:-\s*)?run:\s*(.+)", line)
            if match and re.search(r"\b(go (test|generate)|pytest|npm (run )?test|cargo test|mvn|gradle)", match.group(1)):
                commands.append(match.group(1).strip())
    return list(dict.fromkeys(commands))


def container_check(source: Path, commit: str, language: str, args, out_dir: Path, runner) -> dict:
    """Build at the baseline with network, then run the test suite with --network none."""
    if language == "Go" and not args.image:
        image = go_image(source, commit, runner)
        # 联网阶段把依赖和测试依赖都下载进缓存卷，断网阶段只能用缓存。
        generate = "go generate ./... && " if has_go_generate(source, commit, runner) else ""
        tags = f" -tags={shlex.quote(args.go_tags)}" if args.go_tags else ""
        prepare = f"{generate}go mod download && go build ./... && go test{tags} -count=1 -run '^$' ./... >/dev/null"
        test = f"GOFLAGS=-mod=mod GOPROXY=off go test{tags} -count=1 ./... 2>&1"
    else:
        if not (args.image and args.prepare_cmd and args.test_cmd):
            raise ValueError(f"{language} 项目需要显式传 --image、--prepare-cmd 和 --test-cmd")
        image, prepare, test = args.image, args.prepare_cmd, args.test_cmd
    prepare = args.prepare_cmd or prepare
    test = args.test_cmd or test

    with tempfile.TemporaryDirectory(prefix="swe-screen.") as tmp:
        work = Path(tmp) / "src"
        work.mkdir()
        archive = subprocess.run(["git", "-C", str(source), "archive", commit], capture_output=True, check=False)
        if archive.returncode != 0:
            raise ValueError("git archive 基线失败")
        subprocess.run(["tar", "-x", "-C", str(work)], input=archive.stdout, check=True)
        cache = f"swe-screen-cache-{hashlib.sha256(f'{source}:{commit}'.encode()).hexdigest()[:12]}"
        mounts = ["-v", f"{work}:/work", "-w", "/work"]
        if image.startswith("golang:"):
            mounts += ["-v", f"{cache}:/go/pkg/mod"]
        pull = run(["docker", "pull", image], runner, timeout=args.timeout)
        prepared = run(["docker", "run", "--rm", *mounts, image, "sh", "-c", prepare], runner, timeout=args.timeout)
        prepare_log = write_log(out_dir, "baseline-prepare.log", prepared)
        if pull.returncode != 0 or prepared.returncode != 0:
            return gate(
                "fail",
                "容器内构建失败。先对照报告里的 ci_test_commands，用 --prepare-cmd/--test-cmd/--go-tags 复现项目自己的构建；"
                "照 CI 也构建不过，基线就不可用，模型会提前收尾",
                image=image,
                prepare_cmd=prepare,
                prepare_log=prepare_log,
            )
        tested = run(
            ["docker", "run", "--rm", "--network", "none", *mounts, image, "sh", "-c", test],
            runner,
            timeout=args.timeout,
        )
        test_log = write_log(out_dir, "baseline-offline-test.log", tested)
        digest = run(["docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", image], runner)
    failures = failing_tests((tested.stdout or "") + (tested.stderr or ""))
    common = {
        "image": image,
        "prepare_cmd": prepare,
        "test_cmd": test,
        "image_digest": digest.stdout.strip() if digest.returncode == 0 else "",
        "prepare_log": prepare_log,
        "test_log": test_log,
        "test_exit_code": tested.returncode,
        "known_failures": failures,
    }
    if tested.returncode == 0:
        return gate("pass", "基线在断网容器里构建并跑通全部测试", **common)
    if failures:
        return gate(
            "warn",
            f"断网后有 {len(failures)} 项既有测试失败；逐项确认与题目无关后写进 known_failures，"
            "失败集中在要改的模块就换项目",
            **common,
        )
    return gate("fail", "测试进程非零退出但没有可定位的失败项，按环境故障处理", **common)


def screen(args, runner=subprocess.run, now: datetime | None = None) -> dict:
    now = now or datetime.now(timezone.utc)
    meta = fetch_meta(args.repo, runner)
    files = tree_files(args.source, args.commit, runner)
    tree = analyze_tree(files, args.language)
    baseline_at = commit_time(args.source, args.commit, runner)
    gates: dict[str, dict] = {}

    issues = meta["issues"]["totalCount"]
    # 不再设上限（2026-10-26 用户决定）；数量只说明功能重合台账要逐条分类多少条。
    gates["issues"] = gate(
        "pass",
        f"公开 Issue {issues} 条（含已关闭，不含 PR），功能重合台账要逐条分类",
        total=issues,
        open=meta["openIssues"]["totalCount"],
    )

    age_days = (baseline_at - parse_time(meta["createdAt"])).total_seconds() / SECONDS_PER_DAY
    idle_days = (now - parse_time(meta["pushedAt"])).total_seconds() / SECONDS_PER_DAY
    problems = []
    if tree["source_files"] < args.min_source_files:
        problems.append(f"非测试源码 {tree['source_files']} 个，少于 {args.min_source_files}")
    if tree["test_files"] < args.min_test_files:
        problems.append(f"测试文件 {tree['test_files']} 个，少于 {args.min_test_files}")
    if age_days < args.min_age_days:
        problems.append(f"基线时仓库只有 {int(age_days)} 天，少于 {args.min_age_days}")
    if idle_days > args.max_idle_days:
        problems.append(f"已 {int(idle_days)} 天没有推送，超过 {args.max_idle_days}")
    if meta["isArchived"] or meta["isFork"]:
        problems.append("仓库已归档或是 fork")
    gates["maturity"] = gate(
        "fail" if problems else "pass",
        "；".join(problems) or "规模、测试、年龄和活跃度达标",
        source_files=tree["source_files"],
        test_files=tree["test_files"],
        age_days=int(age_days),
        idle_days=int(idle_days),
    )

    spdx = (meta.get("licenseInfo") or {}).get("spdxId")
    gates["license"] = gate(
        "pass" if spdx and spdx not in {"NOASSERTION", "OTHER"} else "fail",
        f"许可证 {spdx or '缺失'}",
    )
    primary = (meta.get("primaryLanguage") or {}).get("name")
    gates["language"] = gate(
        "pass" if primary == args.language else "fail",
        f"GitHub 主语言 {primary}，要求 {args.language}",
    )

    hits = external_hits(args.source, args.commit, runner)
    flagged = {term: count for term, count in hits.items() if count >= EXTERNAL_MIN_HITS}
    waived = dict(item.split("=", 1) for item in args.waive_external)
    blocking = {term: count for term, count in flagged.items() if term not in waived}
    gates["external_deps"] = gate(
        "fail" if blocking else "pass",
        ("依赖模型本机碰不到的外部运行时: " + "、".join(f"{t}×{c}" for t, c in sorted(blocking.items())))
        if blocking
        else "未发现成规模的硬件、媒体或推理运行时依赖",
        hits=hits,
        waived=waived,
    )

    past = history(args.workflow_root, args.repo)
    executed = [row for row in past if row["effective_rounds"] is not None]
    over = [row for row in executed if row["effective_rounds"] > 100]
    if len(executed) >= 2 and len(over) * 2 < len(executed):
        status, detail = "fail", f"同项目已执行 {len(executed)} 题，只有 {len(over)} 题过 100 轮"
    elif executed and not over:
        status, detail = "warn", f"同项目已执行 {len(executed)} 题，没有一题过 100 轮"
    elif len(past) >= args.max_project_tasks:
        status, detail = "warn", f"同项目已有 {len(past)} 道题，达到 {args.max_project_tasks} 道上限，新需求很难再避开已出过的功能"
    else:
        status, detail = "pass", f"同项目历史 {len(executed)} 题执行，{len(over)} 题过 100 轮"
    gates["history"] = gate(status, detail, tasks=past)

    # 用过的项目一律不再选（2026-10-31 用户决定）；本批正在出题的项目沿用第一次的筛查报告，不重筛。
    used = used_projects.lookup(args.repo, getattr(args, "used_projects", used_projects.TABLE), [args.workflow_root])
    gates["used"] = gate(
        "fail" if used else "pass",
        (f"项目已用过（{'排除表' if used['matched_by'] == 'table' else 'workflow 里有任务'}：{'、'.join(used.get('tasks') or []) or used.get('reason', '')}），换新项目")
        if used
        else "排除表和 workflow 里都没有这个项目",
    )

    if args.run_container:
        args.out_dir.mkdir(parents=True, exist_ok=True)
        gates["container"] = container_check(args.source, args.commit, args.language, args, args.out_dir, runner)
    else:
        gates["container"] = gate("pending", "尚未在断网容器里跑基线测试；加 --run-container 后才能给出 PASS")

    statuses = {g["status"] for g in gates.values()}
    verdict = "FAIL" if "fail" in statuses else ("INCOMPLETE" if "pending" in statuses else "PASS")
    container = gates["container"]
    services = sorted(
        {
            m.group(1).lower()
            for path in tree["compose_files"]
            for m in SERVICE_IMAGES.finditer(show(args.source, args.commit, path, runner))
        }
    )
    report = {
        "schema_version": 1,
        "repo": meta["nameWithOwner"],
        "repo_url": meta["url"],
        "commit": args.commit,
        "baseline_committed_at": baseline_at.isoformat(),
        "screened_at": now.isoformat(),
        "verdict": verdict,
        "gates": gates,
        "preferences": {
            "root_docker": tree["root_docker"],
            "compose_files": tree["compose_files"],
            "compose_services": services,
            "ci_workflows": tree["ci_workflows"],
            "frontend_files": tree["frontend_files"],
            "ci_test_commands": ci_test_commands(args.source, args.commit, files, runner),
            "stars": meta["stargazerCount"],
        },
    }
    if container["status"] in {"pass", "warn"}:
        report["baseline_health"] = {
            "build": "pass",
            "tests": "pass" if container["status"] == "pass" else "known_failures",
            "known_failures": container["known_failures"],
            "external_deps": sorted(blocking),
            "evidence": [container["prepare_log"], container["test_log"]],
        }
    return report


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--repo", required=True, help="owner/name")
    parser.add_argument("--source", type=Path, required=True, help="本地克隆（需包含基线 commit）")
    parser.add_argument("--commit", required=True, help="固定基线 commit")
    parser.add_argument("--language", default="Go")
    parser.add_argument("--out-dir", type=Path, required=True, help="报告与日志目录，放在模型工作区之外")
    parser.add_argument("--workflow-root", type=Path, default=Path.home() / "repositories/标注任务/swe/workflow")
    parser.add_argument("--min-source-files", type=int, default=100)
    parser.add_argument("--min-test-files", type=int, default=20)
    parser.add_argument("--min-age-days", type=int, default=365)
    parser.add_argument("--max-idle-days", type=int, default=180)
    parser.add_argument("--max-project-tasks", type=int, default=10, help="一个项目出 8-10 道题，达到后警告")
    parser.add_argument("--used-projects", type=Path, default=used_projects.TABLE, help="已用项目排除表")
    parser.add_argument("--waive-external", action="append", default=[], metavar="TERM=REASON",
                        help="确认某个外部运行时只在可选路径上、验收碰不到时豁免；必须写理由")
    parser.add_argument("--run-container", action="store_true", help="在容器里构建基线并断网跑测试")
    parser.add_argument("--image")
    parser.add_argument("--prepare-cmd", help="联网阶段：下载依赖并构建")
    parser.add_argument("--test-cmd", help="断网阶段：运行全部测试")
    parser.add_argument("--go-tags", help="Go 测试构建标签，照项目 CI 填，如 test,awsmock")
    parser.add_argument("--timeout", type=int, default=1800)
    return parser


def main() -> int:
    args = build_parser().parse_args()
    for item in args.waive_external:
        term, sep, reason = item.partition("=")
        if not sep or term not in EXTERNAL_TERMS or not reason.strip():
            print(json.dumps({"error": f"--waive-external 必须是 已知术语=理由: {item}"}, ensure_ascii=False))
            return 2
    report_path = args.out_dir / "project-screen.json"
    if report_path.exists():
        print(json.dumps({"error": f"{report_path} 已存在，换一个 --out-dir"}, ensure_ascii=False))
        return 2
    try:
        report = screen(args)
    except (ValueError, subprocess.TimeoutExpired, json.JSONDecodeError) as exc:
        print(json.dumps({"verdict": "ERROR", "error": str(exc)}, ensure_ascii=False))
        return 2
    args.out_dir.mkdir(parents=True, exist_ok=True)
    report_path.write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    summary = {name: g["status"] for name, g in report["gates"].items()}
    print(json.dumps({"verdict": report["verdict"], "gates": summary, "report": str(report_path)}, ensure_ascii=False))
    return 0 if report["verdict"] == "PASS" else 1


if __name__ == "__main__":
    sys.exit(main())
