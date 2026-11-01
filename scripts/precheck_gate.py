#!/usr/bin/env python3
"""Run the local gates for one task and write the receipt swe-flow requires.

``swe-flow precheck-submit`` and ``precheck-repair`` refuse to send a prompt unless the
task root holds a ``precheck-gate.json`` that:

- binds the exact prompt being sent (SHA-256 of the canonical text);
- reports independence, feature_overlap, style, scope_depth and trial with exit code 0
  (trial only reads the cheap-model rehearsal results in trials/<task>/, it never runs a model);
- lists every file the gates read with its SHA-256, so editing the rubric, contract,
  ledger, corpus, index or a sibling prompt after the run invalidates the receipt.

The receipt is rewritten on every run; gate output goes to ``precheck-gate-logs/``.
Exit code 0 only when all gates pass.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parent
sys.path.insert(0, str(SCRIPTS))
from check_issue_feature_overlap import discover_siblings  # noqa: E402

RECEIPT = "precheck-gate.json"
LOG_DIR = "precheck-gate-logs"
DEFAULT_WORKFLOW_ROOT = Path.home() / "repositories/标注任务/swe/workflow"


def canonical_prompt(text: str) -> str:
    # 与 runtime precheck-client 的 CanonicalPrompt 一致。
    return text.replace("\r\n", "\n").replace("\r", "\n").strip()


def file_sha256(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def write_private(path: Path, data: str) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_name(path.name + ".tmp")
    fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as handle:
        handle.write(data)
    os.replace(tmp, path)


def gate_commands(args, task: Path) -> list[tuple[str, list[str]]]:
    prompt = task / "PROMPT.md"
    candidates = [f"需求 Prompt（原文）={prompt}", f"Verify Rubric={args.rubric}"]
    if args.authenticity:
        candidates.append(f"真实性与难度说明={args.authenticity}")
    independence = [sys.executable, str(SCRIPTS / "check_prompt_independence.py"), "--issues", str(args.corpus)]
    for candidate in candidates:
        independence += ["--candidate", candidate]
    overlap = [
        sys.executable, str(SCRIPTS / "check_issue_feature_overlap.py"), "check",
        "--repo", args.repo, "--corpus", str(args.corpus), "--features", str(args.features),
        "--prompt", str(prompt), "--ledger", str(args.ledger), "--workflow-root", str(args.workflow_root),
    ]
    for sibling in args.sibling:
        overlap += ["--sibling", sibling]
    gates = [
        ("independence", independence),
        ("feature_overlap", overlap),
        ("style", [sys.executable, str(SCRIPTS / "check_prompt_style.py"), "--prompt", str(prompt)]),
        ("scope_depth", [
            sys.executable, str(SCRIPTS / "check_prompt_rubric_scope.py"), "--prompt", str(prompt),
            "--rubric", str(args.rubric), "--contract", str(args.contract), "--source", str(args.source),
        ]),
    ]
    if args.trial_sample:
        sample_cmd = [sys.executable, str(SCRIPTS / "check_sampled_trial.py")]
        for path in args.trial_sample:
            sample_cmd += ["--sample-dir", str(path)]
        gates.append(("trial_sampling", sample_cmd))
    else:
        gates.append(("trial", [
            sys.executable, str(SCRIPTS / "trial_run.py"), "check",
            "--trial-dir", str(args.trial_dir), "--prompt", str(prompt),
        ]))
    return gates


def input_files(args, task: Path) -> list[Path]:
    files = [task / "PROMPT.md", args.rubric, args.contract, args.ledger, args.corpus, args.features]
    if args.authenticity:
        files.append(args.authenticity)
    files += [Path(item.partition("=")[2]) for item in args.sibling]
    files += [path for path in (args.trial_dir / "trial-run.json", args.trial_dir / "trial-confirm.json")
              if path.is_file()]
    for sample in args.trial_sample:
        files += [path for path in (sample / "trial-run.json", sample / "trial-confirm.json") if path.is_file()]
    # 自动找到的同项目题面也是 feature_overlap 的输入。
    for name in discover_siblings(args.workflow_root, args.repo, task / "PROMPT.md"):
        files.append(args.workflow_root / name / "PROMPT.md")
    return [path.resolve() for path in files]


def run(args, runner=subprocess.run) -> dict:
    task = args.task_dir.resolve()
    prompt_path = task / "PROMPT.md"
    if not prompt_path.is_file():
        raise ValueError(f"{prompt_path} 不存在")
    for name in ("rubric", "contract", "ledger", "corpus", "features", "source"):
        value = getattr(args, name)
        if not value.exists():
            raise ValueError(f"--{name} 指向的 {value} 不存在")
    prompt = canonical_prompt(prompt_path.read_text(encoding="utf-8"))
    inputs = {str(path): file_sha256(path) for path in input_files(args, task)}

    gates = []
    for name, command in gate_commands(args, task):
        result = runner(command, capture_output=True, text=True, timeout=600, check=False)
        log = task / LOG_DIR / f"{name}.log"
        output = (result.stdout or "") + (result.stderr or "")
        write_private(log, output)
        gates.append({
            "name": name,
            "exit_code": result.returncode,
            "log": str(log),
            "log_sha256": hashlib.sha256(output.encode("utf-8")).hexdigest(),
        })
    # 门禁跑完后输入又被改动，回执作废。
    if {str(path): file_sha256(path) for path in input_files(args, task)} != inputs:
        raise ValueError("门禁运行期间输入文件被改动，重新运行")
    receipt = {
        "schema_version": 1,
        "checked_at": datetime.now(timezone.utc).isoformat(),
        "passed": all(gate["exit_code"] == 0 for gate in gates),
        "prompt_sha256": hashlib.sha256(prompt.encode("utf-8")).hexdigest(),
        "prompt_chars": len(prompt),
        "gates": gates,
        "inputs": inputs,
    }
    write_private(task / RECEIPT, json.dumps(receipt, ensure_ascii=False, indent=2) + "\n")
    return receipt


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--task-dir", type=Path, required=True, help="workflow/<编号>-<项目> 任务目录，内含 PROMPT.md")
    parser.add_argument("--repo", required=True, help="owner/name")
    parser.add_argument("--corpus", type=Path, required=True, help="本批 Issue/PR 语料")
    parser.add_argument("--features", type=Path, required=True, help="项目级 issue-features.json")
    parser.add_argument("--source", type=Path, required=True, help="固定基线源码树")
    parser.add_argument("--rubric", type=Path, help="默认 <任务目录>/rubric.yaml")
    parser.add_argument("--contract", type=Path, help="默认 <任务目录>/design-contract.json")
    parser.add_argument("--ledger", type=Path, help="默认 <任务目录>/overlap-ledger.json")
    parser.add_argument("--authenticity", type=Path, help="真实性与难度说明；默认 <任务目录>/authenticity.txt（存在时）")
    parser.add_argument("--sibling", action="append", default=[], metavar="NAME=PATH", help="同批还没进 workflow 的题面草稿")
    parser.add_argument("--workflow-root", type=Path, default=DEFAULT_WORKFLOW_ROOT)
    parser.add_argument("--trial-dir", type=Path, help="便宜模型试跑目录；默认 <workflow 根的上级>/trials/<任务目录名>")
    parser.add_argument(
        "--trial-sample",
        type=Path,
        action="append",
        default=[],
        help="用户明确要求抽样试跑时，传入 1 道以上完整 in-window 试跑目录；替代逐题 trial 门禁",
    )
    return parser


def resolve_defaults(args) -> None:
    task = args.task_dir
    args.rubric = args.rubric or task / "rubric.yaml"
    args.contract = args.contract or task / "design-contract.json"
    args.ledger = args.ledger or task / "overlap-ledger.json"
    args.trial_dir = args.trial_dir or args.workflow_root.parent / "trials" / task.name
    if args.authenticity is None and (task / "authenticity.txt").is_file():
        args.authenticity = task / "authenticity.txt"


def main() -> int:
    args = build_parser().parse_args()
    resolve_defaults(args)
    try:
        receipt = run(args)
    except (ValueError, OSError, subprocess.TimeoutExpired) as exc:
        print(json.dumps({"passed": False, "error": str(exc)}, ensure_ascii=False))
        return 2
    print(json.dumps({
        "passed": receipt["passed"],
        "prompt_sha256": receipt["prompt_sha256"],
        "gates": {gate["name"]: gate["exit_code"] for gate in receipt["gates"]},
        "receipt": str(args.task_dir / RECEIPT),
    }, ensure_ascii=False))
    return 0 if receipt["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
