#!/usr/bin/env python3
"""Repair an existing SWE submission in place without retrying ambiguous writes."""

from __future__ import annotations

import argparse
import datetime
import hashlib
import json
import os
import re
import subprocess
import sys
from pathlib import Path
from typing import Any

import requests

BASE_URL = "https://swe.jzxhnh.com/api/v1"
DEFAULT_CONFIG = Path("~/.config/swe-task/config.json").expanduser()
SCRIPT_DIR = Path(__file__).resolve().parent
SCOPE_CHECKER = SCRIPT_DIR / "check_prompt_rubric_scope.py"
RESULT_LINE = re.compile(r"^([1-9][0-9]*) (通过|未通过)(?: (.+))?$")
FORBIDDEN_PAYLOAD_KEYS = {
    "备注",
    "remark",
    "remarks",
    "note",
    "notes",
    "patch_file",
}


def sha256_text(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def write_json(path: Path, value: Any) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    temp = path.with_suffix(path.suffix + ".tmp")
    temp.write_text(
        json.dumps(value, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    os.chmod(temp, 0o600)
    temp.replace(path)


def sanitize(value: Any) -> Any:
    if isinstance(value, list):
        return [sanitize(item) for item in value]
    if isinstance(value, dict):
        result: dict[str, Any] = {}
        for key, item in value.items():
            if key in {"session_id", "trae_session_id", "trae_session_id_2", "token"}:
                result[key] = "<redacted>"
            else:
                result[key] = sanitize(item)
        return result
    return value


def load_config(path: Path) -> dict[str, Any]:
    path = path.expanduser()
    if path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077:
        raise ValueError("private_config_missing_or_permissions_unsafe")
    data = json.loads(path.read_text(encoding="utf-8"))
    if data.get("schema_version") != 1:
        raise ValueError("unsupported_private_config_schema")
    return data


def login_session(config: dict[str, Any]) -> requests.Session:
    values = config.get("swe") or {}
    session = requests.Session()
    session.headers.update({"User-Agent": "swe-task-qc-repair/2"})
    cookie_token = values.get("session")
    csrf_token = values.get("csrf")
    if cookie_token and csrf_token:
        session.cookies.set("swe_qa_session", cookie_token)
        session.cookies.set("swe_qa_csrf", csrf_token)
        return session
    username = values.get("username")
    password = values.get("password")
    if not (username and password):
        raise ValueError("swe_credentials_incomplete")
    response = session.post(
        BASE_URL + "/auth/login",
        json={"username": username, "password": password},
        timeout=30,
    )
    response.raise_for_status()
    envelope = response.json()
    if envelope.get("code") != 0:
        raise RuntimeError("platform_login_failed")
    return session


def get_data(session: requests.Session, path: str) -> dict[str, Any]:
    response = session.get(BASE_URL + path, timeout=30)
    response.raise_for_status()
    envelope = response.json()
    if envelope.get("code") != 0:
        raise RuntimeError(f"platform_get_failed:{path}")
    data = envelope.get("data")
    if not isinstance(data, dict):
        raise TypeError(f"platform_data_invalid:{path}")
    return data


def validate_result(text: str) -> tuple[int, int, int]:
    lines = [line.strip() for line in text.splitlines() if line.strip()]
    if not lines:
        raise ValueError("artifact_result_empty")
    ids: list[int] = []
    passed = 0
    failed = 0
    for line in lines:
        match = RESULT_LINE.fullmatch(line)
        if not match:
            raise ValueError(f"artifact_result_bad_line:{line[:80]}")
        rubric_id = int(match.group(1))
        verdict = match.group(2)
        reason = (match.group(3) or "").strip()
        if verdict == "未通过" and not reason:
            raise ValueError(f"artifact_result_missing_reason:{rubric_id}")
        ids.append(rubric_id)
        if verdict == "通过":
            passed += 1
        else:
            failed += 1
    if ids != list(range(1, len(ids) + 1)):
        raise ValueError("artifact_result_ids_not_contiguous")
    return len(ids), passed, failed


def run_scope_check(
    prompt: Path,
    rubric: Path,
    contract: Path,
    artifact_result: Path,
) -> dict[str, Any]:
    command = [
        sys.executable,
        str(SCOPE_CHECKER),
        "--prompt",
        str(prompt),
        "--rubric",
        str(rubric),
        "--contract",
        str(contract),
        "--artifact-result",
        str(artifact_result),
    ]
    result = subprocess.run(command, check=False, text=True, capture_output=True)
    if result.returncode:
        detail = (result.stdout + "\n" + result.stderr).strip()
        raise ValueError("scope_check_failed:" + " ".join(detail.split())[:500])
    return json.loads(result.stdout)


def task_binding(state_path: Path, detail: dict[str, Any]) -> dict[str, Any]:
    state = json.loads(state_path.read_text(encoding="utf-8"))
    plan_path = Path(state["plan_path"])
    plan = json.loads(plan_path.read_text(encoding="utf-8"))
    values = detail.get("values") or {}
    submission = detail.get("submission") or {}
    expected = {
        "repo_url": state.get("project", {}).get("repo_url"),
        "precheck_item_id": state.get("precheck", {}).get("item_id"),
    }
    for key, value in expected.items():
        if value not in (None, "") and str(values.get(key)) != str(value):
            raise ValueError(f"submission_binding_mismatch:{key}")
    prompt_sha = state.get("precheck", {}).get("prompt_sha256")
    if (
        prompt_sha
        and sha256_text(str(values.get("requirement_prompt", ""))) != prompt_sha
    ):
        raise ValueError("submission_binding_mismatch:requirement_prompt")
    if state.get("submission", {}).get("id") not in (None, 0, submission.get("id")):
        raise ValueError("submission_binding_mismatch:id")
    if plan.get("execution_prompt") != values.get("requirement_prompt"):
        raise ValueError("submission_binding_mismatch:plan_prompt")
    return state


def build_payload(
    schema: dict[str, Any],
    detail: dict[str, Any],
    rubric_text: str,
    artifact_result: str,
) -> dict[str, Any]:
    fields = {field["field_key"]: field for field in schema.get("fields", [])}
    values = detail.get("values") or {}
    locked_fields = set(detail.get("locked_fields") or [])
    payload: dict[str, Any] = {}
    for key, value in values.items():
        field = fields.get(key)
        if field is None or field.get("auto") or field.get("locked_on_fix"):
            continue
        if key in locked_fields or field.get("type") == "file":
            continue
        if value is None or value == "":
            continue
        payload[key] = value
    forbidden = FORBIDDEN_PAYLOAD_KEYS.intersection(payload)
    if forbidden:
        raise ValueError("internal_remark_field_present")
    payload["verify_rubric"] = rubric_text
    payload["artifact_result"] = artifact_result
    return payload


def post_repair(
    session: requests.Session,
    submission_id: int,
    payload_json: str,
    reason: str,
    schema_version: str,
    out_dir: Path,
) -> dict[str, Any]:
    operation = {
        "schema_version": 1,
        "kind": "submission_repair",
        "submission_id": submission_id,
        "method": "PUT",
        "endpoint": f"/submissions/{submission_id}",
        "payload_sha256": sha256_text(payload_json),
        "started_at": datetime.datetime.now(datetime.UTC).isoformat(),
        "outcome": "started",
    }
    write_json(out_dir / "pending-request.json", operation)
    try:
        response = session.put(
            BASE_URL + f"/submissions/{submission_id}",
            headers={"X-CSRF-Token": session.cookies.get("swe_qa_csrf", "")},
            files={
                "payload": (None, payload_json),
                "reason": (None, reason),
                "schema_version": (None, schema_version),
            },
            timeout=60,
        )
    except requests.RequestException as error:
        operation["outcome"] = "uncertain"
        operation["error_type"] = type(error).__name__
        write_json(out_dir / "pending-request.json", operation)
        raise RuntimeError("repair_write_result_uncertain") from error

    operation["http_status"] = response.status_code
    try:
        body = response.json() if response.content else {}
    except ValueError:
        body = {"message": "non_json_response"}
    safe_response = {"status_code": response.status_code, "body": sanitize(body)}
    write_json(out_dir / "response.json", safe_response)
    if response.status_code >= 400 or body.get("code") not in (None, 0):
        operation["outcome"] = "rejected"
        write_json(out_dir / "pending-request.json", operation)
        return safe_response
    operation["outcome"] = "response_success"
    write_json(out_dir / "pending-request.json", operation)
    return safe_response


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--submission-id", type=int, required=True)
    parser.add_argument("--state", type=Path, required=True)
    parser.add_argument("--prompt", type=Path, required=True)
    parser.add_argument("--rubric", type=Path, required=True)
    parser.add_argument("--contract", type=Path, required=True)
    parser.add_argument("--artifact-result", type=Path, required=True)
    parser.add_argument("--repair-note", type=Path, required=True)
    parser.add_argument("--out-dir", type=Path, required=True)
    parser.add_argument("--config", type=Path, default=DEFAULT_CONFIG)
    mode = parser.add_mutually_exclusive_group()
    mode.add_argument("--execute", action="store_true")
    mode.add_argument("--reconcile", action="store_true")
    args = parser.parse_args()

    rubric_text = args.rubric.read_text(encoding="utf-8").strip("\n")
    artifact_result = args.artifact_result.read_text(encoding="utf-8").strip("\n")
    reason = args.repair_note.read_text(encoding="utf-8").strip()
    if not reason:
        raise ValueError("repair_note_empty")
    rubric_count, passed, failed = validate_result(artifact_result)
    scope = run_scope_check(
        args.prompt, args.rubric, args.contract, args.artifact_result
    )
    if int(scope.get("rubrics", -1)) != rubric_count:
        raise ValueError("scope_check_rubric_count_mismatch")

    out_dir = args.out_dir.resolve()
    out_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    session = login_session(load_config(args.config))
    schema = get_data(session, "/submissions/form-schema")
    detail = get_data(session, f"/submissions/{args.submission_id}")
    if args.reconcile:
        request_path = out_dir / "request.json"
        if not request_path.is_file():
            raise ValueError("repair_request_record_missing")
        request_record = json.loads(request_path.read_text(encoding="utf-8"))
        if int(request_record.get("submission_id", 0)) != args.submission_id:
            raise ValueError("repair_request_submission_mismatch")
        current_version = int(
            (detail.get("submission") or {}).get("current_version", 0)
        )
        before = int(request_record.get("current_version_before", 0))
        status = (detail.get("submission") or {}).get("status")
        write_json(out_dir / "reconcile.json", sanitize(detail))
        print(
            json.dumps(
                {
                    "mode": "reconcile",
                    "submission_id": args.submission_id,
                    "current_version_before": before,
                    "current_version": current_version,
                    "status": status,
                    "version_advanced": current_version > before,
                },
                ensure_ascii=False,
            )
        )
        return 0 if current_version > before else 2

    write_json(out_dir / "submission.private.json", detail)
    write_json(
        out_dir / "rejection.json",
        sanitize(
            {
                "schema_version": 1,
                "submission": detail.get("submission"),
                "qc": detail.get("qc"),
                "locked_fields": detail.get("locked_fields"),
                "timeline": detail.get("timeline"),
                "versions": detail.get("versions"),
            }
        ),
    )

    submission = detail.get("submission") or {}
    if int(submission.get("id", 0)) != args.submission_id:
        raise ValueError("submission_identity_mismatch")
    if submission.get("status") != "PENDING_FIX" or not submission.get("editable"):
        raise ValueError("submission_not_editable_pending_fix")
    task_binding(args.state, detail)
    payload = build_payload(schema, detail, rubric_text, artifact_result)
    payload_json = json.dumps(payload, ensure_ascii=False, separators=(",", ":"))
    request_record = {
        "schema_version": 1,
        "submission_id": args.submission_id,
        "method": "PUT",
        "endpoint": f"/submissions/{args.submission_id}",
        "current_version_before": submission.get("current_version"),
        "status_before": submission.get("status"),
        "reason": reason,
        "payload_sha256": sha256_text(payload_json),
        "payload": sanitize(payload),
        "rubric_count": rubric_count,
        "artifact_result": {"passed": passed, "failed": failed},
        "scope_check": scope,
    }
    write_json(out_dir / "request.json", request_record)

    dry_run = {
        "passed": True,
        "execute": bool(args.execute),
        "submission_id": args.submission_id,
        "current_version": submission.get("current_version"),
        "status": submission.get("status"),
        "payload_sha256": request_record["payload_sha256"],
        "rubric_count": rubric_count,
        "artifact_result": {"passed": passed, "failed": failed},
        "locked_fields": detail.get("locked_fields"),
        "scope_check": scope,
    }
    write_json(out_dir / "dry-run.json", dry_run)
    if not args.execute:
        print(json.dumps(dry_run, ensure_ascii=False))
        return 0

    result = post_repair(
        session,
        args.submission_id,
        payload_json,
        reason,
        str(schema.get("schema_version", "")),
        out_dir,
    )
    if result["status_code"] >= 400 or result.get("body", {}).get("code") not in (
        None,
        0,
    ):
        print(json.dumps(result, ensure_ascii=False))
        return 1

    current = get_data(session, f"/submissions/{args.submission_id}")
    write_json(out_dir / "post-repair.json", sanitize(current))
    after = current.get("submission") or {}
    expected_version = int(request_record["current_version_before"]) + 1
    if int(after.get("current_version", 0)) != expected_version:
        raise RuntimeError("repair_version_not_incremented")
    if after.get("status") not in {"PENDING_QC", "QC_RUNNING"}:
        raise RuntimeError("repair_status_not_pending_qc")
    print(
        json.dumps(
            {
                "mode": "execute",
                "submission_id": args.submission_id,
                "status": after.get("status"),
                "current_version": after.get("current_version"),
                "payload_sha256": request_record["payload_sha256"],
                "rubric_count": rubric_count,
                "artifact_result": {"passed": passed, "failed": failed},
            },
            ensure_ascii=False,
        )
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except Exception as error:  # noqa: BLE001 - CLI must return structured diagnostics
        print(json.dumps({"ok": False, "error": str(error)}, ensure_ascii=False))
        raise SystemExit(2)
