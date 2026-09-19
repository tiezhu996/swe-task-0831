#!/usr/bin/env python3
"""Hard gate that prevents delivery copy from reusing public Issue wording."""

from __future__ import annotations

import argparse
import json
import re
import unicodedata
from dataclasses import dataclass
from difflib import SequenceMatcher
from pathlib import Path


CJK_RUN = re.compile(r"[\u4e00-\u9fff]{8,}")
ENGLISH_WORD = re.compile(r"[a-z0-9_]+")
COMPACT_CHAR = re.compile(r"[a-z0-9_\u4e00-\u9fff]")


@dataclass(frozen=True)
class IssueText:
    index: int
    title: str
    body: str


def compact(text: str) -> str:
    text = unicodedata.normalize("NFKC", str(text)).lower()
    return "".join(COMPACT_CHAR.findall(text))


def english_tokens(text: str) -> list[str]:
    return ENGLISH_WORD.findall(unicodedata.normalize("NFKC", str(text)).lower())


def character_grams(text: str, size: int = 4) -> set[str]:
    value = compact(text)
    return {value[index : index + size] for index in range(max(0, len(value) - size + 1))}


def english_phrases(text: str, size: int = 5) -> set[str]:
    tokens = english_tokens(text)
    return {" ".join(tokens[index : index + size]) for index in range(max(0, len(tokens) - size + 1))}


def jaccard(left: set[str], right: set[str]) -> float:
    union = left | right
    return len(left & right) / len(union) if union else 0.0


def load_issues(path: Path) -> list[IssueText]:
    raw = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(raw, list) or not raw:
        raise ValueError("issues 必须是非空 JSON 数组")
    issues: list[IssueText] = []
    for index, item in enumerate(raw, start=1):
        if isinstance(item, str):
            issues.append(IssueText(index=index, title="", body=item))
        elif isinstance(item, dict):
            issues.append(IssueText(index=index, title=str(item.get("title") or ""), body=str(item.get("body") or "")))
        else:
            raise ValueError(f"issues 第 {index} 项必须是字符串或包含 title/body 的对象")
    return issues


def longest_shared_compact(left: str, right: str) -> str:
    left_value, right_value = compact(left), compact(right)
    match = SequenceMatcher(None, left_value, right_value, autojunk=False).find_longest_match()
    return left_value[match.a : match.a + match.size]


def shared_cjk_phrases(candidate: str, issue_text: str) -> list[str]:
    issue_value = compact(issue_text)
    candidate_runs = set(CJK_RUN.findall(compact(candidate)))
    return sorted((run for run in candidate_runs if run in issue_value), key=lambda value: (-len(value), value))


def inspect_pair(candidate: str, issue: IssueText) -> dict:
    issue_text = f"{issue.title}\n{issue.body}"
    candidate_compact = compact(candidate)
    issue_compact = compact(issue_text)
    shared_cjk_runs = shared_cjk_phrases(candidate, issue_text)
    shared_english = sorted(english_phrases(candidate) & english_phrases(issue_text))
    title_compact = compact(issue.title)
    exact_title = bool(title_compact and len(title_compact) >= 12 and title_compact in candidate_compact)
    gram_jaccard = jaccard(character_grams(candidate), character_grams(issue_text))
    token_jaccard = jaccard(set(english_tokens(candidate)), set(english_tokens(issue_text)))
    sequence = SequenceMatcher(None, candidate_compact, issue_compact, autojunk=False).ratio()
    reasons: list[str] = []
    if exact_title:
        reasons.append("复用了完整 Issue 标题")
    if shared_cjk_runs:
        reasons.append("复用了至少 8 个连续中文字符")
    if shared_english:
        reasons.append("复用了至少 5 个连续英文 token")
    if gram_jaccard >= 0.18 or sequence >= 0.40:
        reasons.append("整体文本相似度超过硬阈值")
    return {
        "issue_index": issue.index,
        "title_match": exact_title,
        "shared_cjk": shared_cjk_runs[:3],
        "shared_english": shared_english[:3],
        "gram_jaccard": round(gram_jaccard, 3),
        "token_jaccard": round(token_jaccard, 3),
        "sequence": round(sequence, 3),
        "blocked": bool(reasons),
        "reasons": reasons,
    }


def validate_candidates(candidates: dict[str, str], issues: list[IssueText]) -> dict:
    if not candidates:
        raise ValueError("至少提供一个待检文本")
    reports = []
    for label, candidate in candidates.items():
        if not isinstance(candidate, str) or not candidate.strip():
            raise ValueError(f"待检文本不能为空：{label}")
        matches = [inspect_pair(candidate, issue) for issue in issues]
        blocked = [match for match in matches if match["blocked"]]
        maximum = max(matches, key=lambda match: max(match["gram_jaccard"], match["token_jaccard"], match["sequence"]))
        reports.append({
            "label": label,
            "status": "blocked" if blocked else "passed",
            "maximum_similarity": maximum,
            "violations": blocked,
        })
    return {
        "status": "blocked" if any(report["status"] == "blocked" for report in reports) else "passed",
        "issues_checked": len(issues),
        "candidates": reports,
    }


def parse_labeled_file(value: str) -> tuple[str, str]:
    label, separator, file_name = value.partition("=")
    if not separator or not label.strip() or not file_name.strip():
        raise ValueError("--candidate 格式必须为：字段名=文本文件路径")
    return label.strip(), Path(file_name).read_text(encoding="utf-8")


def run_self_test() -> int:
    issues = [
        IssueText(1, "Add a persistent background work queue", "Workers should retry failed jobs after a restart."),
        IssueText(2, "", "设备恢复网络后继续提交同一批操作，服务端不得重复写入记录。"),
    ]
    passed = validate_candidates({"需求 Prompt（原文）": "为管理员增加一份按车辆查看的维护摘要。"}, issues)
    blocked = validate_candidates({"需求 Prompt（原文）": "Please Add a persistent background work queue for administrators."}, issues)
    blocked_cjk = validate_candidates({"真实性与难度说明": "设备恢复网络后继续提交同一批操作时必须保留结果。"}, issues)
    if passed["status"] != "passed" or blocked["status"] != "blocked" or blocked_cjk["status"] != "blocked":
        print(json.dumps({"status": "failed", "passed": passed, "blocked": blocked, "blocked_cjk": blocked_cjk}, ensure_ascii=False, indent=2))
        return 1
    print(json.dumps({"status": "passed", "self_test": True}, ensure_ascii=False))
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--issues", help="GitHub Issue 标题/正文 JSON 数组")
    parser.add_argument("--candidate", action="append", default=[], help="重复传入：字段名=文本文件路径")
    parser.add_argument("--prompt", help="兼容旧调用：单个 Prompt 文本")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        return run_self_test()
    if not args.issues:
        parser.error("--issues 为必填参数")
    candidates = dict(parse_labeled_file(value) for value in args.candidate)
    if args.prompt:
        candidates["需求 Prompt（原文）"] = args.prompt
    report = validate_candidates(candidates, load_issues(Path(args.issues)))
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 1 if report["status"] == "blocked" else 0


if __name__ == "__main__":
    raise SystemExit(main())
