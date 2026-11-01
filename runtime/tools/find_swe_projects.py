#!/usr/bin/env python3
"""Read GitHub project candidates without cloning or creating platform records."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile


QUERY = """
query ProjectCandidates($searchQuery: String!, $first: Int!) {
  search(query: $searchQuery, type: REPOSITORY, first: $first) {
    repositoryCount
    pageInfo { hasNextPage endCursor }
    nodes {
      ... on Repository {
        nameWithOwner
        url
        description
        stargazerCount
        isArchived
        isFork
        primaryLanguage { name }
        issueTotal: issues(states: [OPEN, CLOSED]) { totalCount }
        pushedAt
        licenseInfo { spdxId name url }
        defaultBranchRef {
          name
          target { ... on Commit { oid committedDate } }
        }
      }
    }
  }
}
"""


def validate_options(language, limit, stars_min, stars_max):
    if not re.fullmatch(r"[A-Za-z][A-Za-z0-9#+. -]{0,30}", language):
        raise ValueError("语言名称不合法")
    if not isinstance(limit, int) or isinstance(limit, bool) or not 1 <= limit <= 50:
        raise ValueError("候选数量必须为 1 至 50")
    if not 1 <= stars_min <= stars_max <= 1_000_000:
        raise ValueError("stars 范围不合法")


def find_projects(language="Go", limit=10, stars_min=1500, stars_max=3000, runner=None):
    validate_options(language, limit, stars_min, stars_max)
    search_query = (
        f'language:"{language}" stars:{stars_min}..{stars_max} '
        "archived:false fork:false sort:updated-desc"
    )
    command = [
        "gh", "api", "graphql", "-f", "query=" + QUERY,
        "-f", "searchQuery=" + search_query, "-F", "first=" + str(limit),
    ]
    if runner is None:
        runner = subprocess.run
    try:
        response = runner(command, capture_output=True, text=True, timeout=60, check=False)
    except FileNotFoundError as exc:
        raise ValueError("未找到 gh CLI") from exc
    except subprocess.TimeoutExpired as exc:
        raise ValueError("GitHub 只读查询超时；未写入任何平台记录") from exc
    if response.returncode != 0:
        raise ValueError("GitHub 只读查询失败，请检查 gh 登录状态与网络")
    try:
        envelope = json.loads(response.stdout)
        if envelope.get("errors"):
            raise ValueError("GitHub 返回 GraphQL 错误，未接受部分候选")
        search = envelope["data"]["search"]
        nodes = search["nodes"]
        if not isinstance(nodes, list) or len(nodes) > limit:
            raise ValueError("GitHub 返回的候选数量不符合查询限制")
        projects = []
        for node in nodes:
            if not node:
                continue
            stars = node["stargazerCount"]
            issue_count = node["issueTotal"]["totalCount"]
            if type(stars) is not int or type(issue_count) is not int or issue_count < 0:
                raise ValueError("GitHub 候选的计数字段不合法")
            if node["isArchived"] or node["isFork"] or not stars_min <= stars <= stars_max:
                raise ValueError("GitHub 候选不符合搜索条件")
            branch = node["defaultBranchRef"]
            commit = branch["target"] if branch else None
            sha = commit.get("oid") if commit else None
            if sha is not None and not re.fullmatch(r"[0-9a-f]{40}", sha):
                raise ValueError("GitHub 候选基线 commit 不合法")
            if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", node["nameWithOwner"]):
                raise ValueError("GitHub 候选仓库标识不合法")
            if node["url"] != "https://github.com/" + node["nameWithOwner"]:
                raise ValueError("GitHub 候选仓库地址不合法")
            projects.append({
                "repo": node["nameWithOwner"], "repo_url": node["url"],
                "description": node["description"], "stars": stars,
                "issue_total_count": issue_count,
                "primary_language": (node["primaryLanguage"] or {}).get("name"),
                "default_branch": branch["name"] if branch else None,
                "baseline_commit": sha,
                "baseline_committed_at": commit.get("committedDate") if commit else None,
                "pushed_at": node["pushedAt"], "license": node["licenseInfo"],
                "ready_for_checkout": sha is not None,
            })
        projects.sort(key=lambda row: (
            row["issue_total_count"], abs(row["stars"] - 2000), row["repo"],
        ))
        return {
            "schema_version": 1,
            "source": "github_graphql_readonly",
            "collected_at": datetime.now(timezone.utc).isoformat(),
            "language": language,
            "search_query": search_query,
            "limit": limit,
            "repository_count": search["repositoryCount"],
            "has_more": search["pageInfo"]["hasNextPage"],
            "ranking": "issue_total_count_ascending_within_bounded_search",
            "issue_count_semantics": "OPEN+CLOSED issues; pull requests excluded",
            "network_writes": 0,
            "projects": projects,
        }
    except (KeyError, TypeError, json.JSONDecodeError) as exc:
        raise ValueError("GitHub 返回结构不完整，未写入候选文件") from exc


def load_used_projects():
    """The skill's used-project table module; this tool may run from a copied runtime."""
    for scripts in (Path(__file__).resolve().parents[2] / "scripts", Path.home() / ".codex/skills/swe-task/scripts"):
        if (scripts / "used_projects.py").is_file():
            sys.path.insert(0, str(scripts))
            import used_projects
            return used_projects
    raise ValueError("找不到已用项目排除表工具 scripts/used_projects.py；确认不排除时加 --include-used")


def exclude_used(result, used_projects, table=None, roots=None):
    """Drop projects that already had tasks; they are never picked again (2026-10-31)."""
    kept, excluded = [], []
    for row in result["projects"]:
        hit = used_projects.lookup(row["repo"], table or used_projects.TABLE, roots)
        if hit:
            excluded.append({"repo": row["repo"], "matched_by": hit["matched_by"], "tasks": hit.get("tasks") or []})
        else:
            kept.append(row)
    return {**result, "projects": kept, "excluded_used": excluded}


def save_candidates(path, result):
    """Publish an fsynced file without replacing an existing candidate record."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".swe-projects-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as output:
            data = (json.dumps(result, ensure_ascii=False, indent=2) + "\n").encode("utf-8")
            output.write(data)
            output.flush()
            os.fsync(output.fileno())
        os.link(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--language", default="Go")
    parser.add_argument("--limit", type=int, default=10)
    parser.add_argument("--stars-min", type=int, default=1500)
    parser.add_argument("--stars-max", type=int, default=3000)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--used-projects", type=Path, help="已用项目排除表，默认技能 data/used-projects.json")
    parser.add_argument("--workflow-root", type=Path, action="append", help="可重复；默认标准 workflow 根目录")
    parser.add_argument("--include-used", action="store_true", help="不排除已用项目（只用于核对）")
    args = parser.parse_args()
    if args.out.exists() or args.out.is_symlink():
        parser.exit(1, "project_search=BLOCKED 候选记录已存在，禁止覆盖\n")
    try:
        result = find_projects(args.language, args.limit, args.stars_min, args.stars_max)
        if not args.include_used:
            result = exclude_used(result, load_used_projects(), args.used_projects, args.workflow_root)
        save_candidates(args.out, result)
    except (ValueError, OSError) as exc:
        message = str(exc) if isinstance(exc, ValueError) else "候选文件保存失败或已存在"
        parser.exit(1, "project_search=BLOCKED " + message + "\n")
    print(json.dumps({
        "source": result["source"], "language": result["language"],
        "candidate_count": len(result["projects"]), "network_writes": 0,
        "candidates": [{"repo": row["repo"], "stars": row["stars"], "issues": row["issue_total_count"]} for row in result["projects"]],
        "excluded_used": [row["repo"] for row in result.get("excluded_used", [])],
    }, ensure_ascii=False))


if __name__ == "__main__":
    main()
