import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import swe_credentials as credentials


class CredentialTests(unittest.TestCase):
    def config(self):
        return {'schema_version': 1, 'swe': {'username': 'tester', 'password': 'private-password'},
                'github': {'login': 'tester', 'token': 'private-token', 'ssh_host': 'github-tester'}}

    def test_permissions_and_symlinks_block(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / 'config.json'
            path.write_text(json.dumps(self.config()))
            path.chmod(0o600)
            self.assertEqual(credentials.load_config(path), self.config())
            path.chmod(0o644)
            with self.assertRaises(ValueError):
                credentials.load_config(path)
            path.chmod(0o600)
            link = path.with_name('link.json')
            link.symlink_to(path)
            with self.assertRaises(ValueError):
                credentials.load_config(link)

    def test_scoped_env_clears_stale_auth_without_overriding_global_config(self):
        with patch.dict(os.environ, {'SWE_SESSION': 'old', 'SWE_CSRF': 'old', 'GITHUB_TOKEN': 'other'}):
            env = credentials.command_env(self.config(), 'swe')
            self.assertNotIn('SWE_SESSION', env)
            self.assertEqual(env['SWE_PASSWORD'], 'private-password')
            env = credentials.command_env(self.config(), 'github')
            self.assertNotIn('GITHUB_TOKEN', env)
            self.assertEqual(env['GH_TOKEN'], 'private-token')
            self.assertEqual(os.environ['GITHUB_TOKEN'], 'other')

    def test_api_or_ssh_wrong_identity_is_rejected(self):
        import subprocess
        with patch.object(credentials.subprocess, 'run', return_value=subprocess.CompletedProcess([], 0, 'other\n', '')):
            with self.assertRaisesRegex(ValueError, 'token_identity'):
                credentials.github_check(self.config())
        results = [subprocess.CompletedProcess([], 0, 'tester\n', ''),
                   subprocess.CompletedProcess([], 1, '', 'Hi other!')]
        with patch.object(credentials.subprocess, 'run', side_effect=results):
            with self.assertRaisesRegex(ValueError, 'ssh_identity'):
                credentials.github_check(self.config())

    def test_wrapper_rejects_secret_dump_shell_alternate_host_and_writes(self):
        for cmd in (['env'], ['sh', '-c', 'env'], ['gh', 'auth', 'token'],
                    ['gh', 'api', 'https://example.com'],
                    ['gh', 'api', '//example.com'],
                    ['gh', 'api', 'user', '--hostname', 'example.com'],
                    ['gh', 'api', 'user', '-X', 'POST'],
                    ['gh', 'api', 'user', '-H', 'Authorization: secret']):
            with self.subTest(cmd=cmd), self.assertRaises(ValueError):
                credentials.validate_run_command(cmd, 'github')
        credentials.validate_run_command(['gh', 'api', 'user', '--jq', '.login'], 'github')
        credentials.validate_run_command(['gh', 'api', 'repos/test/repo', '-X', 'GET'], 'github')
        with self.assertRaises(ValueError):
            credentials.validate_run_command(['python3', '-c', 'print(1)'], 'swe')
