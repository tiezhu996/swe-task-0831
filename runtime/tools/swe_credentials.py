#!/usr/bin/env python3
"""Private SWE/GitHub configuration. Never prints credential values."""
from __future__ import annotations

import argparse
import http.cookiejar
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import urllib.request

DEFAULT_CONFIG = Path.home() / '.config/swe-task/config.json'


def load_config(path=DEFAULT_CONFIG):
    path = Path(path).expanduser()
    if path.is_symlink() or not path.is_file() or path.stat().st_mode & 0o077:
        raise ValueError('private_config_missing_or_permissions_unsafe')
    if path.parent.is_symlink() or path.parent.stat().st_mode & 0o077:
        raise ValueError('private_config_directory_permissions_unsafe')
    data = json.loads(path.read_text())
    if data.get('schema_version') != 1:
        raise ValueError('unsupported_private_config_schema')
    gh = data.get('github', {})
    for key in ('login', 'ssh_host'):
        if gh.get(key) and not re.fullmatch(r'[A-Za-z0-9_.-]+', gh[key]):
            raise ValueError('invalid_github_identity')
    return data


def command_env(config, service):
    env = os.environ.copy()
    for key in ('SWE_USERNAME', 'SWE_PASSWORD', 'SWE_SESSION', 'SWE_CSRF', 'GH_TOKEN', 'GITHUB_TOKEN'):
        env.pop(key, None)
    if service == 'swe':
        values = config['swe']
        if values.get('username') and values.get('password'):
            env.update(SWE_USERNAME=values['username'], SWE_PASSWORD=values['password'])
        elif values.get('session') and values.get('csrf'):
            env.update(SWE_SESSION=values['session'], SWE_CSRF=values['csrf'])
        else:
            raise ValueError('swe_credentials_incomplete')
    elif service == 'github':
        values = config['github']
        if not values.get('token') or not values.get('login'):
            raise ValueError('github_credentials_incomplete')
        env.pop('GITHUB_TOKEN', None)
        env['GH_TOKEN'] = values['token']
        env['GH_PROMPT_DISABLED'] = '1'
        if values.get('ssh_command'):
            env['GIT_SSH_COMMAND'] = values['ssh_command']
    else:
        raise ValueError('unknown_credential_service')
    return env


def validate_run_command(cmd, service):
    """Keep the credential wrapper away from shells and token-dump commands."""
    if service == 'swe':
        installed = Path.home() / '.codex/skills/swe-task/runtime/bin/swe-flow'
        if not cmd or Path(cmd[0]).resolve() != installed.resolve():
            raise ValueError('credential_command_not_allowed')
        return
    if len(cmd) < 3 or cmd[:2] != ['gh', 'api']:
        raise ValueError('credential_command_not_allowed')
    endpoint = cmd[2]
    if not re.fullmatch(r'/?[A-Za-z0-9_./?=&%+-]+', endpoint) or '..' in endpoint or endpoint.startswith('//'):
        raise ValueError('credential_api_endpoint_not_allowed')
    # Only GET queries, with display/pagination flags; no headers, alternate
    # hosts, request bodies, shell commands or auth-token export.
    flags = cmd[3:]
    i = 0
    while i < len(flags):
        flag = flags[i]
        if flag in ('--paginate', '--slurp', '--silent'):
            i += 1
        elif flag in ('--jq', '-q', '--template', '-t') and i + 1 < len(flags):
            i += 2
        elif flag in ('--method', '-X') and i + 1 < len(flags) and flags[i + 1] == 'GET':
            i += 2
        else:
            raise ValueError('credential_api_option_not_allowed')


def github_check(config, *, ssh=True):
    values = config['github']
    env = command_env(config, 'github')
    result = subprocess.run(['gh', 'api', 'user', '--jq', '.login'], env=env,
                            capture_output=True, text=True, timeout=30)
    if result.returncode or result.stdout.strip() != values['login']:
        raise ValueError('github_token_identity_check_failed')
    if ssh:
        host = values.get('ssh_host')
        if not host:
            raise ValueError('github_ssh_host_missing')
        cmd = shlex.split(values.get('ssh_command', 'ssh -o BatchMode=yes -o ConnectTimeout=10'))
        result = subprocess.run([*cmd, '-T', host], capture_output=True, text=True, timeout=20)
        if result.returncode not in (0, 1) or f"Hi {values['login']}!" not in result.stdout + result.stderr:
            raise ValueError('github_ssh_identity_check_failed')
    return {'github_login': values['login'], 'api_verified': True, 'ssh_verified': ssh}


def swe_check(config):
    values = config['swe']
    base = 'https://swe.jzxhnh.com/api/v1'
    opener = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
    # Login creates a session only; never uploads or creates a task.
    if values.get('username') and values.get('password'):
        request = urllib.request.Request(base + '/auth/login', data=json.dumps({
            'username': values['username'], 'password': values['password']}).encode(),
            headers={'Content-Type': 'application/json'})
        with opener.open(request, timeout=30) as response:
            response.read(100000)
        headers = {}
    else:
        if not values.get('session') or not values.get('csrf'):
            raise ValueError('swe_credentials_incomplete')
        headers = {'Cookie': f"swe_qa_session={values['session']}; swe_qa_csrf={values['csrf']}"}
    with opener.open(urllib.request.Request(base + '/auth/me', headers=headers), timeout=30) as response:
        data = json.load(response)
    user = data.get('data', data)
    if not user.get('username') or (values.get('username') and user['username'] != values['username']):
        raise ValueError('swe_identity_check_failed')
    return {'swe_username': user['username'], 'authenticated': True}


def import_local(destination, login, ssh_host):
    if destination.exists() or destination.is_symlink():
        raise ValueError('config_exists_refuse_overwrite')
    legacy = destination.parent / 'auth.json'
    if legacy.is_symlink() or legacy.stat().st_mode & 0o077:
        raise ValueError('legacy_config_permissions_unsafe')
    swe = json.loads(legacy.read_text())
    token = subprocess.run(['gh', 'auth', 'token', '-h', 'github.com', '-u', login],
                           capture_output=True, text=True, timeout=15)
    if token.returncode or not token.stdout.strip():
        raise ValueError('local_github_token_unavailable')
    config = {'schema_version': 1, 'swe': {'username': swe['username'], 'password': swe['password']},
              'github': {'login': login, 'token': token.stdout.strip(), 'ssh_host': ssh_host,
                         'transport': 'ssh', 'ssh_command': 'ssh -o ProxyCommand=none -o BatchMode=yes -o StrictHostKeyChecking=yes -o ConnectTimeout=10'}}
    github_check(config)
    swe_check(config)
    destination.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    os.chmod(destination.parent, 0o700)
    fd, tmp = tempfile.mkstemp(dir=destination.parent, prefix='.config-')
    try:
        with os.fdopen(fd, 'w') as stream:
            json.dump(config, stream, ensure_ascii=False, indent=2)
            stream.write('\n')
        # Atomic exclusive publication, never replaces a concurrent config.
        os.link(tmp, destination)
    finally:
        os.unlink(tmp)
    return {'configured': True, 'permissions': '0600', 'github_login': login}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--config', type=Path, default=DEFAULT_CONFIG)
    sub = parser.add_subparsers(dest='action', required=True)
    imp = sub.add_parser('import-local')
    imp.add_argument('--github-login', required=True)
    imp.add_argument('--ssh-host', required=True)
    check = sub.add_parser('check')
    check.add_argument('--service', choices=('swe', 'github', 'all'), default='all')
    run = sub.add_parser('run')
    run.add_argument('--service', choices=('swe', 'github'), required=True)
    run.add_argument('command', nargs=argparse.REMAINDER)
    args = parser.parse_args()
    try:
        if args.action == 'import-local':
            result = import_local(args.config.expanduser(), args.github_login, args.ssh_host)
        else:
            config = load_config(args.config)
            if args.action == 'run':
                cmd = args.command[1:] if args.command[:1] == ['--'] else args.command
                if not cmd:
                    raise ValueError('command_missing')
                validate_run_command(cmd, args.service)
                if args.service == 'github':
                    github_check(config, ssh=False)
                return subprocess.call(cmd, env=command_env(config, args.service))
            result = {}
            if args.service in ('swe', 'all'):
                result.update(swe_check(config))
            if args.service in ('github', 'all'):
                result.update(github_check(config))
        print(json.dumps(result, ensure_ascii=False))
        return 0
    except Exception as error:
        # Never emit external response bodies, commands or exception strings.
        code = str(error) if isinstance(error, ValueError) and re.fullmatch('[a-z_]+', str(error)) else 'credential_operation_failed'
        print(json.dumps({'ok': False, 'error_code': code}))
        return 2


if __name__ == '__main__':
    raise SystemExit(main())
