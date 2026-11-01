#!/usr/bin/env python3
"""Flag template-like or spec-like phrasing in a draft SWE task prompt before precheck."""

from __future__ import annotations

import argparse
import json
import re
import sys
from pathlib import Path


MAX_CHARS = 240
MAX_LIST_ITEMS = 3

# 规格腔、宣传腔和“给 Rubric 留钩子”的常见措辞；命中即改写成具体症状。
BANNED_PHRASES = (
    "明确区分",
    "不受影响",
    "可复核",
    "平滑",
    "干净关闭",
    "干净地",
    "互相串",
    "形成闭环",
    "闭环",
    "统一机制",
    "一套机制",
    "一致性",
    "无缝",
    "优雅",
    "全链路",
    "体系化",
    "赋能",
    "保障",
    "确保",
    "有效提升",
    "大幅提升",
)

# 不点名对象的泛称，读起来放到哪个项目都成立。
VAGUE_NOUNS = ("有些数据", "某些数据", "一些设置", "相关功能", "相关数据", "某些场景", "有些场景")

CONSTRAINT_SENTENCE = re.compile(r"不能|不要|不受|不会|不该|不得|要能|也要|仍要|保持")
SENTENCE_SPLIT = re.compile(r"[。！？!?]")
CLAUSE_SPLIT = re.compile(r"[，,；;。！？!?：:\n]")
LIST_TAIL = re.compile(r"、[^、]*?(?:和|或|与|及)")

CAMEL_EXEMPT = {"iPhone", "iPad", "iOS", "macOS", "iCloud", "eBay"}
CAMEL_CASE = re.compile(r"\b[a-z]+(?:[A-Z][a-z0-9]+)+\b")
SNAKE_CASE = re.compile(r"\b[a-z0-9]+_[a-z0-9_]+\b")
FILE_NAME = re.compile(r"\b[\w-]+\.(?:go|py|ts|tsx|js|jsx|java|kt|rs|vue|rb|php|yaml|yml|toml|json|sql|sh)\b")
CALL_SYNTAX = re.compile(r"\b\w+\(\)")
MARKUP = re.compile(r"(?m)^\s*(?:#{1,6} |[-*] |[0-9]+[.)] )|```|`")


def paragraphs(text: str) -> list[str]:
    return [p.strip() for p in re.split(r"\n\s*\n", text) if p.strip()]


def list_runs(text: str) -> list[tuple[str, int]]:
    """Return (clause, item_count) for every clause that enumerates with 、."""
    runs = []
    for clause in CLAUSE_SPLIT.split(text):
        if "、" not in clause:
            continue
        items = clause.count("、") + 1
        if LIST_TAIL.search(clause):
            items += 1
        runs.append((clause.strip(), items))
    return runs


def check(text: str) -> tuple[list[str], list[str]]:
    errors: list[str] = []
    warnings: list[str] = []
    flat = text.strip()

    if len(flat) > MAX_CHARS:
        errors.append(f"题面 {len(flat)} 字，超过预检上限 {MAX_CHARS} 字")
    if MARKUP.search(flat):
        errors.append("题面含 Markdown 列表、标题或反引号")

    runs = list_runs(flat)
    for clause, items in runs:
        if items > MAX_LIST_ITEMS:
            errors.append(f"顿号列举 {items} 项（上限 {MAX_LIST_ITEMS}），改成具体症状：{clause}")
    if len(runs) > 1:
        errors.append(f"顿号列举出现 {len(runs)} 处（上限 1 处），题面像规格清单")

    hope = flat.count("希望")
    if hope > 1:
        errors.append(f"“希望”出现 {hope} 次，每个诉求都用同一个句式开头")

    for phrase in BANNED_PHRASES:
        if phrase in flat:
            errors.append(f"规格腔或宣传腔措辞：{phrase}")

    paras = paragraphs(flat)
    if len(paras) >= 2:
        last = paras[-1]
        sentences = [s for s in SENTENCE_SPLIT.split(last) if s.strip()]
        if sentences and len(last) <= 80 and all(CONSTRAINT_SENTENCE.search(s) for s in sentences):
            errors.append("末段只剩“……不能/不要……”式约束，是给 Rubric 留的兜底段；把约束放回具体场景里")

    for match in CAMEL_CASE.findall(flat):
        if match not in CAMEL_EXEMPT:
            errors.append(f"出现代码标识符：{match}")
    for pattern, label in ((SNAKE_CASE, "代码标识符"), (FILE_NAME, "文件名"), (CALL_SYNTAX, "函数调用写法")):
        for match in pattern.findall(flat):
            errors.append(f"出现{label}：{match}")

    for noun in VAGUE_NOUNS:
        if noun in flat:
            warnings.append(f"泛称“{noun}”没有点名对象，换成项目里的真实数据或操作")
    if len(paras) >= 2 and paras[0].count("。") <= 1 and "希望" in paras[0]:
        warnings.append("首段是“一句痛点 + 希望能”的固定开头，考虑先把具体操作和后果写出来")

    return errors, warnings


SELF_TEST_CASES = (
    (
        "natural-symptoms",
        "每年报税我都得把一年的支出导出来重新算。能抵扣的分类要单独加，退款得从原来那笔里扣掉，"
        "美元日元的消费要按当天汇率折回人民币，信用卡还款和账户之间转钱又不能算进去。"
        "最后总数跟我手算的差几百块，也查不出差在哪几笔。想要一份按年出的税务汇总，"
        "每个数字点进去能看到是哪些流水算出来的，没票据的单独列出来。",
        None,
    ),
    (
        "template-tail",
        "上游偶尔会挂。配置了多个上游的时候，某个上游不可用希望能自动换下一个。\n\n"
        "切换过程中正在进行的下载不要被中断。",
        "末段只剩",
    ),
    (
        "spec-list",
        "希望能生成一份汇总，退款、转账、账户间还款和缺失凭证要分开展示。",
        "顿号列举 4 项",
    ),
    ("banned-phrase", "改了设置以后直接生效，正在进行的请求不受影响。", "不受影响"),
    ("double-hope", "希望能自动清理。希望多台机器结果一致。", "“希望”出现 2 次"),
    ("identifier", "调用 reloadConfig 之后读 config.toml 里的 rate_limit。", "代码标识符"),
    ("too-long", "流水" * 121, "超过预检上限"),
)


def self_test() -> int:
    failed = 0
    for name, text, expected in SELF_TEST_CASES:
        errors, _ = check(text)
        joined = "\n".join(errors)
        ok = not errors if expected is None else expected in joined
        print(f"{name}: {'PASS' if ok else 'FAIL'}")
        if not ok:
            failed += 1
            print(joined or "<no errors>")
    return 0 if failed == 0 else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prompt", type=Path)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()
    if args.self_test:
        return self_test()
    if not args.prompt:
        parser.error("需要 --prompt；也可使用 --self-test")
    text = args.prompt.read_text(encoding="utf-8")
    errors, warnings = check(text)
    print(
        json.dumps(
            {
                "passed": not errors,
                "chars": len(text.strip()),
                "errors": errors,
                "warnings": warnings,
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0 if not errors else 1


if __name__ == "__main__":
    sys.exit(main())
