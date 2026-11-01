#!/usr/bin/env python3
"""Run a verification script and capture its real, redacted native Terminal view."""

from __future__ import annotations

import argparse
import datetime as dt
import fcntl
import getpass
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import time
import uuid
import unicodedata


class EvidenceError(Exception):
    pass


def default_screenshot_helper() -> Path:
    local = Path(__file__).resolve().parent / "screenshot" / "take_screenshot.py"
    if local.is_file():
        return local
    return Path.home() / ".codex/skills/screenshot/scripts/take_screenshot.py"


def sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def private_write(path: Path, data: bytes) -> None:
    fd, temporary = tempfile.mkstemp(prefix=".evidence-", dir=path.parent)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def redact(text: str, *, identities: tuple[str, ...] | None = None) -> str:
    # Strip terminal controls before displaying untrusted command output.
    text = re.sub(r"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)", "", text)
    text = re.sub(r"\x1b\[[0-?]*[ -/]*[@-~]", "", text)
    text = re.sub(r"[\x00-\x08\x0b-\x1f\x7f]", "", text)
    text = re.sub(r"(?im)^.*(?:authorization:|swe_qa_(?:session|csrf)|(?:password|api[_-]?key|access[_-]?token|secret)\s*[=:]).*$", "[REDACTED CREDENTIAL]", text)
    text = re.sub(r"\b[0-9a-f]{16}:[^\r\n]*?\.T\([^\r\n]*?\)", "[REDACTED TRAE TOKEN]", text, flags=re.I)
    text = re.sub(r"\b[A-Za-z0-9.!#$%&'*+/=?^_`{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}\b", "[REDACTED EMAIL]", text)
    text = re.sub(r"(?<![.A-Za-z0-9])(?:[A-Za-z]:[\\/]|/)[^\s\"'<>|]+", "[REDACTED PATH]", text)
    text = re.sub(r"\b[0-9a-f]{16,}\b|\b[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}\b", "[REDACTED ID]", text, flags=re.I)
    for identity in identities if identities is not None else (getpass.getuser(), platform.node()):
        if identity:
            text = re.sub(r"(?<!\w)" + re.escape(identity) + r"(?!\w)", "[REDACTED USER]", text, flags=re.I)
    return text


def run_verification(script: Path, workspace: Path, raw_path: Path, timeout: float, limit: int) -> tuple[int, str]:
    if not script.is_file() or script.is_symlink() or not workspace.is_dir():
        raise EvidenceError("invalid_script_or_workspace")
    interpreter = {".sh": "/bin/sh", ".bash": "/bin/bash", ".zsh": shutil.which("zsh"), ".py": sys.executable}.get(script.suffix.lower())
    first_line = script.read_bytes().splitlines()[0] if script.stat().st_size else b""
    if first_line.startswith(b"#!"):
        shebang = first_line.decode("utf-8", errors="replace").strip()
        shells = {"#!/bin/sh": "/bin/sh", "#!/bin/bash": "/bin/bash", "#!/bin/zsh": shutil.which("zsh"), "#!/usr/bin/env bash": shutil.which("bash"), "#!/usr/bin/env zsh": shutil.which("zsh"), "#!/usr/bin/env python3": sys.executable}
        if shebang in shells:
            interpreter = shells[shebang]
        elif script.suffix.lower() != ".py":
            raise EvidenceError("unsupported_script_interpreter")
    if not interpreter:
        raise EvidenceError("unsupported_script_type")
    with raw_path.open("xb") as output:
        os.chmod(raw_path, 0o600)
        try:
            process = subprocess.Popen([interpreter, str(script)], cwd=workspace, stdout=output, stderr=subprocess.STDOUT, start_new_session=True)
        except OSError:
            raise EvidenceError("verification_process_could_not_start") from None
        started = time.monotonic()
        failure = ""
        while process.poll() is None:
            if time.monotonic() - started >= timeout:
                failure = "verification_timeout"
            elif raw_path.stat().st_size > limit:
                failure = "verification_output_limit"
            if failure:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait(timeout=2)
                break
            time.sleep(0.05)
        exit_code = process.wait()
    if raw_path.stat().st_size > limit:
        with raw_path.open("r+b") as output:
            output.truncate(limit)
        failure = "verification_output_limit"
    return exit_code, failure


def rubric_section(log: str, expected: str = "") -> list[str]:
    """Extract real stdout rows, never infer success from exit code."""
    lines = log.splitlines()
    starts = [i for i, line in enumerate(lines) if line.strip() in
              {"Frozen rubric results:", "SWE_RUBRIC_RESULTS_BEGIN"}]
    if len(starts) != 1:
        if expected:
            raise EvidenceError("rubric_result_section_missing_or_ambiguous")
        return []
    rows = []
    for line in lines[starts[0] + 1:]:
        if line.strip() == "SWE_RUBRIC_RESULTS_END":
            break
        if not line.strip():
            continue
        if not re.fullmatch(r"\s*\d+\s+(?:PASS|FAIL|通过|未通过)(?:\s+.*)?", line):
            raise EvidenceError("unexpected_text_in_rubric_result_section")
        rows.append(line.strip())
    ids = [row.split()[0] for row in rows]
    if not rows or len(set(ids)) != len(ids) or (expected and ids != expected.split(",")):
        raise EvidenceError("rubric_result_ids_incomplete_or_duplicate")
    return rows


def display_text(log: str, manifest: dict, marker: str, expected: str = "") -> str:
    rows = rubric_section(log, expected)
    manifest["display_mode"] = "rubric_results" if rows else "log_tail"
    content = "\n".join(rows) if rows else log
    wrapped = []
    for original in content.splitlines():
        line, width = "", 0
        for character in original.expandtabs(4):
            size = 0 if unicodedata.combining(character) else 2 if unicodedata.east_asian_width(character) in {"F", "W"} else 1
            if width + size > 108:
                wrapped.append(line)
                line, width = "", 0
            line += character
            width += size
        wrapped.append(line)
    if rows and len(wrapped) > 38:
        raise EvidenceError("rubric_results_exceed_single_screen_use_shorter_reasons")
    tail = wrapped if rows else wrapped[-36:]
    lines = ["Rubric result" if rows else "Verify Rubric: actual verification output", "", *tail, "", f"exit_code={manifest['exit_code']}  execution_completed={str(manifest['execution_completed']).lower()}"]
    if rows:
        passed = sum(row.split()[1] in {"PASS", "通过"} for row in rows)
        lines.append(f"PASS {passed}/{len(rows)}   FAIL {len(rows)-passed}/{len(rows)}")
        lines.append("Full verification log archived; rows above are actual stdout.")
    elif len(wrapped) > len(tail):
        lines.append(f"Showing the final {len(tail)} lines; complete redacted output is archived.")
    if manifest.get("error_code"):
        lines.append(f"capture_error={manifest['error_code']}")
    lines.append(marker)
    return "\n".join(lines) + "\n"


def apple_script(source: str, *args: str, timeout: int = 15) -> str:
    try:
        result = subprocess.run(["osascript", "-", *args], input=source, text=True, capture_output=True, timeout=timeout, check=True)
    except (OSError, subprocess.SubprocessError):
        raise EvidenceError("terminal_automation_permission_or_command_failed") from None
    return result.stdout.strip()


TERMINAL_TITLE = "SWE Rubric Evidence"


def screenshot_native(display: Path, destination: Path, helper_path: Path, marker: str, lock_wait: float = 0.0) -> dict:
    private = Path.home() / ".config/swe-task"
    private.mkdir(parents=True, exist_ok=True, mode=0o700)
    lock_path = private / "terminal-evidence.lock"
    if lock_path.is_symlink():
        raise EvidenceError("terminal_lock_is_symlink")
    with lock_path.open("a") as lock:
        os.chmod(lock_path, 0o600)
        # Parallel tasks queue for the one dedicated terminal instead of
        # failing; only a capture still held after lock_wait is an error.
        deadline = time.monotonic() + max(lock_wait, 0.0)
        while True:
            try:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                break
            except BlockingIOError:
                if time.monotonic() >= deadline:
                    raise EvidenceError("dedicated_terminal_capture_busy") from None
                time.sleep(2)
        return _screenshot_native(display, destination, helper_path, marker)


def _screenshot_native(display: Path, destination: Path, helper_path: Path, marker: str) -> dict:
    if platform.system() != "Darwin":
        raise EvidenceError("native_capture_requires_macos")
    if any(key.startswith("CODEX_SCREENSHOT_TEST_") for key in os.environ):
        raise EvidenceError("synthetic_screenshot_mode_forbidden")
    if not helper_path.is_file():
        raise EvidenceError("screenshot_helper_missing")
    spec = importlib.util.spec_from_file_location("swe_screenshot_helper", helper_path)
    helper = importlib.util.module_from_spec(spec)
    try:
        spec.loader.exec_module(helper)
        if not helper.macos_screen_capture_granted():
            raise EvidenceError("screen_recording_permission_required")
    except EvidenceError:
        raise
    except (Exception, SystemExit):
        raise EvidenceError("screen_recording_permission_check_failed") from None

    ready = display.with_suffix(".ready")
    runner = display.with_suffix(".command")
    ready.unlink(missing_ok=True)
    command = "printf '\\033[2J\\033[H'; /bin/cat " + shlex.quote(str(display)) + "; /usr/bin/touch " + shlex.quote(str(ready)) + "\n"
    private_write(runner, command.encode())
    registry = Path.home() / ".config/swe-task/terminal-window.json"
    if registry.is_symlink():
        raise EvidenceError("terminal_registry_is_symlink")
    saved_id = 0
    if registry.exists():
        registered = json.loads(registry.read_text())
        if registered.get("owner") != TERMINAL_TITLE or type(registered.get("window_id")) is not int:
            raise EvidenceError("terminal_registry_invalid")
        saved_id = registered["window_id"]
    window = apple_script('''on run argv
tell application "Terminal"
    set ownedTabs to {}
    set ownedWindows to {}
    repeat with w in windows
        if id of w is (item 4 of argv as integer) then
            if (count of tabs of w) is not 1 then error "dedicated window has extra tabs"
            set t to selected tab of w
            if custom title of t is not item 2 of argv then error "registered window identity changed"
            set end of ownedTabs to t
            set end of ownedWindows to w
        end if
    end repeat
    if (count of ownedTabs) > 1 then error "multiple dedicated evidence terminals"
    set displayCommand to "precmd_functions=(); preexec_functions=(); function precmd() { :; }; PROMPT=''; RPROMPT=''; PS1=''; /bin/sh " & quoted form of item 1 of argv
    if (count of ownedTabs) is 1 then
        set evidenceTab to item 1 of ownedTabs
        set evidenceWindow to item 1 of ownedWindows
        if busy of evidenceTab then error "dedicated evidence terminal busy"
        do script displayCommand in evidenceTab
    else
        set evidenceTab to do script displayCommand
        set evidenceWindow to front window
    end if
    set custom title of evidenceTab to item 2 of argv
    set title displays custom title of evidenceTab to true
    set title displays device name of evidenceTab to false
    set title displays shell path of evidenceTab to false
    set title displays window size of evidenceTab to false
    set title displays file name of evidenceTab to false
    set number of columns of evidenceTab to 116
    set number of rows of evidenceTab to (item 3 of argv as integer)
    set selected tab of evidenceWindow to evidenceTab
    set index of evidenceWindow to 1
    activate
    return id of evidenceWindow
end tell
end run''', str(runner), TERMINAL_TITLE, str(min(50, max(20, len(display.read_text().splitlines()) + 3))), str(saved_id))
    try:
        window_id = int(window)
    except ValueError:
        raise EvidenceError("terminal_window_id_unavailable") from None
    private_write(registry, json.dumps({"owner": TERMINAL_TITLE, "window_id": window_id}).encode())
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        contents = apple_script('''on run argv
tell application "Terminal" to return contents of selected tab of window id (item 1 of argv as integer)
end run''', str(window_id))
        if ready.exists() and marker in contents:
            break
        time.sleep(0.1)
    else:
        raise EvidenceError("terminal_output_not_ready")
    # Shell integrations can write an OSC title after do-script. Set and bind
    # the title again once the displayed command has completed.
    apple_script('''on run argv
tell application "Terminal"
    set custom title of selected tab of window id (item 1 of argv as integer) to item 2 of argv
end tell
end run''', str(window_id), TERMINAL_TITLE)
    try:
        payload = helper.swift_json(helper.MAC_WINDOW_SCRIPT, ["--list"])
        windows = payload.get("windows") or []
        # Terminal localizes its owner/title (for example, "终端" and may omit
        # the tab custom title), so the actual window id is the stable identity.
        matches = [entry for entry in windows if int(entry.get("id", 0)) == window_id and entry.get("owner", "").lower() in {"terminal", "终端"}]
        if len(matches) != 1:
            raise EvidenceError("terminal_window_identity_mismatch")
        temporary_dir = tempfile.TemporaryDirectory(prefix="native-window-", dir=destination.parent)
        raw_shot = Path(temporary_dir.name) / "window.png"
        try:
            subprocess.run([sys.executable, str(helper_path), "--window-id", str(window_id), "--path", str(raw_shot)], capture_output=True, timeout=30, check=True)
            from PIL import Image

            with Image.open(raw_shot) as picture:
                bounds = matches[0]["bounds"]
                scale = round(picture.width / bounds["width"])
                if scale not in {1, 2}:
                    raise EvidenceError("native_screenshot_geometry_unknown")
                if "A" not in picture.getbands():
                    raise EvidenceError("native_screenshot_alpha_bounds_missing")
                window_box = picture.getchannel("A").point(lambda alpha: 255 if alpha >= 254 else 0).getbbox()
                if not window_box:
                    raise EvidenceError("native_screenshot_window_bounds_missing")
                left, top, right, bottom = window_box
                if abs(right - left - bounds["width"] * scale) > 4 * scale or abs(bottom - top - bounds["height"] * scale) > 4 * scale:
                    raise EvidenceError("native_screenshot_geometry_unknown")
                box = (left, top + 34 * scale, right, bottom)
                picture.crop(box).save(destination)
        finally:
            raw_shot.unlink(missing_ok=True)
            temporary_dir.cleanup()
    except EvidenceError:
        raise
    except (Exception, SystemExit):
        raise EvidenceError("native_window_screenshot_failed") from None
    data = destination.read_bytes()
    if len(data) < 4096 or data[:8] != b"\x89PNG\r\n\x1a\n" or min(struct.unpack(">II", data[16:24])) < 200:
        raise EvidenceError("native_screenshot_invalid_or_blank")
    os.chmod(destination, 0o600)
    return {"window_id": window_id, "privacy_crop": {"reason": "remove native window title and shadow", "pixel_box": list(box), "content_pixels_modified": False}}


def capture(args: argparse.Namespace) -> dict:
    script, workspace, out = args.command_script.resolve(), args.workspace.resolve(), args.out_dir.resolve()
    if args.timeout <= 0 or args.max_log_bytes <= 0:
        raise EvidenceError("invalid_execution_limits")
    out.mkdir(parents=True, exist_ok=True)
    os.chmod(out, 0o700)
    if (out / "verification-manifest.json").exists():
        raise EvidenceError("evidence_already_exists_use_new_output_directory")
    lock = out / ".native-evidence.lock"
    try:
        lock_fd = os.open(lock, os.O_CREAT | os.O_EXCL | os.O_WRONLY, 0o600)
    except FileExistsError:
        raise EvidenceError("evidence_capture_already_running") from None
    os.close(lock_fd)
    manifest = {"schema_version": 1, "source": "subprocess_only" if args.run_only else "native_terminal", "completed": False, "execution_completed": False, "exit_code": -1, "command_path": "verification-command" + script.suffix.lower(), "command_sha256": "", "log_sha256": "", "screenshot_sha256": "", "log_path": "verification.log", "screenshot_path": "", "captured_at": dt.datetime.now(dt.timezone.utc).isoformat()}
    raw, log, screenshot = out / "verification.raw.log", out / "verification.log", out / "verification-terminal.png"
    try:
        command_data = script.read_bytes()
        manifest["command_sha256"] = sha256(command_data)
        private_write(out / manifest["command_path"], command_data)
        manifest["exit_code"], error = run_verification(script, workspace, raw, args.timeout, args.max_log_bytes)
        if sha256(script.read_bytes()) != manifest["command_sha256"]:
            error = "verification_command_changed_during_execution"
        manifest["execution_completed"] = not error
        if error:
            manifest["error_code"] = error
        raw_data = raw.read_bytes()
        manifest["raw_log_sha256"] = sha256(raw_data)
        private_write(log, redact(raw_data.decode("utf-8", errors="replace")).encode())
        manifest["log_sha256"] = sha256(log.read_bytes())
        marker = "Verify Rubric " + uuid.uuid4().hex[:10]
        display = out / "terminal-display.log"
        private_write(display, display_text(log.read_text(), manifest, marker, getattr(args, "rubric_ids", "")).encode())
        manifest["display_sha256"] = sha256(display.read_bytes())
        manifest["display_path"] = display.name
        manifest["schema_version"] = 2
        if not args.run_only:
            manifest.update(screenshot_native(display, screenshot, args.screenshot_helper, marker, getattr(args, "lock_wait", 0.0)))
            manifest["screenshot_path"] = screenshot.name
            manifest["screenshot_sha256"] = sha256(screenshot.read_bytes())
            manifest["completed"] = True
    except (OSError, EvidenceError) as error:
        manifest["error_code"] = str(error) if isinstance(error, EvidenceError) else "local_evidence_io_failed"
    finally:
        manifest["captured_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
        private_write(out / "verification-manifest.json", (json.dumps(manifest, ensure_ascii=False, indent=2) + "\n").encode())
        lock.unlink(missing_ok=True)
    return manifest


def recapture(args: argparse.Namespace) -> dict:
    """Display an immutable previous execution without running its command."""
    source, out = args.from_evidence.resolve(), args.out_dir.resolve()
    previous_bytes = (source / "verification-manifest.json").read_bytes()
    previous = json.loads(previous_bytes)
    if previous.get("execution_completed") is not True or type(previous.get("exit_code")) is not int:
        raise EvidenceError("previous_execution_incomplete")
    copies = {}
    for path_key, hash_key in (("command_path", "command_sha256"), ("log_path", "log_sha256")):
        name = previous.get(path_key, "")
        if not name or Path(name).name != name or (source / name).is_symlink():
            raise EvidenceError("previous_evidence_path_unsafe")
        data = (source / name).read_bytes()
        if sha256(data) != previous.get(hash_key):
            raise EvidenceError("previous_evidence_hash_mismatch")
        copies[name] = data
    raw = (source / "verification.raw.log").read_bytes()
    if sha256(raw) != previous.get("raw_log_sha256") or redact(raw.decode("utf-8", errors="replace")).encode() != copies[previous["log_path"]]:
        raise EvidenceError("previous_raw_log_mismatch")
    copies["verification.raw.log"] = raw
    # New exclusive directory, original execution/screenshot remains untouched.
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    manifest = {key: previous[key] for key in ("exit_code", "execution_completed", "command_path", "command_sha256", "log_path", "log_sha256", "raw_log_sha256")}
    manifest.update(schema_version=2, source="subprocess_only" if args.run_only else "native_terminal", completed=False,
                    screenshot_path="", screenshot_sha256="", previous_manifest_sha256=sha256(previous_bytes),
                    execution_captured_at=previous.get("captured_at"))
    try:
        for name, data in copies.items():
            private_write(out / name, data)
        marker = "Verify Rubric " + uuid.uuid4().hex[:10]
        display = out / "terminal-display.log"
        private_write(display, display_text(copies[previous["log_path"]].decode(), manifest, marker, args.rubric_ids).encode())
        manifest.update(display_path=display.name, display_sha256=sha256(display.read_bytes()))
        if not args.run_only:
            screenshot = out / "verification-terminal.png"
            manifest.update(screenshot_native(display, screenshot, args.screenshot_helper, marker, getattr(args, "lock_wait", 0.0)))
            manifest.update(screenshot_path=screenshot.name, screenshot_sha256=sha256(screenshot.read_bytes()), completed=True)
    except (OSError, EvidenceError) as error:
        manifest["error_code"] = str(error) if isinstance(error, EvidenceError) else "local_evidence_io_failed"
    finally:
        manifest["captured_at"] = dt.datetime.now(dt.timezone.utc).isoformat()
        private_write(out / "verification-manifest.json", (json.dumps(manifest, ensure_ascii=False, indent=2) + "\n").encode())
    return manifest


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--command-script", type=Path)
    group.add_argument("--from-evidence", type=Path, help="recapture immutable existing logs without rerunning verification")
    parser.add_argument("--workspace", type=Path)
    parser.add_argument("--out-dir", required=True, type=Path)
    parser.add_argument("--timeout", type=float, default=900)
    parser.add_argument("--lock-wait", type=float, default=900, help="seconds to queue for the dedicated terminal while another task captures; 0 fails at once")
    parser.add_argument("--max-log-bytes", type=int, default=20 * 1024 * 1024)
    parser.add_argument("--rubric-ids", default="", help="frozen rubric IDs in order, e.g. 1,2,3; require complete actual stdout summary")
    parser.add_argument("--run-only", action="store_true", help="capture logs only; never claims native screenshot completion")
    parser.add_argument("--screenshot-helper", type=Path, default=default_screenshot_helper())
    args = parser.parse_args()
    try:
        if not args.from_evidence and not args.workspace:
            raise EvidenceError("workspace_required_for_execution")
        manifest = recapture(args) if args.from_evidence else capture(args)
    except (EvidenceError, OSError) as error:
        print(json.dumps({"completed": False, "error_code": str(error) if isinstance(error, EvidenceError) else "local_evidence_io_failed"}))
        return 2
    print(json.dumps(manifest, ensure_ascii=False))
    if manifest.get("error_code"):
        return 2
    return 0 if manifest["exit_code"] == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
