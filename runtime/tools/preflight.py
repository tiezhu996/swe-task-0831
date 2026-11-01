#!/usr/bin/env python3
"""Aggregate deterministic SWE task checks into one machine-readable report."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
from datetime import datetime, timezone


def digest(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def check(name: str, passed: bool, detail: str, *, skipped: bool = False) -> dict:
    return {"name": name, "passed": bool(passed), "skipped": skipped, "detail": detail}


def run_external(name: str, command: list[str]) -> dict:
    try:
        result = subprocess.run(command, capture_output=True, text=True, timeout=300)
    except Exception as exc:
        return check(name, False, f"执行失败: {type(exc).__name__}")
    detail = (result.stdout or result.stderr).strip().splitlines()
    return check(name, result.returncode == 0, (detail[-1] if detail else f"exit={result.returncode}"))


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--prompt", type=Path, required=True)
    parser.add_argument("--workspace", type=Path)
    parser.add_argument("--rubric", type=Path)
    parser.add_argument("--issues", type=Path)
    parser.add_argument("--candidate", action="append", default=[], help="field=path; may be repeated")
    parser.add_argument("--issue-check-script", type=Path)
    parser.add_argument("--prose-checker", type=Path)
    parser.add_argument("--profile", default="normal", choices=("normal", "ab-comparison"))
    parser.add_argument("--out", type=Path)
    args = parser.parse_args()

    checks: list[dict] = []
    prompt_bytes = args.prompt.read_bytes() if args.prompt.is_file() else b""
    prompt = prompt_bytes.decode("utf-8", errors="strict") if prompt_bytes else ""
    canonical = prompt.replace("\r\n", "\n").replace("\r", "\n").strip()
    checks.append(check("prompt", bool(canonical) and len(canonical) <= 240 and not re.search(r"```|(?:/Users/|/home/|[A-Za-z]:\\)", canonical), "题面可读，预检使用 canonical 文本" if canonical else "题面为空或不可读"))

    if args.workspace:
        if not args.workspace.is_dir():
            checks.append(check("workspace", False, "源码工作区不存在"))
        else:
            result = subprocess.run(["git", "-C", str(args.workspace), "status", "--porcelain=v1"], capture_output=True, text=True)
            clean = result.returncode == 0 and not result.stdout.strip()
            checks.append(check("workspace_clean", clean, "Git 工作区干净" if clean else "Git 工作区存在改动或不是有效仓库"))
    else:
        checks.append(check("workspace_clean", True, "未提供工作区，跳过", skipped=True))

    if args.rubric:
        text = args.rubric.read_text(encoding="utf-8") if args.rubric.is_file() else ""
        ids = [int(value) for value in re.findall(r"(?m)^\s*-?\s*id:\s*(\d+)\s*$", text)]
        types = re.findall(r"(?m)^\s*type:\s*(f2p|p2p)\s*$", text)
        valid = len(ids) >= 5 and ids == list(range(1, len(ids) + 1)) and "f2p" in types
        checks.append(check("rubric", valid, "Rubric ID 连续且含 f2p" if valid else "Rubric 至少 5 条、ID 必须连续并含 f2p"))
    else:
        checks.append(check("rubric", True, "未提供 Rubric，跳过", skipped=True))

    if args.issues and args.issue_check_script:
        candidates = []
        for value in args.candidate:
            if "=" not in value:
                raise SystemExit("--candidate 必须是 field=path")
            field, path = value.split("=", 1)
            candidates.extend(["--candidate", f"{field}={path}"])
        checks.append(run_external("issue_independence", [sys.executable, str(args.issue_check_script), "--issues", str(args.issues), *candidates]))
    else:
        checks.append(check("issue_independence", True, "未提供 Issue 独立性输入，跳过", skipped=True))

    if args.prose_checker:
        checks.append(run_external("prose", [sys.executable, str(args.prose_checker), str(args.prompt)]))
    else:
        checks.append(check("prose", True, "未提供 prose 检查器，跳过", skipped=True))

    passed = all(item["passed"] for item in checks if not item["skipped"])
    inputs = {"profile": args.profile, "prompt_sha256": digest(canonical.encode()), "workspace": str(args.workspace.resolve()) if args.workspace else "", "rubric": str(args.rubric.resolve()) if args.rubric else ""}
    input_bytes = json.dumps(inputs, ensure_ascii=False, sort_keys=True).encode()
    report = {"schema_version": 1, "profile": args.profile, "passed": passed, "observed_at": datetime.now(timezone.utc).isoformat(), "inputs": inputs, "inputs_sha256": digest(input_bytes), "checks": checks}
    encoded = (json.dumps(report, ensure_ascii=False, indent=2) + "\n").encode()
    if args.out:
        args.out.parent.mkdir(parents=True, exist_ok=True)
        fd = os.open(args.out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "wb") as stream:
            stream.write(encoded)
            stream.flush()
            os.fsync(stream.fileno())
    else:
        sys.stdout.buffer.write(encoded)
    return 0 if passed else 1


if __name__ == "__main__":
    raise SystemExit(main())
