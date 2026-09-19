#!/usr/bin/env python3
"""Publish one to three existing SWE task worktrees to GitHub fork branches."""

from __future__ import annotations

import argparse
import json
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path


GITHUB_REPOSITORY_URL_PATTERN = re.compile(
    r"^https?://(?:www\.)?github\.com/(?P<owner>[^/?#\s]+)/(?P<repo>[^/?#\s]+?)(?:\.git)?/?(?:[?#].*)?$",
    re.IGNORECASE,
)
COMMIT_PATTERN = re.compile(r"^[0-9a-f]{7,64}$", re.IGNORECASE)


class CommandError(RuntimeError):
    pass


@dataclass(frozen=True)
class GitHubRepository:
    owner: str
    name: str

    @property
    def slug(self) -> str:
        return f"{self.owner}/{self.name}"

    @property
    def url(self) -> str:
        return f"https://github.com/{self.slug}"


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="将 1 至 3 个完整任务工作目录提交到自有 GitHub fork 的 task1 至 taskN 分支"
    )
    parser.add_argument("--repo-url", required=True, help="原始 GitHub 仓库根地址")
    parser.add_argument("--commit", required=True, help="原始仓库的基线 Commit ID")
    parser.add_argument(
        "--task-dir",
        action="append",
        required=True,
        help="完整 Git 任务工作目录；按给定顺序映射 task1 至 taskN，可重复传入 1 至 3 次",
    )
    parser.add_argument("--fork-url", help="已有自有 fork 的 GitHub 仓库根地址；省略时通过 gh 创建或复用当前账号的 fork")
    parser.add_argument("--commit-message-prefix", default="Implement SWE task", help="提交信息前缀")
    parser.add_argument(
        "--allow-unverified-task-tree",
        action="store_true",
        help="允许没有 .git 或不含基线 Commit 的目录；仅在已人工确认目录是完整源码树时使用",
    )
    return parser.parse_args()


def run(
    command: list[str],
    cwd: Path | None = None,
    input_text: str | None = None,
    strip_output: bool = True,
) -> str:
    result = subprocess.run(command, cwd=cwd, text=True, input=input_text, capture_output=True)
    if result.returncode:
        detail = result.stderr.strip() or result.stdout.strip()
        raise CommandError(f"命令失败：{' '.join(command)}\n{detail}")
    return result.stdout.strip() if strip_output else result.stdout


def repository_from_url(value: str, field: str) -> GitHubRepository:
    match = GITHUB_REPOSITORY_URL_PATTERN.fullmatch(value.strip())
    if not match:
        raise ValueError(f"{field} 必须是 GitHub 仓库根地址，例如 https://github.com/owner/repo")
    return GitHubRepository(match.group("owner"), match.group("repo").removesuffix(".git"))


def repository_metadata(repository: GitHubRepository) -> dict:
    try:
        return json.loads(run(["gh", "api", f"repos/{repository.slug}"]))
    except json.JSONDecodeError as exc:
        raise CommandError(f"无法读取 GitHub 仓库元数据：{repository.url}") from exc


def ensure_fork(source: GitHubRepository, supplied_fork_url: str | None) -> GitHubRepository:
    if supplied_fork_url:
        fork = repository_from_url(supplied_fork_url, "fork-url")
    else:
        login = run(["gh", "api", "user", "--jq", ".login"])
        fork = GitHubRepository(login, source.name)
        attempt = subprocess.run(
            ["gh", "repo", "fork", source.slug, "--clone=false", "--remote=false"],
            text=True,
            capture_output=True,
        )
        if attempt.returncode:
            try:
                repository_metadata(fork)
            except CommandError as exc:
                detail = attempt.stderr.strip() or attempt.stdout.strip()
                raise CommandError(f"创建或复用 fork 失败：{detail}") from exc

    if fork.slug.lower() == source.slug.lower():
        raise ValueError("fork 不能与原始 Repo URL 指向同一仓库")
    metadata = repository_metadata(fork)
    parent = str(metadata.get("parent", {}).get("full_name", "")).lower()
    if not metadata.get("fork") or parent != source.slug.lower():
        raise ValueError(f"{fork.url} 不是 {source.url} 的直接 fork")
    return fork


def check_task_directory(task_dir: Path, baseline: str, allow_unverified: bool) -> None:
    if not task_dir.is_dir():
        raise ValueError(f"任务目录不存在或不是目录：{task_dir}")
    if allow_unverified:
        return
    if not (task_dir / ".git").exists():
        raise ValueError(
            f"任务目录必须是包含基线 Commit 的完整 Git 工作树：{task_dir}；"
            "已人工确认完整源码树时才可使用 --allow-unverified-task-tree"
        )
    run(["git", "-C", str(task_dir), "cat-file", "-e", f"{baseline}^{{commit}}"])
    ancestor_check = subprocess.run(
        ["git", "-C", str(task_dir), "merge-base", "--is-ancestor", baseline, "HEAD"],
        text=True,
        capture_output=True,
    )
    if ancestor_check.returncode:
        raise ValueError(f"任务目录的 HEAD 不包含基线 Commit {baseline}：{task_dir}")


def author_identity(task_dir: Path) -> tuple[str, str]:
    name = run(["git", "-C", str(task_dir), "config", "--get", "user.name"])
    email = run(["git", "-C", str(task_dir), "config", "--get", "user.email"])
    if not name or not email:
        raise ValueError(f"任务目录缺少可用 Git 提交身份：{task_dir}")
    return name, email


def remote_branch_exists(repository_dir: Path, branch: str) -> bool:
    result = subprocess.run(
        ["git", "-C", str(repository_dir), "ls-remote", "--exit-code", "--heads", "origin", branch],
        text=True,
        capture_output=True,
    )
    if result.returncode == 2:
        return False
    if result.returncode:
        detail = result.stderr.strip() or result.stdout.strip()
        raise CommandError(f"无法检查远端分支 {branch}：{detail}")
    return True


def apply_task_changes(source_dir: Path, destination_dir: Path, baseline: str, allow_unverified: bool) -> None:
    if allow_unverified:
        if shutil.which("rsync") is None:
            raise CommandError("找不到 rsync，无法同步人工确认的完整任务源码树")
        run(["rsync", "-a", "--delete", "--exclude=.git/", f"{source_dir}/", f"{destination_dir}/"])
        return

    patch = run(["git", "-C", str(source_dir), "diff", "--binary", baseline], strip_output=False)
    if patch:
        run(["git", "-C", str(destination_dir), "apply", "--binary", "--whitespace=nowarn", "-"], input_text=patch)
    untracked = run(["git", "-C", str(source_dir), "ls-files", "--others", "--exclude-standard", "-z"])
    for relative_path in filter(None, untracked.split("\0")):
        source_path = source_dir / relative_path
        destination_path = destination_dir / relative_path
        if not source_path.is_file():
            continue
        destination_path.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source_path, destination_path)


def publish_task(
    source: GitHubRepository,
    fork: GitHubRepository,
    baseline: str,
    task_dir: Path,
    branch: str,
    message_prefix: str,
    checkout_root: Path,
    allow_unverified: bool,
    author: tuple[str, str],
) -> dict:
    checkout = checkout_root / branch
    run(["git", "clone", "--quiet", fork.url, str(checkout)])
    run(["git", "-C", str(checkout), "config", "user.name", author[0]])
    run(["git", "-C", str(checkout), "config", "user.email", author[1]])
    run(["git", "-C", str(checkout), "remote", "add", "upstream", source.url])
    run(["git", "-C", str(checkout), "fetch", "--quiet", "upstream", baseline])
    base_sha = run(["git", "-C", str(checkout), "rev-parse", "--verify", f"{baseline}^{{commit}}"])
    if remote_branch_exists(checkout, branch):
        raise ValueError(f"远端分支已存在，拒绝重写历史：{branch}")
    run(["git", "-C", str(checkout), "switch", "--create", branch, base_sha])
    apply_task_changes(task_dir, checkout, baseline, allow_unverified)
    run(["git", "-C", str(checkout), "add", "--all"])
    changed = subprocess.run(["git", "-C", str(checkout), "diff", "--cached", "--quiet"])
    if changed.returncode == 0:
        raise ValueError(f"任务目录相对基线没有可提交变更：{task_dir}")
    if changed.returncode != 1:
        raise CommandError(f"无法检查暂存变更：{task_dir}")
    run(["git", "-C", str(checkout), "commit", "-m", f"{message_prefix}: {branch}"])
    task_commit = run(["git", "-C", str(checkout), "rev-parse", "HEAD"])
    run(["git", "-C", str(checkout), "push", "--set-upstream", "origin", branch])
    return {
        "task_dir": str(task_dir),
        "branch": branch,
        "commit": task_commit,
        "commit_url": f"{fork.url}/commit/{task_commit}",
    }


def main() -> None:
    args = parse_args()
    if not 1 <= len(args.task_dir) <= 3:
        raise ValueError("--task-dir 必须提供 1 至 3 次")
    if not COMMIT_PATTERN.fullmatch(args.commit):
        raise ValueError("--commit 必须是原始仓库的 Commit ID")
    if shutil.which("gh") is None:
        raise CommandError("找不到 gh；请先安装并完成 GitHub 登录")
    source = repository_from_url(args.repo_url, "repo-url")
    task_dirs = [Path(value).expanduser().resolve() for value in args.task_dir]
    for task_dir in task_dirs:
        check_task_directory(task_dir, args.commit, args.allow_unverified_task_tree)
    if args.allow_unverified_task_tree:
        author = (run(["git", "config", "--get", "user.name"]), run(["git", "config", "--get", "user.email"]))
        if not all(author):
            raise ValueError("当前环境缺少可用 Git 提交身份；请先配置 user.name 和 user.email")
    else:
        author = author_identity(task_dirs[0])
    fork = ensure_fork(source, args.fork_url)
    with tempfile.TemporaryDirectory(prefix="swe-task-commits-") as directory:
        checkout_root = Path(directory)
        entries = [
            publish_task(
                source,
                fork,
                args.commit,
                task_dir,
                f"task{index}",
                args.commit_message_prefix,
                checkout_root,
                args.allow_unverified_task_tree,
                author,
            )
            for index, task_dir in enumerate(task_dirs, start=1)
        ]
    print(json.dumps({"repo_url": source.url, "commit": args.commit, "fork_url": fork.url, "tasks": entries}, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    try:
        main()
    except (CommandError, ValueError) as exc:
        print(f"错误：{exc}", file=sys.stderr)
        raise SystemExit(1)
