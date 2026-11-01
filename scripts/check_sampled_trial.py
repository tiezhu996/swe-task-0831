#!/usr/bin/env python3
"""Check a user-requested sampled trial set.

The normal precheck gate runs a cheap-model trial for every task. Some batches
explicitly opt into a sampled workload instead: two representative tasks must
have completed confirmation and trial runs in the target window, while the
remaining tasks still pass the four deterministic gates.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path


def digest(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--sample-dir", type=Path, action="append", required=True)
    parser.add_argument("--minimum", type=int, default=2)
    args = parser.parse_args()

    errors: list[str] = []
    samples: list[dict[str, object]] = []
    for sample_dir in args.sample_dir:
        trial_path = sample_dir / "trial-run.json"
        confirm_path = sample_dir / "trial-confirm.json"
        prompt_path = sample_dir.parent.parent / "workflow" / sample_dir.name / "PROMPT.md"
        if not trial_path.is_file() or not confirm_path.is_file():
            errors.append(f"{sample_dir.name}: 缺完整试跑或二次确认")
            continue
        trial = json.loads(trial_path.read_text(encoding="utf-8"))
        confirm = json.loads(confirm_path.read_text(encoding="utf-8"))
        if prompt_path.is_file():
            expected = digest(prompt_path)
            if trial.get("prompt_sha256") != expected or confirm.get("prompt_sha256") != expected:
                errors.append(f"{sample_dir.name}: 试跑没有绑定当前冻结题面")
        if trial.get("verdict") != "in_window":
            errors.append(f"{sample_dir.name}: 抽样试跑判定为 {trial.get('verdict')}")
        if confirm.get("errors"):
            errors.append(f"{sample_dir.name}: 二次确认仍有错误")
        samples.append(
            {
                "name": sample_dir.name,
                "trial_sha256": digest(trial_path),
                "confirm_sha256": digest(confirm_path),
                "verdict": trial.get("verdict"),
                "prediction": trial.get("prediction"),
            }
        )
    if len(samples) < args.minimum:
        errors.append(f"抽样试跑至少需要 {args.minimum} 道完整结果，当前 {len(samples)} 道")
    print(
        json.dumps(
            {
                "passed": not errors,
                "mode": "sampled",
                "samples": samples,
                "errors": errors,
            },
            ensure_ascii=False,
            indent=2,
        )
    )
    return 0 if not errors else 1


if __name__ == "__main__":
    raise SystemExit(main())
