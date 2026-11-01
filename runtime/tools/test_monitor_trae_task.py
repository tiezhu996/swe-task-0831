import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import monitor_trae_task as monitor


class MonitorTests(unittest.TestCase):
    def test_incremental_exact_session_completion_followup_and_rate_limit(self):
        with tempfile.TemporaryDirectory() as tmp:
            log = Path(tmp) / 'renderer.log'
            log.write_text('[DoneHandler] {"sessionId":"other","status":"completed","text":"target"}\n'
                           '[PlanItemHandler] {"sessionId":"target"}\n')
            state = {'workspace': tmp, 'task_id': 'task', 'status': 'trae_running'}
            with patch.object(monitor, 'git_signature', return_value='same'):
                result, cp = monitor.check(state, {}, [log], 'target', now=1000)
                self.assertEqual(result['action'], 'running_changed')
                with log.open('a') as f:
                    f.write('[DoneHandler] {"sessionId":"target","status":"completed"}\n')
                result, _ = monitor.check(state, cp, [log], 'target', now=1100)
                self.assertEqual(result['action'], 'rate_limited')
                result, cp = monitor.check(state, cp, [log], 'target', now=1900)
                self.assertEqual(result['action'], 'confirm_completion_then_collect_verify_submit')
                self.assertLess(result['new_log_bytes'], log.stat().st_size)
                with patch.object(monitor, 'read_increment', side_effect=AssertionError('no rescan')):
                    result, _ = monitor.check(state, cp, [log], 'target', now=1950)
                    self.assertEqual(result['action'], 'confirm_completion_then_collect_verify_submit')
                    state['status'] = 'session_verified'
                    result, _ = monitor.check(state, cp, [log], 'target', now=1960)
                    self.assertEqual(result['action'], 'verify_frozen_rubric')
                # A later stopped stream must invalidate a previous completion
                # candidate instead of permanently hiding all new log events.
                state['status'] = 'trae_running'
                with log.open('a') as f:
                    f.write('[DoneHandler] {"sessionId":"target","status":"cancelled"}\n')
                result, _ = monitor.check(state, cp, [log], 'target', now=2800)
                self.assertEqual(result['action'], 'preserve_abnormal_stop_no_resume')

    def test_submission_and_uncertain_writes_never_read_or_create(self):
        for state, action in (({'submission': {'id': 1488}}, 'already_submitted_no_create'),
                              ({'pending_operation': {'outcome': 'uncertain'}}, 'reconcile_pending_write_only')):
            with patch.object(monitor, 'git_signature', side_effect=AssertionError('no source access')):
                result, cp = monitor.check(state, {}, [], 'session')
            self.assertEqual(result['action'], action)
            self.assertEqual(cp, {})

    def test_abnormal_and_partial_lines_are_not_completion(self):
        with tempfile.TemporaryDirectory() as tmp:
            log = Path(tmp) / 'renderer.log'
            log.write_text('[DoneHandler] {"sessionId":"target","status":"cancelled"}\npartial')
            observation = monitor.read_increment(log, 'target', {})
            self.assertEqual(observation['terminal_event'], 'cancelled')
            self.assertEqual(observation['offset'], log.stat().st_size - len('partial'))
            state = {'workspace': tmp, 'task_id': 't', 'status': 'trae_running'}
            with patch.object(monitor, 'git_signature', return_value='same'):
                result, _ = monitor.check(state, {}, [log], 'target', now=1000)
            self.assertEqual(result['action'], 'preserve_abnormal_stop_no_resume')

    def test_binding_change_blocks(self):
        with self.assertRaisesRegex(ValueError, 'binding_changed'):
            monitor.check({'workspace': '/tmp/a', 'task_id': 't'}, {'binding': {}}, [], 's')
