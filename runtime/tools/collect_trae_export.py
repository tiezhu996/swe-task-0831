#!/usr/bin/env python3
"""Collect a Markdown file exported by the Trae CN app; preserve a private original."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import tempfile
from datetime import datetime, timezone


def sha(data):
    return hashlib.sha256(data).hexdigest()


def private_write(path, data):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".export-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as target:
            target.write(data)
            target.flush()
            os.fsync(target.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


MESSAGE_HEADER = re.compile(r"^\*\*(User|Assistant)\*\*\s*$", re.MULTILINE)


def check_prompt_messages(text, prompt):
    headers = list(MESSAGE_HEADER.finditer(text))
    if len(headers) < 2 or headers[0].group(1) != "User" or headers[1].group(1) != "Assistant":
        raise ValueError("无法确认 Trae 导出的首条人工消息")
    user_messages = [header for header in headers if header.group(1) == "User"]
    if len(user_messages) != 1:
        raise ValueError("Trae 导出存在第二条人工消息")
    first_message = text[headers[0].end():headers[1].start()].strip("\r\n")
    if first_message != prompt:
        raise ValueError("首条人工消息与冻结题面不一致")
    if not text[headers[1].end():].strip():
        raise ValueError("Trae 导出缺少模型轨迹")
    return {"user_message_count": 1, "message_count": len(headers), "prompt_exact_match": True}


def collect(source, plan_path, output):
    if source.suffix.lower() != ".md" or not source.is_file():
        raise ValueError("需要 Trae CN 应用导出的 Markdown 文件")
    if source.resolve() in {plan_path.resolve(), output.resolve()}:
        raise ValueError("输入、plan 与脱敏输出不能覆盖彼此")
    plan = json.loads(plan_path.read_text(encoding="utf-8"))
    raw = source.read_bytes()
    text = raw.decode("utf-8")
    prompt = plan["execution_prompt"]
    token = plan["payload"]["trae_session_id"]
    if not token or token in {"未提供", "待填写"} or not prompt:
        raise ValueError("导出文件与实际执行题面不匹配或 Session 未捕获")
    message_audit = check_prompt_messages(text, prompt)
    text = text.replace(token, "[session redacted]")
    text = re.sub(r"[0-9a-f]{16}:[0-9a-f]{32}_[^\s`]+(?: CN[^\n`]+)?", "[session redacted]", text)
    text = re.sub(r"(?:/Users/|/home/)[^\s`\"'<>]+", "[local path redacted]", text)
    text = re.sub(r"[\w.+-]+@[\w.-]+\.[A-Za-z]{2,}", "[email redacted]", text)
    text = re.sub(r"(?im)^.*(?:cookie|authorization|password|密码)\s*[:=].*$", "[credential redacted]", text)
    check_prompt_messages(text, prompt)
    cleaned = text.encode("utf-8")
    receipt = {
        "source": "trae_cn_app_export",
        "collected_at": datetime.now(timezone.utc).isoformat(),
        "raw_sha256": sha(raw),
        "sha256": sha(cleaned),
        "prompt_sha256": sha(prompt.encode()),
        "session_sha256": sha(token.encode()),
        **message_audit,
    }
    private_write(output.with_name("trae-export-original.private.md"), raw)
    private_write(output, cleaned)
    private_write(output.with_suffix(".receipt.json"), (json.dumps(receipt, ensure_ascii=False, indent=2)+"\n").encode())
    return receipt


def verify_existing(source, plan_path, output, receipt_path):
    plan = json.loads(plan_path.read_text(encoding="utf-8"))
    receipt = json.loads(receipt_path.read_text(encoding="utf-8"))
    raw = source.read_bytes()
    cleaned = output.read_bytes()
    prompt = plan["execution_prompt"]
    token = plan["payload"]["trae_session_id"]
    message_audit = check_prompt_messages(raw.decode("utf-8"), prompt)
    check_prompt_messages(cleaned.decode("utf-8"), prompt)
    if (
        receipt.get("source") != "trae_cn_app_export"
        or receipt.get("raw_sha256") != sha(raw)
        or receipt.get("sha256") != sha(cleaned)
        or receipt.get("prompt_sha256") != sha(prompt.encode())
        or receipt.get("session_sha256") != sha(token.encode())
        or not receipt.get("prompt_exact_match")
        or receipt.get("user_message_count") != 1
        or receipt.get("message_count") != message_audit["message_count"]
        or token.encode() in cleaned
    ):
        raise ValueError("轨迹回执与原始导出或冻结题面不一致")
    return {"verified": True, "raw_sha256": receipt["raw_sha256"], "sha256": receipt["sha256"], **message_audit}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--input", type=Path, required=True)
    parser.add_argument("--plan", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--verify-only", action="store_true", help="recheck an existing private original, sanitized copy and receipt without writing")
    parser.add_argument("--receipt", type=Path, help="receipt path for --verify-only")
    args = parser.parse_args()
    try:
        if args.verify_only:
            if args.receipt is None:
                raise ValueError("缺少轨迹回执")
            receipt = verify_existing(args.input, args.plan, args.out, args.receipt)
        else:
            receipt = collect(args.input, args.plan, args.out)
    except (ValueError, KeyError, OSError, UnicodeError):
        parser.exit(1, "trace_collect=BLOCKED 导出内容、题面或本地文件不匹配；未上传\n")
    print(json.dumps(receipt, ensure_ascii=False))


if __name__ == "__main__":
    main()
