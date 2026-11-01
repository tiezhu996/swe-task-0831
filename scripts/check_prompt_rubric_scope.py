#!/usr/bin/env python3
"""Validate prompt-to-rubric traceability and submission scope before precheck."""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import sys
import tempfile
from pathlib import Path
from typing import Any

import yaml


RUBRIC_TYPES = {"f2p", "p2p"}
QUESTION_TYPES = {
    "有效轮数 > 100",
    "有效轮数 < 100 且 效果差",
    "有效轮数 < 100 且 效果好",
}
COMPLETION_STATES = {"完成", "部分完成", "未完成", "无法判断"}

RISK_RULES: tuple[tuple[str, re.Pattern[str], re.Pattern[str], str], ...] = (
    (
        "observability",
        re.compile(r"日志|状态查询|状态接口|监控|可观测|运行状态"),
        re.compile(r"日志|状态查询|状态接口|监控|可观测|运行状态"),
        "原 Prompt 未要求额外日志、状态接口或监控能力",
    ),
    (
        "performance",
        re.compile(r"性能|吞吐|延迟|可用性|不显著|大表|高并发"),
        re.compile(r"性能|吞吐|延迟|可用性|不显著|大表|高并发"),
        "原 Prompt 未提出对应性能、规模或可用性约束",
    ),
    (
        "idempotency",
        re.compile(r"幂等|重复执行|重试后|去重"),
        re.compile(r"幂等|重复执行|重试|去重"),
        "原 Prompt 未要求重复执行、重试或幂等语义",
    ),
    (
        "filtering",
        re.compile(r"按.+过滤|过滤记录|筛选记录|时间过滤|来源过滤"),
        re.compile(r"过滤|筛选|查询条件"),
        "原 Prompt 未要求额外过滤或筛选入口",
    ),
    (
        "aggregation",
        re.compile(r"跨节点汇总|节点间一致|可汇总|聚合"),
        re.compile(r"汇总|聚合|节点间一致|多台机器.+一致|各节点.+一致"),
        "原 Prompt 未要求跨节点汇总或一致性",
    ),
    (
        "cancellation",
        re.compile(r"取消|中止|中途停止|半成品"),
        re.compile(r"取消|中止|中途停止|半成品"),
        "原 Prompt 未要求取消流程或半成品清理",
    ),
    (
        "nonblocking-write",
        re.compile(r"不中断.*写入|不影响.*写入|正常写入不受影响"),
        re.compile(r"不中断.*写入|不影响.*写入|写入.*不受影响|不阻塞.*写入"),
        "原 Prompt 未要求在线检查时写入完全不受影响",
    ),
    (
        "input-metadata-output",
        re.compile(r"生成时间|来源路径|旧结果(?:的)?来源|结果来源|报告路径|输入路径|输入结果路径"),
        re.compile(r"生成时间|来源路径|旧结果(?:的)?来源|结果来源|报告路径|输入路径"),
        "原 Prompt 只要求使用输入，不要求回显输入的路径、生成时间或其他元数据",
    ),
    (
        "unmatched-category",
        re.compile(r"无法匹配|未知分类|未识别分类|无法识别的?.{0,4}数量"),
        re.compile(r"无法匹配|未知|未识别|无法识别"),
        "原 Prompt 未要求增加“无法匹配/未知”分类",
    ),
    (
        "preflight-subject-expansion",
        re.compile(r"(?:输出|给出|列出|报告).{0,16}命名空间(?:的)?检查(?:结果|项)|命名空间.{0,8}(?:检查结果|检查项)"),
        re.compile(r"命名空间"),
        "原 Prompt 未要求把命名空间加入额外预检输出",
    ),
    (
        "existing-statement-listing",
        re.compile(r"(?:逐条)?列出.{0,12}已存在语句|已存在语句.{0,12}(?:逐条)?列出"),
        re.compile(r"已存在语句|已有语句|现有语句"),
        "原 Prompt 未要求逐条列出已存在语句",
    ),
    (
        "crash-recovery",
        re.compile(r"进程(?:退出|崩溃|被杀)|下一次运行|持久化阶段记录|跨运行|崩溃恢复|恢复日志|事务日志"),
        re.compile(r"进程(?:退出|崩溃|被杀)|下一次运行|持久化阶段记录|跨运行|崩溃恢复|恢复日志|事务日志"),
        "原 Prompt 只要求当前运行失败时回滚，不代表要求跨进程崩溃恢复或持久化事务日志",
    ),
    (
        "scope-isolation",
        re.compile(r"用户级|项目级|组间隔离|作用域隔离|全局设置.+项目|项目.+全局设置"),
        re.compile(r"用户级|项目级|组间隔离|作用域隔离|全局设置.+项目|项目.+全局设置"),
        "原 Prompt 未要求用户级与项目级作用域分组或组间隔离",
    ),
    (
        # 2026-10-31 5131 v2：Prompt 只要求“调用方能发出停止请求”，Rubric 写成“发起查询时得到句柄”。
        "api-shape",
        re.compile(
            r"(?:停止|取消|中止|撤销|控制)(?:句柄|令牌|token)"
            r"|(?:句柄|令牌|token|回调).{0,12}(?:请求)?(?:停止|取消|中止)"
            r"|(?:发起|启动|提交|调用).{0,8}(?:得到|返回|拿到)(?:一个)?.{0,6}(?:句柄|令牌|控制器|对象)"
            r"|\b(?:cancel|stop|abort)\s*\(\s*\)",
            re.IGNORECASE,
        ),
        re.compile(r"句柄|令牌|token|回调|返回.{0,8}(?:对象|控制器)|\b(?:cancel|stop|abort)\s*\(", re.IGNORECASE),
        "原 Prompt 只要求调用方能发出请求，未规定由返回的句柄、预建令牌、回调或特定方法提供",
    ),
    (
        # 2026-10-31 5131 v1：Prompt 只要求结果说清终态，Rubric 要求文字和机器可读结果带终态与行数。
        "serialization-content",
        re.compile(
            r"(?:序列化|机器可读|文字(?:和|与|、)机器可读).{0,16}(?:包含|携带|带上|写入|输出|显示|给出).{0,12}(?:终态|状态|行数|计数|数量|总数)"
            r"|(?:终态|状态).{0,8}(?:和|与|及)行数"
        ),
        re.compile(r"序列化|机器可读|JSON|CSV|XML|导出|行数|条数|多少行", re.IGNORECASE),
        "原 Prompt 只要求结果说明状态，未要求序列化或机器可读输出携带终态、行数或计数",
    ),
)

IMPLEMENTATION_LOCATORS = re.compile(
    r"resolver|handler|middleware|函数|方法名|类名|包名|文件名|接口名",
    re.IGNORECASE,
)
UNQUANTIFIED_QUALITY = re.compile(
    r"不显著|适当|合理|尽量|较快|足够快|明显更|大幅|稳定地.*不"
)

# 出题深度门禁（2026-10-26 按原始轨迹校准）：有效轮数约 2/3 是思考、1/3 是工具调用，
# 工具调用主要来自分散的改动处和“改-跑-改”循环。过 100 轮的会话工具调用都在 34 次以上、
# Edit 都在 10 次以上；见 references/ROUND_DEPTH_DESIGN.md 与 scripts/trajectory_metrics.py。
MIN_EDIT_SITES = 12
MIN_EDIT_FILES = 4
# 上限（2026-10-30）：登记的改动处超过 30 个或分布超过 10 个文件，说明题出大了，不是出深了；
# 3378/1527 这类 230 轮以上的题都是铺开改很多入口。宁可拆成两道题。
MAX_EDIT_SITES = 30
MAX_EDIT_FILES = 10
MIN_STOP_TRAPS = 2
MIN_PREDICTED_LOW = 100
MIN_DESIGN_RUBRICS = 8
BASELINE_TEST_STATES = {"pass", "known_failures"}
# 效果落点（2026-10-30）：TraeCN 执行前会二次确认“要不要实现对应的接口、页面和测试”，
# 题面没说清效果在哪看到时，一点确认就多出页面和关联业务。设计阶段登记效果落点和允许新增的入口。
EFFECT_NEW_KINDS = {"page", "endpoint", "command", "setting"}


def normalize_space(value: str) -> str:
    return re.sub(r"\s+", " ", value).strip()


def failure(message: str) -> dict[str, str]:
    return {"message": message}


def load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except Exception as exc:
        raise ValueError(f"无法读取 JSON {path}: {exc}") from exc


def verify_artifact(entry: Any) -> str | None:
    if not isinstance(entry, dict):
        return "证据项必须是包含 path、sha256、bytes 的对象"
    path = Path(str(entry.get("path", "")))
    expected_sha = str(entry.get("sha256", ""))
    expected_bytes = entry.get("bytes")
    if not path.is_absolute() or not path.is_file():
        return f"证据文件不存在或不是绝对路径: {path}"
    data = path.read_bytes()
    if hashlib.sha256(data).hexdigest() != expected_sha:
        return f"证据文件哈希不一致: {path}"
    if not isinstance(expected_bytes, int) or expected_bytes != len(data):
        return f"证据文件字节数不一致: {path}"
    return None


def non_empty_strings(value: Any) -> list[str]:
    if not isinstance(value, list):
        return []
    return [str(item).strip() for item in value if str(item).strip()]


def split_location(location: str) -> tuple[str, int | None]:
    path, sep, line = location.rpartition(":")
    if sep and line.isdigit() and path:
        return path, int(line)
    return location, None


def validate_depth(depth: Any, clause_ids: set[str], rubric_count: int, source: Path | None) -> list[str]:
    """Check the design-time depth gate; see references/ROUND_DEPTH_DESIGN.md."""
    if not isinstance(depth, dict):
        return ["design 阶段必须填写 depth（出题深度门禁），见 references/ROUND_DEPTH_DESIGN.md"]
    errors: list[str] = []

    sites = depth.get("edit_sites")
    if not isinstance(sites, list):
        sites = []
    locations: set[str] = set()
    files: set[str] = set()
    for site in sites:
        if not isinstance(site, dict):
            errors.append("depth.edit_sites 条目必须是对象")
            continue
        location = str(site.get("location", "")).strip()
        path, line = split_location(location)
        if line is None:
            errors.append(f"改动处 location 必须是“基线相对路径:行号”: {location or '<empty>'}")
            continue
        if not str(site.get("reason", "")).strip():
            errors.append(f"改动处 {location} 缺少 reason：这里为什么非改不可")
            continue
        if source is not None:
            target = source / path
            if not target.is_file():
                errors.append(f"改动处 {location} 在基线源码中不存在")
                continue
            if line < 1 or line > len(target.read_text(encoding="utf-8", errors="replace").splitlines()):
                errors.append(f"改动处 {location} 的行号超出文件长度")
                continue
        locations.add(location)
        files.add(path)
    if len(locations) < MIN_EDIT_SITES or len(files) < MIN_EDIT_FILES:
        errors.append(
            f"改动处需要至少 {MIN_EDIT_SITES} 个、分布在至少 {MIN_EDIT_FILES} 个文件，"
            f"当前 {len(locations)} 个、{len(files)} 个文件；改动集中在一处的题历史上不到 100 轮"
        )
    elif len(locations) > MAX_EDIT_SITES or len(files) > MAX_EDIT_FILES:
        errors.append(
            f"改动处最多 {MAX_EDIT_SITES} 个、{MAX_EDIT_FILES} 个文件，当前 {len(locations)} 个、"
            f"{len(files)} 个文件；铺得太开会把轮数推过 200，收窄范围或拆成两道题"
        )

    traps = depth.get("stop_traps")
    if not isinstance(traps, list):
        traps = []
    valid_traps = 0
    for trap in traps:
        if not isinstance(trap, dict):
            errors.append("depth.stop_traps 条目必须是对象")
            continue
        refs = non_empty_strings(trap.get("clause_ids"))
        missing = [ref for ref in refs if ref not in clause_ids]
        if not refs or missing:
            errors.append(f"停止陷阱必须用 clause_ids 回指题面条款: {missing or '未填写'}")
            continue
        if not str(trap.get("looks_done", "")).strip() or not str(trap.get("exposed_by", "")).strip():
            errors.append("停止陷阱需要写明 looks_done（看起来完成的实现）和 exposed_by（怎样跑出它的错）")
            continue
        if trap.get("local") is not True:
            errors.append("停止陷阱必须能在本机离线跑出来（local=true），否则模型发现不了，会提前收尾")
            continue
        valid_traps += 1
    if valid_traps < MIN_STOP_TRAPS:
        errors.append(f"至少需要 {MIN_STOP_TRAPS} 个有效停止陷阱，当前 {valid_traps} 个")

    prediction = depth.get("predicted_rounds")
    low = prediction.get("low") if isinstance(prediction, dict) else None
    high = prediction.get("high") if isinstance(prediction, dict) else None
    if not all(isinstance(v, int) and not isinstance(v, bool) for v in (low, high)) or high < low:
        errors.append("depth.predicted_rounds 必须是 {low, high} 整数区间，执行前登记")
    elif low < MIN_PREDICTED_LOW:
        errors.append(f"预测轮数下限 {low} 低于 {MIN_PREDICTED_LOW}，按平台收录规则不值得执行")

    health = depth.get("baseline_health")
    if not isinstance(health, dict):
        errors.append("depth.baseline_health 必须记录基线构建、测试和外部依赖")
    else:
        if health.get("build") != "pass":
            errors.append("baseline_health.build 必须为 pass：基线本机构建不过时模型会提前收尾")
        tests = health.get("tests")
        if tests not in BASELINE_TEST_STATES:
            errors.append("baseline_health.tests 必须是 pass 或 known_failures")
        elif tests == "known_failures" and not non_empty_strings(health.get("known_failures")):
            errors.append("baseline_health.tests=known_failures 时必须逐项列出 known_failures")
        external = health.get("external_deps")
        if not isinstance(external, list):
            errors.append("baseline_health.external_deps 必须是列表，没有时写空列表")
        elif non_empty_strings(external):
            errors.append(
                "验收依赖外部模型下载、硬件或云账号时模型无法在本机跑通，换题: "
                + "、".join(non_empty_strings(external))
            )
        evidence = health.get("evidence")
        if not isinstance(evidence, list) or not evidence:
            errors.append("baseline_health.evidence 不能为空")
        else:
            for entry in evidence:
                if isinstance(entry, dict):
                    problem = verify_artifact(entry)
                    if problem:
                        errors.append(f"baseline_health: {problem}")
                elif not isinstance(entry, str) or not entry.strip():
                    errors.append("baseline_health 的证据必须是非空字符串或文件摘要")

    if rubric_count < MIN_DESIGN_RUBRICS:
        errors.append(f"新题设计阶段 Rubric 至少 {MIN_DESIGN_RUBRICS} 条，当前 {rubric_count} 条")
    return errors


def validate_effect(effect: Any, prompt: str) -> list[str]:
    """Design-time check that the prompt says where the user sees the result."""
    if not isinstance(effect, dict):
        return ["design 阶段必须填写 effect（效果落点），见 references/ROUND_DEPTH_DESIGN.md 第 5 节"]
    errors = []
    if not non_empty_strings(effect.get("seen_in")):
        errors.append("effect.seen_in 要写用户在哪个现有功能、命令或页面上看到结果")
    quote = effect.get("quote")
    if not isinstance(quote, str) or not quote.strip():
        errors.append("effect.quote 要逐字摘出题面里交代效果落点的那句话")
    elif normalize_space(quote) not in normalize_space(prompt):
        errors.append("effect.quote 不在题面原文里；题面本身要说清效果在哪看到，不能只写在合同里")
    allowed = effect.get("allowed_new")
    if not isinstance(allowed, list) or not all(isinstance(item, str) for item in allowed):
        errors.append("effect.allowed_new 必须是列表，不新增入口时写空列表")
    else:
        unknown = sorted(set(allowed) - EFFECT_NEW_KINDS)
        if unknown:
            errors.append(f"effect.allowed_new 只能取 {sorted(EFFECT_NEW_KINDS)}，出现了 {unknown}")
    return errors


def prediction_outcome(contract: Any) -> dict[str, Any] | None:
    if not isinstance(contract, dict):
        return None
    depth = contract.get("depth")
    submission = contract.get("submission")
    if not isinstance(depth, dict) or not isinstance(submission, dict):
        return None
    prediction = depth.get("predicted_rounds")
    actual = submission.get("effective_rounds")
    if not isinstance(prediction, dict) or not isinstance(actual, int):
        return None
    low, high = prediction.get("low"), prediction.get("high")
    if not isinstance(low, int) or not isinstance(high, int):
        return None
    return {"predicted": [low, high], "actual": actual, "within_range": low <= actual <= high}


def validate_contract(prompt: str, rubric_data: Any, contract: Any, source: Path | None = None) -> list[str]:
    errors: list[str] = []
    if not isinstance(contract, dict):
        return ["范围合同必须是 JSON 对象"]
    if contract.get("schema_version") != 1:
        errors.append("范围合同 schema_version 必须为 1")

    clauses = contract.get("prompt_clauses")
    if not isinstance(clauses, list) or not clauses:
        errors.append("prompt_clauses 至少需要一条")
        clauses = []
    clause_by_id: dict[str, dict[str, Any]] = {}
    prompt_flat = normalize_space(prompt)
    for clause in clauses:
        if not isinstance(clause, dict):
            errors.append("prompt_clauses 中的条目必须是对象")
            continue
        clause_id = str(clause.get("id", "")).strip()
        quote = normalize_space(str(clause.get("quote", "")))
        if not clause_id or clause_id in clause_by_id:
            errors.append(f"Prompt 条款 ID 为空或重复: {clause_id or '<empty>'}")
            continue
        if not quote or quote not in prompt_flat:
            errors.append(f"Prompt 条款 {clause_id} 的 quote 未逐字出现在题面中")
        clause_by_id[clause_id] = clause

    if not isinstance(rubric_data, dict) or not isinstance(rubric_data.get("rubrics"), list):
        errors.append("Rubric 必须是顶层 rubrics YAML 列表")
        rubrics: list[dict[str, Any]] = []
    else:
        rubrics = rubric_data["rubrics"]
    if len(rubrics) < 5:
        errors.append("Rubric 至少需要 5 条")
    ids: list[int] = []
    rubric_by_id: dict[int, dict[str, Any]] = {}
    for index, rubric in enumerate(rubrics, start=1):
        if not isinstance(rubric, dict):
            errors.append(f"第 {index} 条 Rubric 不是对象")
            continue
        rid = rubric.get("id")
        if not isinstance(rid, int) or isinstance(rid, bool):
            errors.append(f"第 {index} 条 Rubric id 必须是整数")
            continue
        ids.append(rid)
        rubric_by_id[rid] = rubric
        if rubric.get("type") not in RUBRIC_TYPES:
            errors.append(f"Rubric {rid} type 必须是 f2p 或 p2p")
        if not str(rubric.get("text", "")).strip():
            errors.append(f"Rubric {rid} text 不能为空")
    if ids != list(range(1, len(rubrics) + 1)):
        errors.append("Rubric id 必须从 1 连续递增")
    if not any(r.get("type") == "f2p" for r in rubrics if isinstance(r, dict)):
        errors.append("Rubric 至少需要一条 f2p")

    mappings = contract.get("rubric_map")
    if not isinstance(mappings, list):
        errors.append("rubric_map 必须是列表")
        mappings = []
    mapped_ids: list[int] = []
    covered_clauses: set[str] = set()
    for item in mappings:
        if not isinstance(item, dict):
            errors.append("rubric_map 条目必须是对象")
            continue
        rid = item.get("id")
        if rid not in rubric_by_id:
            errors.append(f"rubric_map 引用了不存在的 Rubric: {rid}")
            continue
        mapped_ids.append(rid)
        clause_ids = item.get("clause_ids")
        if not isinstance(clause_ids, list) or not clause_ids:
            errors.append(f"Rubric {rid} 未映射任何 Prompt 条款")
            clause_ids = []
        for clause_id in clause_ids:
            clause_id = str(clause_id)
            if clause_id not in clause_by_id:
                errors.append(f"Rubric {rid} 引用了不存在的 Prompt 条款 {clause_id}")
            else:
                covered_clauses.add(clause_id)
        if not str(item.get("observable", "")).strip():
            errors.append(f"Rubric {rid} 的 observable 不能为空")
    if sorted(mapped_ids) != sorted(rubric_by_id):
        errors.append("rubric_map 必须恰好覆盖每个 Rubric 一次")
    for clause_id, clause in clause_by_id.items():
        if clause.get("required", True) and clause_id not in covered_clauses:
            errors.append(f"必需 Prompt 条款 {clause_id} 没有任何 Rubric 覆盖")

    novelty = contract.get("novelty")
    if not isinstance(novelty, dict) or novelty.get("status") != "confirmed":
        errors.append("novelty.status 必须为 confirmed，先证明固定基线没有等价能力")
    else:
        checks = novelty.get("checks")
        if not isinstance(checks, list) or len(checks) < 2:
            errors.append("novelty.checks 至少需要两个独立检查项")
            checks = []
        areas: set[str] = set()
        for check in checks:
            if not isinstance(check, dict):
                errors.append("novelty.checks 条目必须是对象")
                continue
            area = str(check.get("area", "")).strip()
            result = str(check.get("result", "")).strip()
            areas.add(area)
            if result not in {"absent", "gap_confirmed"}:
                errors.append(f"novelty 检查 {area or '<empty>'} 的 result 无效")
            evidence = check.get("evidence")
            if not isinstance(evidence, list) or not evidence:
                errors.append(f"novelty 检查 {area or '<empty>'} 缺少证据")
                continue
            for entry in evidence:
                if isinstance(entry, dict):
                    problem = verify_artifact(entry)
                    if problem:
                        errors.append(f"novelty 检查 {area}: {problem}")
                elif not isinstance(entry, str) or not entry.strip():
                    errors.append(f"novelty 检查 {area} 的证据必须是非空字符串或文件摘要")
        if not areas.intersection({"runtime_behavior", "tests", "public_api"}):
            errors.append("novelty 必须包含 runtime_behavior、tests 或 public_api 证据")

    submission = contract.get("submission")
    if not isinstance(submission, dict):
        errors.append("submission 必须是对象")
    else:
        phase = submission.get("phase", "design")
        if phase not in {"design", "final"}:
            errors.append("submission.phase 必须是 design 或 final")
        # 新题在 design 阶段必须过深度门禁；2026-10-26 前的旧合同没有 depth，
        # 到 final 阶段只在填写了 depth 时复核。
        if phase == "design" or "depth" in contract:
            errors.extend(validate_depth(contract.get("depth"), set(clause_by_id), len(rubrics), source))
        # 2026-10-30 前已过预检的合同（如 5133）没有 effect，只在 design 阶段新跑门禁时要求。
        if phase == "design" or "effect" in contract:
            errors.extend(validate_effect(contract.get("effect"), prompt))
        if phase == "final":
            rounds = submission.get("effective_rounds")
            question_type = submission.get("question_type")
            completed = submission.get("requirement_completed")
            if not isinstance(rounds, int) or isinstance(rounds, bool) or rounds < 0:
                errors.append("submission.effective_rounds 必须是非负整数")
                rounds = None
            if question_type not in QUESTION_TYPES:
                errors.append("submission.question_type 不在当前平台枚举中")
            if completed not in COMPLETION_STATES:
                errors.append("submission.requirement_completed 不在当前平台枚举中")
            if rounds == 100:
                errors.append("恰好 100 轮没有匹配的平台 Type，必须停止并人工确认")
            if rounds is not None and rounds < 100 and completed == "完成":
                errors.append("当前平台拒收有效轮数 <100 且状态为“完成”的样本，不能靠改 Type 绕过")
            if rounds is not None and rounds > 100 and question_type != "有效轮数 > 100":
                errors.append("有效轮数 >100 时 Type 必须为“有效轮数 > 100”")
            if rounds is not None and rounds < 100 and question_type not in {
                "有效轮数 < 100 且 效果差",
                "有效轮数 < 100 且 效果好",
            }:
                errors.append("有效轮数 <100 时 Type 必须使用 <100 枚举")

    for rid, rubric in rubric_by_id.items():
        text = str(rubric.get("text", ""))
        for category, trigger, prompt_support, message in RISK_RULES:
            if trigger.search(text) and not prompt_support.search(prompt_flat):
                errors.append(f"Rubric {rid} 越界[{category}]：{message}")
        if IMPLEMENTATION_LOCATORS.search(text):
            errors.append(f"Rubric {rid} 绑定了实现位置或内部符号：{text}")
        if UNQUANTIFIED_QUALITY.search(text):
            errors.append(f"Rubric {rid} 含不可量化的质量措辞：{text}")

    return errors


def parse_artifact_result(text: str, rubric_ids: list[int], completion: str, question_type: str) -> list[str]:
    errors: list[str] = []
    seen: dict[int, str] = {}
    for raw in text.splitlines():
        line = raw.strip()
        if not line:
            continue
        match = re.fullmatch(r"(\d+) (通过|未通过)(?: (.*))?", line)
        if not match:
            errors.append(f"产物结果格式不合法: {line}")
            continue
        rid = int(match.group(1))
        verdict = match.group(2)
        reason = (match.group(3) or "").strip()
        if rid in seen:
            errors.append(f"产物结果重复编号: {rid}")
        seen[rid] = verdict
        if verdict == "未通过" and not reason:
            errors.append(f"产物结果 {rid} 未通过但缺少原因")
    if sorted(seen) != sorted(rubric_ids):
        errors.append("产物结果必须与 Rubric id 逐项且唯一对应")
    has_failure = any(value == "未通过" for value in seen.values())
    if has_failure and completion == "完成":
        errors.append("存在未通过 Rubric 时不能填写“完成”")
    if question_type == "有效轮数 < 100 且 效果差" and not has_failure:
        errors.append("“效果差”必须至少有一个可复现的未通过 Rubric")
    if question_type == "有效轮数 < 100 且 效果好" and completion != "完成":
        errors.append("“效果好”样本必须已完成需求")
    return errors


def analyze(
    prompt_path: Path,
    rubric_path: Path,
    contract_path: Path,
    artifact_path: Path | None,
    source: Path | None = None,
) -> tuple[list[str], dict[str, Any]]:
    prompt = prompt_path.read_text(encoding="utf-8")
    rubric_data = yaml.safe_load(rubric_path.read_text(encoding="utf-8"))
    contract = load_json(contract_path)
    errors = validate_contract(prompt, rubric_data, contract, source)
    rubrics = rubric_data.get("rubrics", []) if isinstance(rubric_data, dict) else []
    rubric_ids = [r["id"] for r in rubrics if isinstance(r, dict) and isinstance(r.get("id"), int)]
    if artifact_path is not None:
        submission = contract.get("submission", {}) if isinstance(contract, dict) else {}
        if submission.get("phase", "design") != "final":
            errors.append("传入 --artifact-result 时必须把 submission.phase 设为 final")
        errors.extend(
            parse_artifact_result(
                artifact_path.read_text(encoding="utf-8"),
                rubric_ids,
                str(submission.get("requirement_completed", "")),
                str(submission.get("question_type", "")),
            )
        )
    summary = {
        "contract_sha256": hashlib.sha256(contract_path.read_bytes()).hexdigest(),
        "prompt_clauses": len(contract.get("prompt_clauses", [])) if isinstance(contract, dict) else 0,
        "rubrics": len(rubrics),
        "f2p": sum(1 for r in rubrics if isinstance(r, dict) and r.get("type") == "f2p"),
        "p2p": sum(1 for r in rubrics if isinstance(r, dict) and r.get("type") == "p2p"),
        "errors": len(errors),
    }
    outcome = prediction_outcome(contract)
    if outcome is not None:
        # 预测命中与否只用于校准门禁阈值，不阻断提交。
        summary["round_prediction"] = outcome
    return errors, summary


def self_test() -> int:
    prompt = (
        "接口偶尔会返回旧数据。希望读取时能返回最新版本，并保持旧调用兼容。"
    )
    valid_rubrics = {
        "rubrics": [
            {"id": 1, "type": "f2p", "text": "读取请求返回最新版本数据。"},
            {"id": 2, "type": "f2p", "text": "并发读取不会返回旧版本数据。"},
            {"id": 3, "type": "f2p", "text": "读取结果包含当前版本标识。"},
            {"id": 4, "type": "f2p", "text": "更新后的下一次读取能看到新值。"},
            {"id": 5, "type": "p2p", "text": "旧调用方式继续兼容。"},
        ]
    }
    valid_contract = {
        "schema_version": 1,
        "prompt_clauses": [
            {"id": "P1", "quote": "读取时能返回最新版本", "required": True},
            {"id": "P2", "quote": "保持旧调用兼容", "required": True},
        ],
        "rubric_map": [
            {"id": 1, "clause_ids": ["P1"], "observable": "读回值"},
            {"id": 2, "clause_ids": ["P1"], "observable": "并发读回值"},
            {"id": 3, "clause_ids": ["P1"], "observable": "版本标识"},
            {"id": 4, "clause_ids": ["P1"], "observable": "更新后读回值"},
            {"id": 5, "clause_ids": ["P2"], "observable": "旧调用响应"},
        ],
        "novelty": {
            "status": "confirmed",
            "checks": [
                {"area": "runtime_behavior", "result": "gap_confirmed", "evidence": ["baseline-read-returns-old"]},
                {"area": "tests", "result": "absent", "evidence": ["baseline tests do not cover freshness"]},
            ],
        },
        "submission": {
            "phase": "final",
            "effective_rounds": 120,
            "question_type": "有效轮数 > 100",
            "requirement_completed": "完成",
        },
    }
    cases = [
        ("valid", prompt, valid_rubrics, valid_contract, ""),
        (
            "observability-overreach",
            prompt,
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "可通过日志观察旧调用仍兼容。"}]},
            valid_contract,
            "observability",
        ),
        (
            "implementation-location",
            prompt,
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "旧调用在 resolver 中保持兼容。"}]},
            valid_contract,
            "实现位置",
        ),
        (
            "idempotency-overreach",
            prompt,
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "重复执行旧调用保持幂等。"}]},
            valid_contract,
            "idempotency",
        ),
        (
            "filtering-overreach",
            prompt,
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "可按来源或时间过滤记录。"}]},
            valid_contract,
            "filtering",
        ),
        (
            "aggregation-overreach",
            prompt,
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "记录在节点间一致并可汇总。"}]},
            valid_contract,
            "aggregation",
        ),
        (
            "performance-overreach",
            prompt,
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "检查大表时不显著影响可用性。"}]},
            valid_contract,
            "performance",
        ),
        (
            "nonblocking-overreach",
            prompt,
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "检查过程不中断正常写入。"}]},
            valid_contract,
            "nonblocking-write",
        ),
        (
            "input-metadata-overreach",
            prompt,
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "输出上一轮报告的来源路径和生成时间。"}]},
            valid_contract,
            "input-metadata-output",
        ),
        (
            "unmatched-category-overreach",
            prompt,
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "输出无法匹配数量。"}]},
            valid_contract,
            "unmatched-category",
        ),
        (
            "preflight-subject-overreach",
            "导入前给出图、语句和空白节点的检查结果。",
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "f2p", "text": "导入前输出命名空间的检查结果。"}]},
            valid_contract,
            "preflight-subject-expansion",
        ),
        (
            "existing-statement-overreach",
            "成功后列出实际新增和替换的内容。",
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "f2p", "text": "变化清单逐条列出已存在语句。"}]},
            valid_contract,
            "existing-statement-listing",
        ),
        (
            "crash-recovery-overreach",
            "一次给几款助手安装设置，中途失败要回到运行前的样子。",
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "f2p", "text": "进程退出后，下一次运行根据持久化阶段记录恢复完整状态。"}]},
            valid_contract,
            "crash-recovery",
        ),
        (
            "scope-isolation-overreach",
            "一次给几款助手安装设置。",
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "f2p", "text": "用户级和项目级设置分别判断并保持组间隔离。"}]},
            valid_contract,
            "scope-isolation",
        ),
        (
            "api-shape-overreach",
            "希望调用方能发出停止请求，查询在下一批结果前停下。",
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "f2p", "text": "停止句柄：发起查询时得到一个句柄，调用方可在结果尚未完成时请求停止。"}]},
            valid_contract,
            "api-shape",
        ),
        (
            "api-shape-neutral",
            prompt + "希望调用方能发出停止请求，查询在下一批结果前停下。",
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "f2p", "text": "停止请求：查询尚未完成时，调用方能够对它发出停止请求，停止入口的形式不限。"}]},
            valid_contract,
            "",
        ),
        (
            "resource-handle-not-api-shape",
            prompt + "导入失败后不能留下临时数据。",
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "f2p", "text": "导入失败后本次临时数据和输入句柄得到清理。"}]},
            valid_contract,
            "",
        ),
        (
            "serialization-content-overreach",
            "结果要说清是正常结束、用户停止还是执行失败。",
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "f2p", "text": "序列化一致：文字和机器可读结果包含同一终态与行数。"}]},
            valid_contract,
            "serialization-content",
        ),
        (
            "serialization-regression-allowed",
            prompt + "结果要说清是正常结束、用户停止还是执行失败。",
            {**valid_rubrics, "rubrics": valid_rubrics["rubrics"][:-1] + [{"id": 5, "type": "p2p", "text": "结果格式回归：原有结果行、绑定值和序列化格式保持兼容。"}]},
            valid_contract,
            "",
        ),
        (
            "short-completed",
            prompt,
            valid_rubrics,
            {**valid_contract, "submission": {"phase": "final", "effective_rounds": 81, "question_type": "有效轮数 < 100 且 效果好", "requirement_completed": "完成"}},
            "拒收有效轮数 <100",
        ),
        (
            "exact-100",
            prompt,
            valid_rubrics,
            {**valid_contract, "submission": {"phase": "final", "effective_rounds": 100, "question_type": "有效轮数 > 100", "requirement_completed": "完成"}},
            "恰好 100 轮",
        ),
        (
            "novelty-missing",
            prompt,
            valid_rubrics,
            {**valid_contract, "novelty": {"status": "unchecked", "checks": []}},
            "novelty.status",
        ),
    ]
    design_rubrics = {
        "rubrics": valid_rubrics["rubrics"][:4]
        + [
            {"id": 5, "type": "f2p", "text": "第二个入口读取同样返回最新版本。"},
            {"id": 6, "type": "f2p", "text": "第三个入口读取同样返回最新版本。"},
            {"id": 7, "type": "f2p", "text": "两个后端读回的版本相同。"},
            {"id": 8, "type": "p2p", "text": "旧调用方式继续兼容。"},
        ]
    }
    design_sites = [
        {"location": f"pkg/{name}.go:{line}", "reason": "读取路径要换成最新版本"}
        for name in ("read", "list", "get", "pull")
        for line in (3, 6, 9)
    ]
    design_depth = {
        "edit_sites": design_sites,
        "stop_traps": [
            {
                "clause_ids": ["P1"],
                "looks_done": "只改了主读取入口",
                "exposed_by": "并发写入后从列表入口读回",
                "local": True,
            },
            {
                "clause_ids": ["P2"],
                "looks_done": "新字段让旧客户端解析失败",
                "exposed_by": "用基线客户端测试调用新服务",
                "local": True,
            },
        ],
        "predicted_rounds": {"low": 110, "high": 180},
        "baseline_health": {
            "build": "pass",
            "tests": "pass",
            "external_deps": [],
            "evidence": ["go build ./... && go test ./... 基线全绿"],
        },
    }
    design_contract = {
        **valid_contract,
        "rubric_map": [
            {"id": rid, "clause_ids": ["P2" if rid == 8 else "P1"], "observable": "读回值"}
            for rid in range(1, 9)
        ],
        "depth": design_depth,
        "effect": {"seen_in": ["现有读取接口的返回"], "quote": "希望读取时能返回最新版本", "allowed_new": []},
        "submission": {"phase": "design"},
    }
    cases += [
        (
            "effect-missing",
            prompt,
            design_rubrics,
            {k: v for k, v in design_contract.items() if k != "effect"},
            "必须填写 effect",
        ),
        (
            "effect-quote-not-in-prompt",
            prompt,
            design_rubrics,
            {**design_contract, "effect": {**design_contract["effect"], "quote": "在设置页上看到版本号"}},
            "不在题面原文里",
        ),
        (
            "effect-unknown-kind",
            prompt,
            design_rubrics,
            {**design_contract, "effect": {**design_contract["effect"], "allowed_new": ["dashboard"]}},
            "allowed_new 只能取",
        ),
        ("depth-valid", prompt, design_rubrics, design_contract, ""),
        (
            "depth-missing",
            prompt,
            design_rubrics,
            {k: v for k, v in design_contract.items() if k != "depth"},
            "必须填写 depth",
        ),
        (
            "depth-concentrated",
            prompt,
            design_rubrics,
            {**design_contract, "depth": {**design_depth, "edit_sites": design_sites[:3]}},
            "改动处需要至少 12 个",
        ),
        (
            "depth-too-wide",
            prompt,
            design_rubrics,
            {
                **design_contract,
                "depth": {
                    **design_depth,
                    "edit_sites": [
                        {"location": f"pkg/m{i}.go:{line}", "reason": "读取路径要换成最新版本"}
                        for i in range(11)
                        for line in (3, 6, 9)
                    ],
                },
            },
            "改动处最多 30 个",
        ),
        (
            "depth-trap-not-local",
            prompt,
            design_rubrics,
            {
                **design_contract,
                "depth": {
                    **design_depth,
                    "stop_traps": [design_depth["stop_traps"][0], {**design_depth["stop_traps"][1], "local": False}],
                },
            },
            "local=true",
        ),
        (
            "depth-low-prediction",
            prompt,
            design_rubrics,
            {**design_contract, "depth": {**design_depth, "predicted_rounds": {"low": 60, "high": 90}}},
            "预测轮数下限 60",
        ),
        (
            "depth-external-deps",
            prompt,
            design_rubrics,
            {
                **design_contract,
                "depth": {
                    **design_depth,
                    "baseline_health": {**design_depth["baseline_health"], "external_deps": ["语音识别模型下载"]},
                },
            },
            "外部模型下载",
        ),
        (
            "depth-few-rubrics",
            prompt,
            valid_rubrics,
            {**design_contract, "rubric_map": valid_contract["rubric_map"]},
            "Rubric 至少 8 条",
        ),
    ]
    failed = 0
    with tempfile.TemporaryDirectory(prefix="swe-scope-selftest.") as tmp:
        root = Path(tmp)
        source = root / "baseline"
        (source / "pkg").mkdir(parents=True)
        for name in ("read", "list", "get", "pull"):
            (source / "pkg" / f"{name}.go").write_text("\n" * 10, encoding="utf-8")
        for name, sites, expected in (
            ("source-valid", design_sites, ""),
            ("source-missing-file", design_sites[:-1] + [{"location": "pkg/gone.go:1", "reason": "x"}], "不存在"),
            ("source-line-range", design_sites[:-1] + [{"location": "pkg/read.go:99", "reason": "x"}], "行号超出"),
        ):
            errors = validate_depth({**design_depth, "edit_sites": sites}, {"P1", "P2"}, 8, source)
            text = "\n".join(errors)
            ok = not errors if not expected else expected in text
            print(f"{name}: {'PASS' if ok else 'FAIL'}")
            if not ok:
                failed += 1
                print(text)
        prompt_path = root / "PROMPT.md"
        prompt_path.write_text(prompt, encoding="utf-8")
        for name, case_prompt, rubrics, contract, expected in cases:
            prompt_path.write_text(case_prompt, encoding="utf-8")
            rubric_path = root / f"{name}.yaml"
            contract_path = root / f"{name}.json"
            rubric_path.write_text(yaml.safe_dump(rubrics, allow_unicode=True, sort_keys=False), encoding="utf-8")
            contract_path.write_text(json.dumps(contract, ensure_ascii=False), encoding="utf-8")
            errors, _ = analyze(prompt_path, rubric_path, contract_path, None)
            text = "\n".join(errors)
            ok = not errors if not expected else expected in text
            print(f"{name}: {'PASS' if ok else 'FAIL'}")
            if not ok:
                failed += 1
                print(text)
    artifact_ok = not parse_artifact_result(
        "\n".join(f"{rid} 通过" for rid in range(1, 6)),
        list(range(1, 6)),
        "完成",
        "有效轮数 > 100",
    )
    artifact_bad = bool(
        parse_artifact_result(
            "1 通过\n2 未通过\n3 通过\n4 通过",
            list(range(1, 5)),
            "完成",
            "有效轮数 > 100",
        )
    )
    for name, ok in (("artifact-result", artifact_ok), ("artifact-mismatch", artifact_bad)):
        print(f"{name}: {'PASS' if ok else 'FAIL'}")
        if not ok:
            failed += 1
    return 0 if failed == 0 else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prompt", type=Path)
    parser.add_argument("--rubric", type=Path)
    parser.add_argument("--contract", type=Path)
    parser.add_argument("--artifact-result", type=Path)
    parser.add_argument("--source", type=Path, help="固定基线源码树，用于核对 depth.edit_sites 的位置")
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        return self_test()
    if not args.prompt or not args.rubric or not args.contract:
        parser.error("需要 --prompt、--rubric 和 --contract；也可使用 --self-test")
    try:
        errors, summary = analyze(args.prompt, args.rubric, args.contract, args.artifact_result, args.source)
    except Exception as exc:
        print(json.dumps({"passed": False, "error": str(exc)}, ensure_ascii=False))
        return 2
    print(
        json.dumps(
            {
                "passed": not errors,
                **summary,
                "failures": [failure(item) for item in errors],
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0 if not errors else 1


if __name__ == "__main__":
    sys.exit(main())
