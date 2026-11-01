#!/usr/bin/env python3
"""Check that a draft task adds a feature that no public issue or PR, and no other task on the
same project, already asks for.

``check_prompt_independence.py`` only catches reused wording. An English issue that
requests the same capability in different words passes it, so this gate works on
features instead. One project yields 8-10 tasks designed in batches of 3-4, so:

- ``fetch`` saves every issue and PR (title + body) of the repo as one corpus JSON;
  the corpus is also a valid ``--issues`` input for ``check_prompt_independence.py``.
- ``index`` checks the project-level feature index: one sentence per issue saying what it
  asks for, written once and reused by every task on the project. It reports numbers a
  re-fetched corpus added.
- ``skeleton`` writes a per-task ledger with every issue (and every PR the search terms
  hit) pre-filled from the index and ``relation`` left empty.
- ``check`` requires that ledger to classify each of those items and each sibling task
  (other prompts of the same repo found under the workflow root, plus ``--sibling`` drafts)
  as unrelated, adjacent or overlap. Any overlap fails: the feature must be redesigned,
  not reworded.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import re
import subprocess
import sys
import tempfile
from pathlib import Path


RELATIONS = {"unrelated", "adjacent", "overlap"}
MIN_TERMS = 6
MIN_ASCII_TERMS = 3
MIN_REASON_CHARS = 15
PROMPT_NAME = "PROMPT.md"


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def fetch(repo: str, out: Path, runner=subprocess.run) -> list[dict]:
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        raise ValueError("--repo 必须是 owner/name")
    if out.exists():
        raise ValueError(f"{out} 已存在，不覆盖")
    response = runner(
        ["gh", "api", "--paginate", f"repos/{repo}/issues?state=all&per_page=100",
         "--jq", ".[] | {number, title, body, state, url: .html_url, is_pull_request: (.pull_request != null)}"],
        capture_output=True, text=True, timeout=300, check=False,
    )
    if response.returncode != 0:
        raise ValueError("GitHub 只读查询失败，请检查 gh 登录状态与网络")
    items = [json.loads(line) for line in response.stdout.splitlines() if line.strip()]
    if not items:
        raise ValueError("仓库没有任何 Issue 或 PR，无法证明功能不重合；确认仓库后再试")
    items.sort(key=lambda item: item["number"])
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_text(json.dumps(items, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    return items


def load_corpus(path: Path) -> tuple[list[dict], str]:
    raw = path.read_bytes()
    corpus = json.loads(raw)
    if not isinstance(corpus, list) or not corpus:
        raise ValueError("语料必须是 fetch 生成的非空数组")
    return corpus, sha256(raw)


def load_features(path: Path | None) -> dict[int, str]:
    """Project-level index: {"features": {"<number>": "它要什么"}}."""
    if path is None:
        return {}
    data = json.loads(path.read_text(encoding="utf-8"))
    return {int(k): str(v).strip() for k, v in (data.get("features") or {}).items() if str(v).strip()}


def check_index(corpus: list[dict], features: dict[int, str]) -> list[str]:
    missing = [item["number"] for item in corpus if not item.get("is_pull_request") and item["number"] not in features]
    if not missing:
        return []
    shown = ", ".join(f"#{n}" for n in missing[:20])
    return [f"功能索引缺 {len(missing)} 条 Issue: {shown}{' …' if len(missing) > 20 else ''}"]


def term_pattern(term: str) -> re.Pattern[str]:
    if re.search(r"[A-Za-z]", term):
        return re.compile(r"(?<![A-Za-z0-9])" + re.escape(term) + r"(?![A-Za-z0-9])", re.IGNORECASE)
    return re.compile(re.escape(term))


def term_hits(corpus: list[dict], terms: list[str]) -> tuple[dict[int, list[str]], dict[str, int]]:
    hits: dict[int, list[str]] = {}
    counts: dict[str, int] = {}
    for term in terms:
        pattern = term_pattern(term)
        counts[term] = 0
        for item in corpus:
            if pattern.search(f"{item.get('title') or ''}\n{item.get('body') or ''}"):
                hits.setdefault(item["number"], []).append(term)
                counts[term] += 1
    return hits, counts


def discover_siblings(workflow_root: Path, repo: str, own_prompt: Path) -> dict[str, str]:
    """同项目其他任务的冻结题面：task-state 里出现该仓库、目录下有 PROMPT.md。"""
    target = f"github.com/{repo}".lower()
    own = own_prompt.resolve()
    siblings = {}
    if not workflow_root.is_dir():
        return siblings
    for state_path in sorted(workflow_root.glob("*/task-state.json")):
        prompt = state_path.parent / PROMPT_NAME
        try:
            text = state_path.read_text(encoding="utf-8")
        except OSError:
            continue
        urls = {u.lower().removesuffix(".git") for u in re.findall(r"https://(github\.com/[\w.-]+/[\w.-]+)", text)}
        if target in urls and prompt.is_file() and prompt.resolve() != own:
            siblings[state_path.parent.name] = prompt.read_text(encoding="utf-8")
    return siblings


def skeleton(corpus: list[dict], corpus_sha: str, features: dict[int, str], terms: list[str]) -> dict:
    hits, _ = term_hits(corpus, terms)
    entries = []
    for item in corpus:
        number = item["number"]
        if item.get("is_pull_request") and number not in hits:
            continue
        entries.append({
            "number": number,
            "title": item.get("title") or "",
            "kind": "pr" if item.get("is_pull_request") else "issue",
            "hit_terms": hits.get(number, []),
            "feature": features.get(number, ""),
            "relation": "",
            "reason": "",
        })
    return {
        "schema_version": 1,
        "corpus_sha256": corpus_sha,
        "prompt_sha256": "",
        "feature": "",
        "search_terms": terms,
        "entries": entries,
        "siblings": [],
    }


def check_relation(label: str, entry: dict, needs_reason: bool, errors: list[str]) -> str | None:
    relation = entry.get("relation")
    if relation not in RELATIONS:
        errors.append(f"{label} relation 必须是 unrelated、adjacent 或 overlap")
        return None
    reason = str(entry.get("reason", "")).strip()
    if (relation == "adjacent" or needs_reason) and len(reason) < MIN_REASON_CHARS:
        errors.append(f"{label} 是相邻功能或命中检索词，reason 要写清它要的和本题要的分界（至少 {MIN_REASON_CHARS} 字）")
    if relation == "overlap":
        errors.append(f"{label} 与本题功能重合：{entry.get('feature', '')}。换需求，不能靠改写措辞绕过")
    return relation


def check(
    corpus_path: Path,
    ledger: dict,
    prompt_text: str,
    features: dict[int, str] | None = None,
    siblings: dict[str, str] | None = None,
) -> tuple[list[str], dict]:
    features = features or {}
    siblings = siblings or {}
    errors: list[str] = []
    try:
        corpus, corpus_sha = load_corpus(corpus_path)
    except ValueError as exc:
        return [str(exc)], {}
    if ledger.get("schema_version") != 1:
        errors.append("台账 schema_version 必须为 1")
    if ledger.get("corpus_sha256") != corpus_sha:
        errors.append("台账 corpus_sha256 与语料文件不一致；语料更新后要重新逐条分类")
    if ledger.get("prompt_sha256") != sha256(prompt_text.encode("utf-8")):
        errors.append("台账 prompt_sha256 与题面不一致；题面改过要重新核对")
    if not str(ledger.get("feature", "")).strip():
        errors.append("台账 feature 必须用一句话写明本题要新增的能力")

    terms = [str(t).strip() for t in ledger.get("search_terms", []) if str(t).strip()]
    ascii_terms = {t for t in terms if re.search(r"[A-Za-z]", t)}
    if len(set(terms)) < MIN_TERMS or len(ascii_terms) < MIN_ASCII_TERMS:
        errors.append(
            f"search_terms 至少 {MIN_TERMS} 个、其中英文至少 {MIN_ASCII_TERMS} 个"
            f"（Issue 多为英文，要覆盖同义说法），当前 {len(set(terms))} 个、英文 {len(ascii_terms)} 个"
        )
    hits, counts = term_hits(corpus, terms)

    by_number = {item["number"]: item for item in corpus}
    entries: dict[int, dict] = {}
    for entry in ledger.get("entries", []):
        number = entry.get("number") if isinstance(entry, dict) else None
        if number not in by_number:
            errors.append(f"台账条目引用了语料里不存在的编号: {number}")
            continue
        if number in entries:
            errors.append(f"台账重复分类 #{number}")
            continue
        entries[number] = entry
        if not (str(entry.get("feature", "")).strip() or features.get(number)):
            errors.append(f"#{number} 缺少 feature：一句话写明该 Issue/PR 要的是什么（写在条目或功能索引里）")
        check_relation(f"#{number}", entry, number in hits, errors)

    missing_issues = [n for n, item in by_number.items() if not item.get("is_pull_request") and n not in entries]
    if missing_issues:
        shown = ", ".join(f"#{n}" for n in missing_issues[:20])
        errors.append(f"有 {len(missing_issues)} 条 Issue 没有逐条分类: {shown}{' …' if len(missing_issues) > 20 else ''}")
    missing_hits = [n for n in hits if n not in entries]
    if missing_hits:
        errors.append("检索词命中但未分类的 PR/Issue: " + ", ".join(f"#{n}" for n in sorted(missing_hits)))

    sibling_entries: dict[str, dict] = {}
    for entry in ledger.get("siblings", []):
        name = entry.get("name") if isinstance(entry, dict) else None
        if name not in siblings:
            errors.append(f"siblings 里的 {name} 不是本项目的其他题，或题面没找到")
            continue
        if name in sibling_entries:
            errors.append(f"台账重复分类同项目题 {name}")
            continue
        sibling_entries[name] = entry
        if entry.get("prompt_sha256") != sha256(siblings[name].encode("utf-8")):
            errors.append(f"同项目题 {name} 的 prompt_sha256 不一致；它的题面改过，要重新核对")
        if not str(entry.get("feature", "")).strip():
            errors.append(f"同项目题 {name} 缺少 feature：一句话写明那道题新增什么")
        # 同项目的题共用代码，相邻是常态，一律写分界。
        check_relation(f"同项目题 {name}", entry, True, errors)
    missing_siblings = sorted(set(siblings) - set(sibling_entries))
    if missing_siblings:
        errors.append("同项目题没有分类: " + ", ".join(missing_siblings))

    summary = {
        "corpus_sha256": corpus_sha,
        "issues": sum(1 for item in corpus if not item.get("is_pull_request")),
        "pull_requests": sum(1 for item in corpus if item.get("is_pull_request")),
        "classified": len(entries),
        "term_hits": counts,
        "hit_items": {f"#{n}": t for n, t in sorted(hits.items())},
        "siblings": sorted(siblings),
        "adjacent": [f"#{n}" for n, e in sorted(entries.items()) if e.get("relation") == "adjacent"]
        + [n for n, e in sorted(sibling_entries.items()) if e.get("relation") == "adjacent"],
        "overlap": [f"#{n}" for n, e in sorted(entries.items()) if e.get("relation") == "overlap"]
        + [n for n, e in sorted(sibling_entries.items()) if e.get("relation") == "overlap"],
    }
    return errors, summary


def parse_siblings(items: list[str]) -> dict[str, str]:
    siblings = {}
    for item in items:
        name, sep, path = item.partition("=")
        if not sep or not name.strip():
            raise ValueError(f"--sibling 必须是 名称=题面路径: {item}")
        siblings[name.strip()] = Path(path).read_text(encoding="utf-8")
    return siblings


def self_test() -> int:
    corpus = [
        {"number": 1, "title": "Per-user bandwidth quota", "body": "limit monthly traffic per token", "is_pull_request": False},
        {"number": 2, "title": "Support ARM image", "body": "", "is_pull_request": False},
        {"number": 3, "title": "Add rate limit by IP", "body": "throttle requests", "is_pull_request": True},
    ]
    terms = ["quota", "bandwidth", "traffic", "额度", "流量", "按人"]
    prompt = "团队共用这台服务，想让每个人按月扣同一份流量额度。"
    sibling = "上游不可用时自动切换，恢复后切回。"
    failed = 0
    with tempfile.TemporaryDirectory() as tmp:
        path = Path(tmp) / "corpus.json"
        path.write_text(json.dumps(corpus), encoding="utf-8")
        corpus_sha = sha256(path.read_bytes())
        base = {
            "schema_version": 1, "corpus_sha256": corpus_sha, "prompt_sha256": sha256(prompt.encode()),
            "feature": "每人一个账号，三个下载入口共用月度流量额度", "search_terms": terms,
        }
        adjacent = {"number": 1, "feature": "按 token 限每月流量", "relation": "adjacent",
                    "reason": "Issue 只要单一 token 限流量；本题要求按人登录、命中缓存计量和断点只计已传字节"}
        unrelated = {"number": 2, "relation": "unrelated"}  # feature 来自索引
        sib = {"name": "9-x", "prompt_sha256": sha256(sibling.encode()), "feature": "上游故障切换",
               "relation": "unrelated", "reason": "那道题改上游选择与健康检查，本题改计量与身份，不碰同一状态"}
        features = {2: "提供 ARM 镜像"}
        siblings = {"9-x": sibling}
        good = {**base, "entries": [adjacent, unrelated], "siblings": [sib]}
        cases = (
            ("valid", good, features, siblings, None),
            ("overlap", {**good, "entries": [{**adjacent, "relation": "overlap"}, unrelated]}, features, siblings, "功能重合"),
            ("missing-issue", {**good, "entries": [adjacent]}, features, siblings, "没有逐条分类"),
            ("hit-without-reason", {**good, "entries": [{**adjacent, "relation": "unrelated", "reason": ""}, unrelated]}, features, siblings, "命中检索词"),
            ("stale-corpus", {**good, "corpus_sha256": "0" * 64}, features, siblings, "corpus_sha256"),
            ("stale-prompt", {**good, "prompt_sha256": "0" * 64}, features, siblings, "prompt_sha256"),
            ("few-english-terms", {**good, "search_terms": ["额度", "流量", "按人", "账号", "月度", "quota"]}, features, siblings, "英文至少"),
            ("feature-missing-without-index", good, {}, siblings, "#2 缺少 feature"),
            ("sibling-unclassified", {**good, "siblings": []}, features, siblings, "同项目题没有分类"),
            ("sibling-overlap", {**good, "siblings": [{**sib, "relation": "overlap"}]}, features, siblings, "功能重合"),
            ("sibling-changed", good, features, {"9-x": sibling + "改"}, "prompt_sha256 不一致"),
            ("sibling-needs-reason", {**good, "siblings": [{**sib, "reason": ""}]}, features, siblings, "分界"),
        )
        for name, ledger, index, sibs, expected in cases:
            errors, _ = check(path, ledger, prompt, index, sibs)
            joined = "\n".join(errors)
            ok = not errors if expected is None else expected in joined
            print(f"{name}: {'PASS' if ok else 'FAIL'}")
            if not ok:
                failed += 1
                print(joined or "<no errors>")
        built = skeleton(corpus, corpus_sha, features, terms)
        ok = [e["number"] for e in built["entries"]] == [1, 2] and built["entries"][1]["feature"] == "提供 ARM 镜像"
        print(f"skeleton: {'PASS' if ok else 'FAIL'}")
        failed += 0 if ok else 1
    return 0 if failed == 0 else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--self-test", action="store_true")
    sub = parser.add_subparsers(dest="command")
    fetch_cmd = sub.add_parser("fetch", help="抓取全部 Issue 与 PR 作为语料（每批题重新抓一次）")
    fetch_cmd.add_argument("--repo", required=True)
    fetch_cmd.add_argument("--out", type=Path, required=True)
    index_cmd = sub.add_parser("index", help="检查项目级功能索引是否覆盖语料里每条 Issue")
    index_cmd.add_argument("--corpus", type=Path, required=True)
    index_cmd.add_argument("--features", type=Path, required=True)
    skel_cmd = sub.add_parser("skeleton", help="生成待填写的逐题台账")
    skel_cmd.add_argument("--corpus", type=Path, required=True)
    skel_cmd.add_argument("--features", type=Path)
    skel_cmd.add_argument("--search-term", action="append", default=[], required=True)
    skel_cmd.add_argument("--out", type=Path, required=True)
    check_cmd = sub.add_parser("check", help="核对功能重合台账")
    check_cmd.add_argument("--corpus", type=Path, required=True)
    check_cmd.add_argument("--ledger", type=Path, required=True)
    check_cmd.add_argument("--prompt", type=Path, required=True, help="本题题面")
    check_cmd.add_argument("--features", type=Path, help="项目级功能索引")
    check_cmd.add_argument("--repo", required=True, help="owner/name，用于找同项目的其他题")
    check_cmd.add_argument("--workflow-root", type=Path, default=Path.home() / "repositories/标注任务/swe/workflow")
    check_cmd.add_argument("--sibling", action="append", default=[], metavar="NAME=PATH",
                           help="同批还没进 workflow 的题面草稿，每道都要传")
    args = parser.parse_args()
    if args.self_test:
        return self_test()
    try:
        if args.command == "fetch":
            items = fetch(args.repo, args.out)
            print(json.dumps({
                "out": str(args.out),
                "sha256": sha256(args.out.read_bytes()),
                "issues": sum(1 for i in items if not i["is_pull_request"]),
                "pull_requests": sum(1 for i in items if i["is_pull_request"]),
            }, ensure_ascii=False))
            return 0
        if args.command == "index":
            corpus, _ = load_corpus(args.corpus)
            errors = check_index(corpus, load_features(args.features))
            print(json.dumps({"passed": not errors, "errors": errors}, ensure_ascii=False, indent=2))
            return 0 if not errors else 1
        if args.command == "skeleton":
            if args.out.exists():
                raise ValueError(f"{args.out} 已存在，不覆盖")
            corpus, corpus_sha = load_corpus(args.corpus)
            built = skeleton(corpus, corpus_sha, load_features(args.features), args.search_term)
            args.out.write_text(json.dumps(built, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
            print(json.dumps({"out": str(args.out), "entries": len(built["entries"])}, ensure_ascii=False))
            return 0
        if args.command == "check":
            prompt_text = args.prompt.read_text(encoding="utf-8")
            siblings = discover_siblings(args.workflow_root, args.repo, args.prompt)
            siblings.update(parse_siblings(args.sibling))
            errors, summary = check(
                args.corpus,
                json.loads(args.ledger.read_text(encoding="utf-8")),
                prompt_text,
                load_features(args.features),
                siblings,
            )
            print(json.dumps({"passed": not errors, **summary, "errors": errors}, ensure_ascii=False, indent=2))
            return 0 if not errors else 1
    except (ValueError, OSError, json.JSONDecodeError, subprocess.TimeoutExpired) as exc:
        print(json.dumps({"passed": False, "error": str(exc)}, ensure_ascii=False))
        return 2
    parser.print_help()
    return 2


if __name__ == "__main__":
    sys.exit(main())
