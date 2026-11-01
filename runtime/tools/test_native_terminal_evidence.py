import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location("evidence", Path(__file__).with_name("native_terminal_evidence.py"))
evidence = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(evidence)


class NativeEvidenceTests(unittest.TestCase):
    def args(self, root, content, *, run_only=True, timeout=5):
        script = root / "check.sh"
        script.write_text(content)
        return argparse.Namespace(command_script=script, workspace=root, out_dir=root / "out", timeout=timeout, max_log_bytes=100000, run_only=run_only, screenshot_helper=root / "helper.py")

    def test_run_only_does_not_claim_screenshot_and_records_actual_failure(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            result = evidence.capture(self.args(root, "printf 'actual failure\\n'; exit 7\n"))
            self.assertEqual(result["exit_code"], 7)
            self.assertTrue(result["execution_completed"])
            self.assertFalse(result["completed"])
            self.assertEqual(result["source"], "subprocess_only")
            self.assertEqual(result["screenshot_sha256"], "")
            raw = root / "out/verification.raw.log"
            self.assertEqual(raw.read_text(), "actual failure\n")
            self.assertEqual(os.stat(raw).st_mode & 0o777, 0o600)
            self.assertEqual(result["log_sha256"], hashlib.sha256(raw.read_bytes()).hexdigest())

    def test_redaction_removes_controls_paths_user_email_and_tokens(self):
        text = "\x1b[31m/Users/tester/private/test.sh\x1b[0m tester tester@example.com\nAuthorization: Bearer secret\n" + "a" * 16 + ":" + "b" * 32 + "_" + "c" * 24 + ":TraeCode CN.3.3.90.no_sid.T(2026/10/5 20:14:45)\n"
        result = evidence.redact(text, identities=("tester",))
        self.assertNotIn("/Users/", result)
        self.assertNotIn("tester", result)
        self.assertNotIn("secret", result)
        self.assertNotIn("TraeCode", result)
        self.assertNotIn("\x1b", result)

    def test_permission_failure_keeps_log_and_incomplete_manifest(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            with mock.patch.object(evidence, "screenshot_native", side_effect=evidence.EvidenceError("screen_recording_permission_required")):
                result = evidence.capture(self.args(root, "printf 'tested\\n'\n", run_only=False))
            self.assertEqual(result["error_code"], "screen_recording_permission_required")
            self.assertEqual(result["exit_code"], 0)
            self.assertFalse(result["completed"])
            self.assertEqual(result["screenshot_path"], "")
            self.assertEqual(json.loads((root / "out/verification-manifest.json").read_text()), result)

    def test_timeout_terminates_process_and_records_incomplete_execution(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            result = evidence.capture(self.args(root, "printf 'started\\n'; sleep 10\n", timeout=0.1))
            self.assertEqual(result["error_code"], "verification_timeout")
            self.assertFalse(result["execution_completed"])
            self.assertNotEqual(result["exit_code"], 0)

    def test_does_not_overwrite_previous_evidence(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            args = self.args(root, "exit 0\n")
            evidence.capture(args)
            with self.assertRaisesRegex(evidence.EvidenceError, "evidence_already_exists"):
                evidence.capture(args)

    def test_synthetic_screenshot_mode_is_rejected(self):
        with mock.patch.object(evidence.platform, "system", return_value="Darwin"), mock.patch.dict(os.environ, {"CODEX_SCREENSHOT_TEST_MODE": "1"}):
            with self.assertRaisesRegex(evidence.EvidenceError, "synthetic_screenshot_mode_forbidden"):
                evidence.screenshot_native(Path("display"), Path("screenshot"), Path("helper"), "marker")

    def test_shell_shebang_is_respected_and_wide_text_is_wrapped(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            result = evidence.capture(self.args(root, "#!/bin/bash\n[[ 1 == 1 ]] && printf 'shell checked\\n'\n"))
            self.assertEqual(result["exit_code"], 0)
            self.assertEqual((root / "out/verification.log").read_text(), "shell checked\n")
        text = evidence.display_text("测" * 100, {"exit_code": 0, "execution_completed": True}, "marker")
        self.assertNotIn("测" * 55, text)

    def test_output_limit_and_changed_script_are_incomplete(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            args = self.args(root, "yes overflow\n")
            args.max_log_bytes = 200
            result = evidence.capture(args)
            self.assertEqual(result["error_code"], "verification_output_limit")
            self.assertFalse(result["execution_completed"])

            self.assertLessEqual((root / "out/verification.raw.log").stat().st_size, 200)
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            result = evidence.capture(self.args(root, 'printf changed >> "$0"\n'))
            self.assertEqual(result["error_code"], "verification_command_changed_during_execution")
            self.assertFalse(result["execution_completed"])

    def test_complete_rubric_summary_keeps_failures_and_excludes_noise(self):
        rows = [f'{i} {"FAIL" if i in (1, 2, 3, 5) else "PASS"} actual evidence' for i in range(1, 10)]
        log = 'noise traceback\n' * 80 + 'SWE_RUBRIC_RESULTS_BEGIN\n' + '\n'.join(rows) + '\nSWE_RUBRIC_RESULTS_END\nother logs\n'
        manifest = {"exit_code": 1, "execution_completed": True}
        display = evidence.display_text(log, manifest, 'marker', '1,2,3,4,5,6,7,8,9')
        for row in rows:
            self.assertIn(row, display)
        self.assertNotIn('traceback', display)
        self.assertNotIn('other logs', display)
        self.assertIn('PASS 5/9', display)
        self.assertIn('exit_code=1', display)
        for invalid in ('SWE_RUBRIC_RESULTS_BEGIN\n1 PASS\nSWE_RUBRIC_RESULTS_END',
                        'SWE_RUBRIC_RESULTS_BEGIN\n1 PASS\n1 FAIL\nSWE_RUBRIC_RESULTS_END'):
            with self.assertRaises(evidence.EvidenceError):
                evidence.display_text(invalid, manifest, 'marker', '1,2')

    def test_recapture_never_runs_verification_and_checks_original_hashes(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            args = self.args(root, "printf 'Frozen rubric results:\\n1 FAIL actual failure\\n'; exit 1\n")
            original = evidence.capture(args)
            recapture = argparse.Namespace(from_evidence=args.out_dir, out_dir=root / 'new',
                rubric_ids='1', run_only=True, screenshot_helper=root / 'helper.py')
            with mock.patch.object(evidence, 'run_verification', side_effect=AssertionError('must not execute')):
                result = evidence.recapture(recapture)
            self.assertEqual(result['exit_code'], 1)
            self.assertEqual(result['log_sha256'], original['log_sha256'])
            self.assertFalse(result['completed'])
            (args.out_dir / 'verification.log').write_text('changed to PASS')
            recapture.out_dir = root / 'another'
            with self.assertRaisesRegex(evidence.EvidenceError, 'hash_mismatch'):
                evidence.recapture(recapture)

    def test_concurrent_native_capture_is_rejected_before_any_ui_action(self):
        import fcntl
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(evidence.Path, 'home', return_value=Path(tmp)):
            private = Path(tmp) / '.config/swe-task'
            private.mkdir(parents=True)
            with (private / 'terminal-evidence.lock').open('a') as lock:
                fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
                with mock.patch.object(evidence, '_screenshot_native') as native:
                    with self.assertRaisesRegex(evidence.EvidenceError, 'dedicated_terminal_capture_busy'):
                        evidence.screenshot_native(Path('d'), Path('s'), Path('h'), 'm')
                    native.assert_not_called()

    def test_parallel_capture_queues_until_dedicated_terminal_is_free(self):
        import fcntl
        import threading
        with tempfile.TemporaryDirectory() as tmp, mock.patch.object(evidence.Path, 'home', return_value=Path(tmp)):
            private = Path(tmp) / '.config/swe-task'
            private.mkdir(parents=True)
            lock = (private / 'terminal-evidence.lock').open('a')
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            threading.Timer(0.5, lock.close).start()
            with mock.patch.object(evidence, '_screenshot_native', return_value={'completed': True}) as native:
                result = evidence.screenshot_native(Path('d'), Path('s'), Path('h'), 'm', lock_wait=10)
            self.assertEqual(result, {'completed': True})
            native.assert_called_once()


if __name__ == "__main__":
    unittest.main()
