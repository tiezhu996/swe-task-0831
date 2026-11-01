#!/usr/bin/env python3
"""One bounded read-only Trae check; return outstanding follow-up, never drive Trae."""
from __future__ import annotations
import argparse
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tempfile
import time

SESSION = re.compile(r'"(?:sessionId|chat_session_id|session_id)"\s*:\s*"([^"]+)"')
STATUS = re.compile(r'"status"\s*:\s*"([^"]+)"')
FOLLOW_UP = {'session_captured': 'verify_session_and_collect', 'session_verified': 'verify_frozen_rubric',
             'artifacts_verified': 'dry_run', 'dry_run_passed': 'submit_and_confirm',
             'submitting': 'reconcile_submission_only'}


def git_signature(workspace):
    def git(*args):
        return subprocess.run(['git', '-C', str(workspace), *args], capture_output=True,
                              check=True, timeout=30).stdout
    digest = hashlib.sha256()
    for output in (git('rev-parse', 'HEAD'), git('status', '--porcelain=v1', '-z'), git('diff', '--binary', 'HEAD')):
        digest.update(output)
    for name in filter(None, git('ls-files', '--others', '--exclude-standard', '-z').split(b'\0')):
        path = workspace / os.fsdecode(name)
        digest.update(name)
        if path.is_symlink():
            digest.update(os.readlink(path).encode())
        elif path.is_file():
            with path.open('rb') as stream:
                for chunk in iter(lambda: stream.read(1024 * 1024), b''):
                    digest.update(chunk)
    return digest.hexdigest()


def read_increment(log, session, previous, limit=4 * 1024 * 1024):
    stat = log.stat()
    identity = [stat.st_dev, stat.st_ino]
    offset = previous.get('offset', 0)
    rotated = bool(previous) and (previous.get('identity') != identity or stat.st_size < offset)
    if rotated:
        offset = 0
    with log.open('rb') as stream:
        stream.seek(offset)
        data = stream.read(limit)
    # Do not advance over a partial JSON line.
    complete = data.rsplit(b'\n', 1)[0] + b'\n' if b'\n' in data else b''
    observed = previous.get('terminal_event', '')
    matched = previous.get('matched', False)
    for line in complete.decode('utf-8', errors='replace').splitlines():
        ids = SESSION.findall(line)
        if not ids or set(ids) != {session}:
            continue
        matched = True
        if '[DoneHandler]' in line:
            status = STATUS.search(line)
            if status:
                observed = status.group(1)
    new_offset = offset + len(complete)
    return {'identity': identity, 'offset': new_offset, 'terminal_event': observed,
            'matched': matched, 'rotated': previous.get('rotated', False) or rotated,
            'backlog': new_offset < stat.st_size, 'bytes_read': len(data)}


def check(state, checkpoint, logs, session, now=None, interval=900):
    now = time.time() if now is None else now
    if state.get('submission', {}).get('id'):
        return {'action': 'already_submitted_no_create', 'submission_id': state['submission']['id'], 'notify': False}, checkpoint
    pending = state.get('pending_operation') or {}
    if pending.get('outcome') in ('started', 'uncertain'):
        return {'action': 'reconcile_pending_write_only', 'notify': True}, checkpoint
    workspace = Path(state['workspace']).resolve()
    binding = {'workspace': str(workspace), 'session': session, 'task_id': state['task_id']}
    if checkpoint and checkpoint.get('binding') != binding:
        raise ValueError('monitor_binding_changed')
    status = state.get('status')
    if status in FOLLOW_UP:
        return {'action': FOLLOW_UP[status], 'notify': checkpoint.get('last_action') != FOLLOW_UP[status]}, {
            **checkpoint, 'binding': binding, 'last_action': FOLLOW_UP[status]}
    # Once completion was observed, quiet/limit must not swallow unfinished work.
    if checkpoint.get('terminal_event') and checkpoint['terminal_event'] != 'completed':
        return {'action': 'preserve_abnormal_stop_no_resume', 'notify': False}, checkpoint
    if status != 'trae_running':
        return {'action': 'inspect_task_state', 'notify': False}, checkpoint
    if now - checkpoint.get('last_check', 0) < interval:
        if checkpoint.get('terminal_event') == 'completed':
            return {'action': 'confirm_completion_then_collect_verify_submit', 'notify': False}, checkpoint
        return {'action': 'rate_limited', 'notify': False}, checkpoint
    observations = {}
    for log in logs:
        observations[str(log.resolve())] = read_increment(log, session, checkpoint.get('logs', {}).get(str(log.resolve()), {}))
    git = git_signature(workspace)
    events = {entry['terminal_event'] for entry in observations.values() if entry['terminal_event']}
    rotated = any(entry['rotated'] for entry in observations.values())
    backlog = any(entry['backlog'] for entry in observations.values())
    matched = any(entry['matched'] for entry in observations.values())
    event = next(iter(events)) if len(events) == 1 else ''
    if rotated or len(events) > 1:
        action = 'audit_log_rotation_or_conflicting_events'
    elif event and event != 'completed':
        action = 'preserve_abnormal_stop_no_resume'
    elif event == 'completed' and not backlog:
        action = 'confirm_completion_then_collect_verify_submit'
    elif backlog:
        action = 'log_backlog_next_scheduled_check'
    elif not matched:
        action = 'need_target_log_or_ui_status'
    else:
        action = 'running_changed' if git != checkpoint.get('git_signature') else 'running_unchanged'
    new = {'binding': binding, 'last_check': now, 'logs': observations, 'git_signature': git,
           'terminal_event': event if not backlog and not rotated else '', 'last_action': action}
    return {'action': action, 'notify': action not in ('running_unchanged', 'log_backlog_next_scheduled_check') and action != checkpoint.get('last_action'),
            'new_log_bytes': sum(entry['bytes_read'] for entry in observations.values())}, new


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--state', type=Path, required=True)
    parser.add_argument('--checkpoint', type=Path, required=True)
    parser.add_argument('--session-id', required=True, help='short log ID; never the full private Session')
    parser.add_argument('--log', action='append', type=Path, required=True, help='only target renderer logs')
    args = parser.parse_args()
    try:
        state = json.loads(args.state.read_text())
        workspace = Path(state['workspace']).resolve()
        cp = args.checkpoint.resolve()
        if cp == args.state.resolve() or cp == workspace or workspace in cp.parents:
            raise ValueError('checkpoint_must_be_outside_model_workspace_and_state')
        cp.parent.mkdir(parents=True, exist_ok=True)
        with cp.with_suffix('.lock').open('a') as lock:
            os.chmod(lock.name, 0o600)
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            previous = json.loads(cp.read_text()) if cp.exists() else {}
            result, updated = check(state, previous, args.log, args.session_id)
            if updated != previous:
                fd, tmp = tempfile.mkstemp(dir=cp.parent, prefix='.monitor-')
                try:
                    with os.fdopen(fd, 'w') as stream:
                        json.dump(updated, stream)
                    os.replace(tmp, cp)
                finally:
                    if os.path.exists(tmp):
                        os.unlink(tmp)
        print(json.dumps(result, ensure_ascii=False))
        return 0
    except Exception:
        print(json.dumps({'action': 'monitor_read_or_binding_failed', 'notify': True}))
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
