#!/usr/bin/env python3
"""Write a small provenance manifest for an installed swe-task runtime."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import sys
from datetime import datetime, timezone


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def tree_hash(root: Path) -> str:
    digest = hashlib.sha256()
    for path in sorted(p for p in root.rglob("*") if p.is_file() and not p.name.endswith(".pyc")):
        # The manifest is written after the runtime tree is staged. Excluding
        # it keeps the recorded runtime hash stable and self-consistent.
        if path == root / "runtime-manifest.json":
            continue
        rel = path.relative_to(root).as_posix().encode()
        digest.update(rel + b"\0")
        digest.update(bytes.fromhex(sha256_file(path)))
    return digest.hexdigest()


def git_commit(root: Path) -> str:
    try:
        return subprocess.run(["git", "-C", str(root), "rev-parse", "HEAD"], capture_output=True, text=True, check=True, timeout=5).stdout.strip()
    except Exception:
        return ""


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--runtime", type=Path, required=True)
    parser.add_argument("--source", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    args = parser.parse_args()
    runtime = args.runtime.resolve()
    manifest = {
        "schema_version": 1,
        "created_at": datetime.now(timezone.utc).isoformat(),
        "source_root": str(args.source.resolve()),
        "source_git_commit": git_commit(args.source),
        "source_tree_sha256": tree_hash(args.source) if args.source.is_dir() else "",
        "runtime_tree_sha256": tree_hash(runtime),
        "swe_flow_sha256": sha256_file(runtime / "bin" / "swe-flow") if (runtime / "bin" / "swe-flow").is_file() else "",
        "python": sys.version,
        "platform": platform.platform(),
    }
    args.out.parent.mkdir(parents=True, exist_ok=True)
    fd = os.open(args.out, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w", encoding="utf-8") as stream:
        json.dump(manifest, stream, ensure_ascii=False, indent=2)
        stream.write("\n")
        stream.flush()
        os.fsync(stream.fileno())
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
