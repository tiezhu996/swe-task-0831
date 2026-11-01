#!/usr/bin/env python3
"""Validate the local review record for a hard SWE task.

The checker is intentionally local and read-only. It validates the schema and
the prompt back-references; it cannot prove that a corpus record is genuine or
that a remote commit contains the declared changes without an optional git
range, so those fields must carry explicit evidence for human review.
"""
from __future__ import annotations

import argparse
import json
import re
import subprocess
import sys
from pathlib import Path
from typing import Any


SIGNALS = ("多模块整合", "关键设计取舍", "复杂技术关注点")
TECHNICAL_KINDS = {
    "并发", "一致性", "故障恢复", "幂等", "资源生命周期", "协议兼容",
    "性能", "安全", "数据迁移", "分布式", "可观测性",
}
SUBSTANTIVE_KINDS = {
    "状态一致性", "失败恢复", "并发冲突", "持久化边界", "资源生命周期",
    "兼容性", "跨模块协作", "权限边界",
}
TOPOLOGY_KINDS = {
    "跨系统对账", "失败恢复", "迁移兼容", "并发冲突", "权限边界",
    "容量约束", "离线合并", "独立所有权拆分",
}
HARD_WORDS = re.compile(r"必须|不得|需要|应当|务必|至少|不能|不可|禁止")
MARKDOWN = re.compile(r"(^|\n)\s{0,3}(?:#|[-*+] |\d+[.)] )|[`*_\[\]]")
PATH_OR_CODE = re.compile(
    r"(?:/|\\)|\b(?:API|JSON|HTTP|HTTPS|SQL|Go|Docker|commit|token|endpoint|schema|CLI|SDK)\b",
    re.IGNORECASE,
)


def s(value: Any) -> str:
    return str(value or "").strip()


def entries(value: Any) -> list[dict[str, Any]]:
    if isinstance(value, list):
        return [item for item in value if isinstance(item, dict)]
    if isinstance(value, dict):
        return [item for item in value.values() if isinstance(item, dict)]
    return []


def first(obj: dict[str, Any], *names: str) -> Any:
    for name in names:
        if name in obj:
            return obj[name]
    return None


def check(review: dict[str, Any], prompt: str, expected_difficulty: str, expected_verdict: str) -> list[str]:
    errors: list[str] = []

    def require(condition: bool, message: str) -> None:
        if not condition:
            errors.append(message)

    def integer(value: Any, label: str) -> int:
        try:
            return int(value or 0)
        except (TypeError, ValueError):
            errors.append(f"{label} 必须是数字")
            return 0

    require(review.get("schemaVersion") == 1, "schemaVersion 必须为 1")
    require(s(review.get("difficulty")) == expected_difficulty, "difficulty 与命令行不一致")
    require(s(review.get("verdict")) == expected_verdict, "verdict 与命令行不一致")
    require(bool(s(review.get("reviewedBy"))), "reviewedBy 不能为空")
    require(review.get("routineOnly") is False, "routineOnly 必须为 false")

    require(len(prompt) <= 240, "题面超过 240 字")
    require(not MARKDOWN.search(prompt), "题面包含 Markdown 标记")
    require(not PATH_OR_CODE.search(prompt), "题面包含文件路径或代码术语")
    require(len(HARD_WORDS.findall(prompt)) <= 3, "题面硬性措辞超过 3 处")
    require(prompt.count(";") + prompt.count("；") <= 2, "题面分号超过 2 个")

    signal_entries = entries(review.get("signals"))
    passed = {s(e.get("name") or e.get("kind") or e.get("label")) for e in signal_entries if e.get("passed") is True}
    require(len(passed & set(SIGNALS)) >= 2, "signals 至少两项 passed=true")
    require(bool(passed & {"关键设计取舍", "复杂技术关注点"}), "signals 必须包含关键设计取舍或复杂技术关注点")
    for name in SIGNALS:
        item = next((e for e in signal_entries if s(e.get("name") or e.get("kind") or e.get("label")) == name), None)
        if not item or item.get("passed") is not True:
            continue
        if name == "多模块整合":
            require(len(item.get("modules") or []) >= 3, "多模块整合 modules 至少 3 个")
            require(len(s(item.get("crossModuleInvariant"))) >= 20, "多模块整合 crossModuleInvariant 少于 20 字")
            require(len(s(item.get("evidence"))) >= 20, "多模块整合 evidence 少于 20 字")
        elif name == "关键设计取舍":
            for field in ("conflict", "decision", "whyNotRoutine", "evidence"):
                require(len(s(item.get(field))) >= 20, f"关键设计取舍 {field} 少于 20 字")
        else:
            require(set(item.get("kinds") or []) <= TECHNICAL_KINDS and bool(item.get("kinds")), "复杂技术关注点 kinds 不在 11 类枚举内")
            for field in ("technicalRisk", "observableFailure", "evidence"):
                require(len(s(item.get(field))) >= 20, f"复杂技术关注点 {field} 少于 20 字")

    platform = entries(review.get("platformSignals"))
    require(len(platform) >= 3, "platformSignals 必须有三项")
    for index, item in enumerate(platform[:3], 1):
        require(len(s(item.get("answer"))) >= 20, f"platformSignals[{index}].answer 少于 20 字")
        quote = s(item.get("promptQuote"))
        require(len(quote) >= 12 and quote in prompt, f"platformSignals[{index}].promptQuote 未逐字回指题面")
        require(len(s(item.get("ifViolated"))) >= 12, f"platformSignals[{index}].ifViolated 少于 12 字")

    invariant = review.get("crossObjectInvariant") or {}
    objects = first(invariant, "objects", "crossObjects") or []
    require(len(objects) >= 2, "crossObjectInvariant 至少需要两个题面对象")
    for obj in objects:
        require(s(obj) in prompt, f"crossObjectInvariant 对象未出现在题面: {obj!r}")
    require(len(s(invariant.get("invariant"))) >= 20, "crossObjectInvariant.invariant 少于 20 字")
    require(len(s(invariant.get("divergenceFailure"))) >= 12, "crossObjectInvariant.divergenceFailure 少于 12 字")

    corpus = review.get("corpusEvidence") or {}
    deprecated = first(corpus, "deprecated", "废弃样本")
    accepted = first(corpus, "passed", "accepted", "通过样本")
    for label, item in (("废弃样本", deprecated), ("通过样本", accepted)):
        require(isinstance(item, dict), f"corpusEvidence 缺少{label}")
        if isinstance(item, dict):
            require(bool(s(first(item, "historyId", "id", "recordId"))), f"{label} 缺少历史记录 ID")
            require(len(s(item.get("difference"))) >= 20, f"{label} difference 少于 20 字")
            require(len(s(item.get("takeaway"))) >= 20, f"{label} takeaway 少于 20 字")

    substantive = review.get("substantiveComplexity") or {}
    kinds = substantive.get("kinds") or []
    require(bool(kinds) and set(kinds) <= SUBSTANTIVE_KINDS, "substantiveComplexity.kinds 不在 8 类枚举内")
    quote = s(substantive.get("promptQuote"))
    require(len(quote) >= 12 and quote in prompt, "substantiveComplexity.promptQuote 未逐字回指题面")
    require(len(s(substantive.get("whyHard"))) >= 20, "substantiveComplexity.whyHard 少于 20 字")
    require(len(s(substantive.get("visibleFailure"))) >= 12, "substantiveComplexity.visibleFailure 少于 12 字")
    if re.search(r"台账|清单|草稿|备份|并回|合并到一处|汇总到一处", prompt):
        require(len(s(review.get("mediumShapeDefense"))) >= 20, "命中中等形态时 mediumShapeDefense 少于 20 字")

    floor = review.get("p3DifficultyFloor") or {}
    for key in ("修改范围", "上下文依赖", "交互轮次", "技术广度"):
        item = floor.get(key) or floor.get({"修改范围": "scope", "上下文依赖": "contextDependency", "交互轮次": "interactionRounds", "技术广度": "technicalBreadth"}[key]) or {}
        require(item.get("hit") is True, f"p3DifficultyFloor.{key} 必须 hit=true")
        q = s(item.get("promptQuote"))
        require(len(q) >= 12 and q in prompt, f"p3DifficultyFloor.{key}.promptQuote 未回指题面")
        require(len(s(item.get("counterEvidence") or item.get("counterargument"))) >= 20, f"p3DifficultyFloor.{key} 反证少于 20 字")

    architecture = review.get("crossModuleOrArchitecture") or {}
    modules = architecture.get("modules") or []
    require(len(modules) >= 2 and all(s(m) in prompt for m in modules), "crossModuleOrArchitecture.modules 必须是题面中的至少两个对象")
    q = s(architecture.get("quote"))
    require(len(q) >= 12 and q in prompt, "crossModuleOrArchitecture.quote 未回指题面")
    require(len(s(architecture.get("whyArchitecture"))) >= 30, "crossModuleOrArchitecture.whyArchitecture 少于 30 字")
    context = review.get("repoContextDependency") or {}
    require(len(s(context.get("whatMustBeRead"))) >= 20, "repoContextDependency.whatMustBeRead 少于 20 字")
    require(len(s(context.get("evidence"))) >= 20, "repoContextDependency.evidence 少于 20 字")
    require(len(s((review.get("oneRoundEstimate") or {}).get("why"))) >= 30, "oneRoundEstimate.why 少于 30 字")
    prompt_hit_names = {
        name
        for name, aliases in {
            "修改范围": ("修改范围", "scope"),
            "上下文依赖": ("上下文依赖", "contextDependency"),
            "交互轮次": ("交互轮次", "interactionRounds"),
            "技术广度": ("技术广度", "technicalBreadth"),
        }.items()
        if any((floor.get(alias) or {}).get("hit") is True for alias in aliases)
    }
    self_hit_names = {
        s(item.get("name") if isinstance(item, dict) else item)
        for item in (review.get("selfReportedSignals") or [])
    }
    self_hit_names &= set(prompt_hit_names) | {"修改范围", "上下文依赖", "交互轮次", "技术广度"}
    if len(prompt_hit_names | self_hit_names) >= 2:
        require(architecture.get("passed") is True, "题面与自报命中达到 2 项时跨架构必须 passed=true")
        require(review.get("canModelFinishInOneRound") is False, "题面与自报命中达到 2 项时不能一轮完成")

    topology = review.get("complexityTopology") or {}
    require(topology.get("kind") in TOPOLOGY_KINDS, "complexityTopology.kind 不在 8 类枚举内")
    q = s(topology.get("promptQuote"))
    require(len(q) >= 12 and q in prompt, "complexityTopology.promptQuote 未回指题面")
    owners = topology.get("stateOwners") or []
    require(len(owners) >= 2 and all(s(owner) in prompt for owner in owners), "complexityTopology.stateOwners 必须是题面中的至少两个对象")
    ownership = topology.get("ownership") or []
    require(len(ownership) >= 2, "complexityTopology.ownership 至少两侧")
    owned_objects: list[str] = []
    for side in ownership:
        require(bool(s(side.get("holder"))) and bool(s(side.get("exclusiveObjects"))), "ownership 必须写持有方和独占对象")
        owned_objects.extend([s(obj) for obj in (side.get("exclusiveObjects") or [])])
    require(len(owned_objects) == len(set(owned_objects)), "ownership 两侧不能共用业务对象")
    require(len(s(topology.get("failureOrRecovery"))) >= 20, "complexityTopology.failureOrRecovery 少于 20 字")
    require(len(s(topology.get("whyNotLocalRuleList"))) >= 30, "complexityTopology.whyNotLocalRuleList 少于 30 字")
    require(len(s(topology.get("negativeOutcome"))) >= 12, "complexityTopology.negativeOutcome 少于 12 字")

    changes = review.get("changes") or review.get("changeSets") or []
    labels = {s(item.get("label")) for item in changes if isinstance(item, dict)}
    require({"A", "B"} <= labels, "changes 必须同时声明 A/B")
    for item in changes:
        if not isinstance(item, dict) or s(item.get("label")) not in {"A", "B"}:
            continue
        require(integer(item.get("businessLines"), f"{item.get('label')} businessLines") >= 30, f"{item.get('label')} 非测试业务代码少于 30 行")
        require(len(item.get("businessFiles") or []) >= 3, f"{item.get('label')} 跨业务文件少于 3 个")
        require(bool(s(item.get("remoteCommit") or item.get("commit"))), f"{item.get('label')} 缺少远端 commit 证据")

    return errors


def git_change_stats(repo: Path, base: str, head: str) -> tuple[int, int, str | None]:
    """Return added non-test lines, business files, and an error message."""
    try:
        result = subprocess.run(
            ["git", "diff", "--numstat", f"{base}..{head}", "--"],
            cwd=repo,
            check=True,
            capture_output=True,
            text=True,
        )
    except (OSError, subprocess.CalledProcessError) as exc:
        detail = getattr(exc, "stderr", "") or str(exc)
        return 0, 0, f"git diff {base}..{head} 失败: {detail.strip()}"
    added = 0
    files = 0
    for line in result.stdout.splitlines():
        fields = line.split("\t", 2)
        if len(fields) != 3:
            continue
        add_text, _deleted, path = fields
        normalized = path.lower().replace("\\", "/")
        if normalized.endswith("_test.go") or "/test/" in f"/{normalized}" or normalized.startswith("tests/"):
            continue
        if add_text.isdigit():
            added += int(add_text)
        files += 1
    return added, files, None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--review", type=Path, required=True)
    parser.add_argument("--prompt", type=Path, required=True)
    parser.add_argument("--difficulty", required=True)
    parser.add_argument("--verdict", required=True)
    parser.add_argument("--repo", type=Path, help="可选：本地 Git 工作树，用于从 commit 复算 A/B 改动量")
    parser.add_argument("--base", help="可选：A/B 共用的基线 commit")
    parser.add_argument("--head-a", help="可选：A 远端 commit")
    parser.add_argument("--head-b", help="可选：B 远端 commit")
    args = parser.parse_args()
    try:
        review = json.loads(args.review.read_text(encoding="utf-8"))
        prompt = args.prompt.read_text(encoding="utf-8").strip()
    except (OSError, json.JSONDecodeError) as exc:
        print(f"INVALID: {exc}", file=sys.stderr)
        return 2
    if not isinstance(review, dict):
        print("INVALID: review 根节点必须是 JSON 对象", file=sys.stderr)
        return 2
    errors = check(review, prompt, args.difficulty, args.verdict)
    if any((args.repo, args.base, args.head_a, args.head_b)) and not all((args.repo, args.base, args.head_a, args.head_b)):
        errors.append("Git 改动量复算必须同时提供 --repo、--base、--head-a、--head-b")
    elif all((args.repo, args.base, args.head_a, args.head_b)):
        for label, head in (("A", args.head_a), ("B", args.head_b)):
            added, files, git_error = git_change_stats(args.repo, args.base, head)
            if git_error:
                errors.append(f"{label}: {git_error}")
            elif added < 30 or files < 3:
                errors.append(f"{label}: Git 复算得到 {added} 行、{files} 个非测试业务文件，未达到 30 行/3 文件")
    if errors:
        print("HARD_TASK_GATE: FAIL")
        for error in errors:
            print(f"- {error}")
        return 1
    print("HARD_TASK_GATE: PASS")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
