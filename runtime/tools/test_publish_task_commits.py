import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch
import publish_task_commits as publish


class PublicationTests(unittest.TestCase):
    def test_bound_ssh_transport_and_wrong_fork_owner(self):
        config = {'github': {'login': 'tester', 'transport': 'ssh', 'ssh_host': 'github-tester'}}
        source = publish.GitHubRepository('upstream', 'project')
        with patch.object(publish, 'CONFIG', config):
            self.assertEqual(publish.remote_url(source), 'git@github-tester:upstream/project.git')
            with patch.object(publish, 'repository_metadata') as metadata:
                with self.assertRaises(ValueError):
                    publish.ensure_fork(source, 'https://github.com/other/project')
                metadata.assert_not_called()

    def test_failed_push_preserves_prepared_checkout_and_uncertain_receipt(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            commands = []
            def command(cmd, **kwargs):
                commands.append(cmd)
                if cmd[1] == 'clone':
                    Path(cmd[-1]).mkdir()
                if 'rev-parse' in cmd:
                    return 'a' * 40 if '--verify' in cmd else 'b' * 40
                if 'push' in cmd:
                    raise publish.CommandError('actual push denied')
                return ''
            with patch.object(publish, 'run', side_effect=command), \
                 patch.object(publish, 'remote_branch_exists', return_value=False), \
                 patch.object(publish, 'apply_task_changes'), \
                 patch.object(publish.subprocess, 'run', return_value=subprocess.CompletedProcess([], 1)):
                with self.assertRaises(publish.CommandError):
                    publish.publish_task(publish.GitHubRepository('up', 'repo'),
                        publish.GitHubRepository('me', 'repo'), 'a' * 40, root, 'task1', 'message', root, False, ('n', 'e'))
            self.assertTrue((root / 'task1').is_dir())
            receipt = json.loads((root / 'task1-publication.json').read_text())
            self.assertEqual(receipt['outcome'], 'push_started_reconcile_before_retry')
            push = next(cmd for cmd in commands if 'push' in cmd)
            self.assertIn('--force-with-lease=refs/heads/task1:', push)

    def test_command_error_redacts_secrets(self):
        config = {'github': {'token': 'private-token'}, 'swe': {'password': 'private-password'}}
        with patch.object(publish, 'CONFIG', config), patch.object(publish.subprocess, 'run',
            return_value=subprocess.CompletedProcess([], 1, '', 'private-token private-password')):
            with self.assertRaises(publish.CommandError) as failure:
                publish.run(['gh', 'api', 'user'])
            self.assertNotIn('private-token', str(failure.exception))
            self.assertNotIn('private-password', str(failure.exception))
