#!/usr/bin/env python3
"""Local fixtures only; never read a real clipboard, log, or session token."""

import contextlib
from datetime import datetime, timedelta
import hashlib
import io
import json
import os
from pathlib import Path
import stat
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

import capture_trae_session as capture


def synthetic_token():
    return (
        "a" * 16 + ":" + "b" * 32 + "_" + "c" * 24 + "."
        + "d" * 24 + "." + "e" * 24
        + ":TraeCode CN.3.3.90.no_sid.no_ppe.T(2026/10/5 20:14:45)"
    )


class CounterFixture:
    DEFAULT_TRAE_ROOTS = ()

    def __init__(self, logs):
        self.logs = logs
        self.session = "c" * 24
        self.workspace_session = self.session
        self.resolved_sessions = {self.session}
        self.events = {log: ({"plan-1", "plan-2"}, {"turn-1"}, True) for log in logs}
        self.seen_roots = None

    def candidate_logs(self, roots):
        self.seen_roots = roots
        return self.logs

    def session_ids_in_token(self, token, logs):
        return self.resolved_sessions

    def discover_session(self, workspace, roots):
        return self.workspace_session

    def inspect_log(self, log, session):
        return self.events[log]


class CaptureTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.logs = [self.root / "renderer-1.log", self.root / "renderer-2.log"]
        for index, log in enumerate(self.logs):
            log.write_text("fixture log " + str(index), encoding="utf-8")
        self.counter = CounterFixture(self.logs)
        self.plan = {"payload": {"effective_rounds": 2}}
        self.plan_path = self.root / "submission-plan.json"
        self.plan_path.write_text(json.dumps(self.plan), encoding="utf-8")
        self.token = synthetic_token()
        self.audit_path = self.root / "submission-session-audit.json"

    def tearDown(self):
        self.temporary.cleanup()

    def verify(self, token=None):
        return capture.verify_capture(
            self.plan, self.token if token is None else token,
            self.workspace, self.counter, (),
        )

    def run_cli(self, extra=(), clipboard=None):
        stdout, stderr = io.StringIO(), io.StringIO()
        args = ["--plan", str(self.plan_path), "--workspace", str(self.workspace), *extra]
        with (
            contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr),
            patch.object(capture, "load_counter", return_value=self.counter),
            patch.object(capture.subprocess, "check_output", return_value=(self.token if clipboard is None else clipboard).encode("utf-8")) as clipboard_read,
        ):
            code = capture.run(args)
        return code, stdout.getvalue(), stderr.getvalue(), clipboard_read

    def test_deduplicates_mirrored_plan_events_and_hashes_matched_logs(self):
        audit = self.verify()
        self.assertEqual(audit["effective_round_count"], 2)
        self.assertEqual(audit["matching_log_count"], 2)
        self.assertEqual(audit["matched_log_sha256"], [hashlib.sha256(log.read_bytes()).hexdigest() for log in self.logs])
        self.assertEqual(audit["workspace_sha256"], hashlib.sha256(str(self.workspace.resolve()).encode()).hexdigest())
        self.assertTrue(audit["verified_at"].endswith("+00:00"))

    def test_rounds_are_unique_plan_events_not_model_turns(self):
        self.counter.events[self.logs[0]] = ({"plan-1"}, {"turn-1", "turn-2", "turn-3"}, True)
        self.counter.events[self.logs[1]] = ({"plan-2"}, set(), False)
        self.assertEqual(self.verify()["effective_round_count"], 2)

    def test_accepts_current_trae_cn_product_label(self):
        current = self.token.replace(":TraeCode CN.3.3.90.", ":TRAE CN.3.4.7.", 1)
        self.assertNotEqual(current, self.token)
        for token in (self.token, current):
            with self.subTest(label=token.split(":")[-1].split(".")[0]):
                self.verify(token)
        with self.assertRaises(capture.CaptureError):
            self.verify(current.replace(":TRAE CN.", ":TraeWork.", 1))

    def test_rejects_short_truncated_or_whitespace_modified_tokens(self):
        for value in [self.token[:-1], self.token[:90] + "...", "c" * 24, self.token + "\n", " " + self.token, self.token + " ", 123, None]:
            with self.subTest(token_type=type(value).__name__):
                with self.assertRaises(capture.CaptureError):
                    capture.verify_capture(self.plan, value, self.workspace, self.counter, ())

    def test_stdin_source_does_not_use_clipboard_or_print_token(self):
        stdin = io.TextIOWrapper(io.BytesIO(self.token.encode("utf-8")), encoding="utf-8")
        with patch.object(capture.sys, "stdin", stdin):
            code, stdout, stderr, clipboard = self.run_cli(extra=("--stdin", "--apply"))
        self.assertEqual(code, 0)
        clipboard.assert_not_called()
        self.assertNotIn(self.token, stdout + stderr)
        self.assertEqual(json.loads(self.audit_path.read_text())["source"], "human-entered local web form")

    def test_rejects_invalid_round_types_or_mismatched_counts(self):
        for rounds in [True, False, 2.0, "2", None, -2, 0, 1, 3]:
            with self.subTest(rounds=rounds):
                self.plan["payload"]["effective_rounds"] = rounds
                with self.assertRaises(capture.CaptureError):
                    self.verify()

    def test_rejects_ambiguous_or_unrelated_sessions_and_workspace_mismatch(self):
        for sessions in [set(), {"c" * 24, "d" * 24}, {"f" * 24}, ["c" * 24]]:
            self.counter.resolved_sessions = sessions
            with self.assertRaises(capture.CaptureError):
                self.verify()
        self.counter.resolved_sessions = {"c" * 24}
        self.counter.workspace_session = "d" * 24
        with self.assertRaises(capture.CaptureError):
            self.verify()

    def test_rejects_no_logs_missing_logs_or_events(self):
        self.counter.logs = []
        with self.assertRaises(capture.CaptureError):
            self.verify()
        self.counter.logs = [self.root / "missing.log"]
        with self.assertRaises(OSError):
            self.verify()
        self.counter.logs = self.logs
        self.counter.events = {log: (set(), {"turn-1"}, True) for log in self.logs}
        with self.assertRaises(capture.CaptureError):
            self.verify()

    def test_rejects_logs_changed_during_verification(self):
        original = self.counter.inspect_log

        def mutate(log, session):
            log.write_text("changed fixture", encoding="utf-8")
            return original(log, session)

        self.counter.inspect_log = mutate
        with self.assertRaises(capture.CaptureError):
            self.verify()

    def test_ignores_churn_in_logs_that_carry_no_session_events(self):
        # Trae appends to unrelated windows' renderer logs continuously. Those
        # lines cannot change the measured rounds, so they must not fail a task.
        unrelated = self.root / "renderer-unrelated.log"
        unrelated.write_text("unrelated fixture", encoding="utf-8")
        self.counter.logs = [*self.logs, unrelated]
        self.counter.events[unrelated] = (set(), set(), False)
        original = self.counter.inspect_log

        def append_unrelated(log, session):
            if log == unrelated:
                log.write_text("unrelated fixture with a newer line", encoding="utf-8")
            return original(log, session)

        self.counter.inspect_log = append_unrelated
        audit = self.verify()
        self.assertEqual(audit["matching_log_count"], 2)
        self.assertNotIn(hashlib.sha256(unrelated.read_bytes()).hexdigest(), audit["matched_log_sha256"])

    def test_rejects_a_log_that_gains_session_events_during_verification(self):
        original = self.counter.inspect_log
        seen = {}

        def gain_events(log, session):
            seen[log] = seen.get(log, 0) + 1
            if log == self.logs[1]:
                return ({"plan-2"}, {"turn-1"}, True) if seen[log] > 1 else (set(), set(), False)
            return original(log, session)

        self.counter.inspect_log = gain_events
        with self.assertRaises(capture.CaptureError):
            self.verify()

    def test_rejects_added_log_or_workspace_session_switch_during_verification(self):
        original = self.counter.inspect_log

        def switch(log, session):
            self.counter.workspace_session = "d" * 24
            return original(log, session)

        self.counter.inspect_log = switch
        with self.assertRaises(capture.CaptureError):
            self.verify()
        self.counter.workspace_session = self.counter.session
        added = self.root / "new-renderer.log"
        added.write_text("fixture rotation", encoding="utf-8")
        snapshots = iter([self.logs, self.logs + [added]])
        self.counter.inspect_log = original
        self.counter.candidate_logs = lambda roots: next(snapshots)
        with self.assertRaises(capture.CaptureError):
            self.verify()

    def test_dry_run_is_read_only_and_output_is_redacted(self):
        original = self.plan_path.read_bytes()
        code, stdout, stderr, _ = self.run_cli()
        self.assertEqual(code, 0)
        self.assertFalse(json.loads(stdout)["applied"])
        self.assertEqual(self.plan_path.read_bytes(), original)
        self.assertFalse(self.audit_path.exists())
        self.assertEqual(stderr, "")
        self.assertNotIn(self.token, stdout)
        self.assertNotIn("c" * 24, stdout)
        self.assertNotIn(str(self.workspace), stdout)

    def test_apply_preserves_exact_original_and_writes_private_files(self):
        code, stdout, stderr, _ = self.run_cli(["--apply"])
        self.assertEqual(code, 0)
        saved = json.loads(self.plan_path.read_text(encoding="utf-8"))
        self.assertEqual(saved["payload"]["trae_session_id"], self.token)
        audit = json.loads(self.audit_path.read_text(encoding="utf-8"))
        self.assertEqual(audit["token_sha256"], hashlib.sha256(self.token.encode()).hexdigest())
        self.assertTrue(audit["applied"])
        for path in [self.plan_path, self.audit_path]:
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        self.assertNotIn(self.token, stdout + stderr + self.audit_path.read_text())

    def test_initial_capture_updates_missing_or_zero_rounds_from_unique_events(self):
        for rounds in [None, 0]:
            with self.subTest(rounds=rounds):
                self.plan = {"payload": {}}
                if rounds is not None:
                    self.plan["payload"]["effective_rounds"] = rounds
                self.plan_path.write_text(json.dumps(self.plan), encoding="utf-8")
                state = self.task_state(status="trae_running")
                original_state = state.read_bytes()
                code, stdout, stderr, _ = self.run_cli(["--apply", "--update-rounds", "--task-state", str(state)])
                self.assertEqual(code, 0)
                saved = json.loads(self.plan_path.read_text())
                self.assertEqual(saved["payload"]["effective_rounds"], 2)
                self.assertEqual(saved["payload"]["trae_session_id"], self.token)
                self.assertEqual(state.read_bytes(), original_state)
                self.assertTrue(json.loads(stdout)["rounds_updated"])
                self.assertNotIn(self.token, stdout + stderr)
                self.assertEqual(stat.S_IMODE(self.plan_path.stat().st_mode), 0o600)
                self.audit_path.unlink()

    def test_session_captured_draft_may_update_rounds(self):
        state = self.task_state(status="session_captured")
        code, _, _, _ = self.run_cli(["--apply", "--update-rounds", "--task-state", str(state)])
        self.assertEqual(code, 0)

    def test_round_update_requires_apply_state_and_initial_clipboard_capture(self):
        state = self.task_state(status="trae_running")
        options = [
            ["--update-rounds", "--task-state", str(state)],
            ["--apply", "--update-rounds"],
            ["--apply", "--update-rounds", "--from-plan", "--task-state", str(state)],
        ]
        original = self.plan_path.read_bytes()
        for extra in options:
            with self.subTest(extra=extra):
                code, _, _, clipboard = self.run_cli(extra)
                self.assertEqual(code, 1)
                clipboard.assert_not_called()
                self.assertEqual(self.plan_path.read_bytes(), original)
                self.assertFalse(self.audit_path.exists())

    def test_round_update_rejects_frozen_or_wrong_task_states(self):
        original = self.plan_path.read_bytes()
        for status in [None, "initialized", "precheck_passed", "session_verified", "artifacts_verified", "dry_run_passed", "submitting", "submitted", "qc_pending", "qc_rejected", "qc_accepted", "closed"]:
            with self.subTest(status=status):
                state = self.task_state(status=status)
                code, _, _, clipboard = self.run_cli(["--apply", "--update-rounds", "--task-state", str(state)])
                self.assertEqual(code, 1)
                clipboard.assert_not_called()
                self.assertEqual(self.plan_path.read_bytes(), original)
                self.assertFalse(self.audit_path.exists())

    def test_round_update_rejects_existing_or_invalid_submission_records(self):
        for existing in [1481, -1, True, "1481", None]:
            with self.subTest(plan_existing=existing):
                self.plan["existing_submission_id"] = existing
                self.plan_path.write_text(json.dumps(self.plan))
                original = self.plan_path.read_bytes()
                state = self.task_state(status="trae_running")
                code, _, _, _ = self.run_cli(["--apply", "--update-rounds", "--task-state", str(state)])
                self.assertEqual(code, 1)
                self.assertEqual(self.plan_path.read_bytes(), original)
                self.assertFalse(self.audit_path.exists())
        self.plan.pop("existing_submission_id")
        self.plan_path.write_text(json.dumps(self.plan))
        for existing in [1481, -1, True, "1481", None]:
            with self.subTest(state_existing=existing):
                state = self.task_state(status="trae_running", submission={"id": existing})
                code, _, _, clipboard = self.run_cli(["--apply", "--update-rounds", "--task-state", str(state)])
                self.assertEqual(code, 1)
                clipboard.assert_not_called()
                self.assertFalse(self.audit_path.exists())

    def test_round_update_rejects_changed_logs_and_empty_plan_events(self):
        state = self.task_state(status="trae_running")
        original_plan = self.plan_path.read_bytes()
        original_inspect = self.counter.inspect_log

        def mutate(log, session):
            log.write_text("changed fixture")
            return original_inspect(log, session)

        self.counter.inspect_log = mutate
        code, _, _, _ = self.run_cli(["--apply", "--update-rounds", "--task-state", str(state)])
        self.assertEqual(code, 1)
        self.assertEqual(self.plan_path.read_bytes(), original_plan)
        self.assertFalse(self.audit_path.exists())
        self.counter.inspect_log = original_inspect
        self.counter.events = {log: (set(), {"turn-1"}, True) for log in self.logs}
        code, _, _, _ = self.run_cli(["--apply", "--update-rounds", "--task-state", str(state)])
        self.assertEqual(code, 1)
        self.assertEqual(self.plan_path.read_bytes(), original_plan)
        self.assertFalse(self.audit_path.exists())

    def test_default_and_from_plan_never_update_missing_or_mismatched_rounds(self):
        for rounds in [None, 0, 1, 3]:
            self.plan["payload"].update(effective_rounds=rounds, trae_session_id=self.token)
            self.plan_path.write_text(json.dumps(self.plan))
            original = self.plan_path.read_bytes()
            for extra in [["--apply"], ["--apply", "--from-plan"]]:
                with self.subTest(rounds=rounds, extra=extra):
                    code, _, _, _ = self.run_cli(extra)
                    self.assertEqual(code, 1)
                    self.assertEqual(self.plan_path.read_bytes(), original)
                    self.assertFalse(self.audit_path.exists())

    def test_verify_capture_update_calculates_count_without_mutating_plan(self):
        plan = {"payload": {"effective_rounds": 0}}
        audit = capture.verify_capture(plan, self.token, self.workspace, self.counter, (), update=True)
        self.assertEqual(audit["effective_round_count"], 2)
        self.assertEqual(plan, {"payload": {"effective_rounds": 0}})

    def test_from_plan_does_not_read_clipboard(self):
        self.plan["payload"]["trae_session_id"] = self.token
        self.plan_path.write_text(json.dumps(self.plan), encoding="utf-8")
        code, _, _, clipboard_read = self.run_cli(["--from-plan"], clipboard="invalid")
        self.assertEqual(code, 0)
        clipboard_read.assert_not_called()

    def test_does_not_strip_clipboard_or_write_invalid_token(self):
        original = self.plan_path.read_bytes()
        code, stdout, stderr, _ = self.run_cli(["--apply"], clipboard=self.token + "\n")
        self.assertEqual(code, 1)
        self.assertIn("BLOCKED", stdout)
        self.assertEqual(stderr, "")
        self.assertEqual(self.plan_path.read_bytes(), original)
        self.assertFalse(self.audit_path.exists())

    def test_additional_trae_root_is_passed_to_counter(self):
        additional = self.root / "additional"
        code, _, _, _ = self.run_cli(["--trae-root", str(additional)])
        self.assertEqual(code, 0)
        self.assertEqual(self.counter.seen_roots, (additional.resolve(),))

    def task_state(self, **updates):
        state = {
            "schema_version": 1, "task_id": "fixture-task", "revision": 0,
            "workspace": str(self.workspace), "plan_path": str(self.plan_path),
        }
        state.update(updates)
        path = self.root / "task-state.json"
        path.write_text(json.dumps(state), encoding="utf-8")
        return path

    def test_task_state_is_read_only_and_only_digest_metadata_is_public(self):
        sensitive_task_id = "c" * 24
        path = self.task_state(task_id=sensitive_task_id)
        original = path.read_bytes()
        code, stdout, stderr, _ = self.run_cli(["--apply", "--task-state", str(path)])
        self.assertEqual(code, 0)
        self.assertEqual(path.read_bytes(), original)
        audit = json.loads(self.audit_path.read_text(encoding="utf-8"))
        self.assertEqual(audit["task_state"]["state_sha256"], hashlib.sha256(original).hexdigest())
        self.assertTrue(json.loads(stdout)["task_state_linked"])
        self.assertNotIn(sensitive_task_id, stdout + stderr)

    def test_task_state_init_may_have_an_unbound_plan(self):
        path = self.task_state(plan_path="")
        code, _, _, _ = self.run_cli(["--task-state", str(path)])
        self.assertEqual(code, 0)

    def test_rejects_task_state_workspace_plan_or_schema_mismatch(self):
        for update in [
            {"workspace": str(self.root)}, {"workspace": "relative"},
            {"plan_path": str(self.logs[0])}, {"plan_path": "relative"},
            {"schema_version": True}, {"schema_version": 1.0}, {"schema_version": 2},
            {"revision": True}, {"revision": -1}, {"task_id": ""},
        ]:
            path = self.task_state(**update)
            code, stdout, stderr, _ = self.run_cli(["--apply", "--task-state", str(path)])
            self.assertEqual(code, 1)
            self.assertIn("BLOCKED", stdout)
            self.assertEqual(stderr, "")
            self.assertFalse(self.audit_path.exists())

    def test_rejects_plan_changed_during_verification(self):
        original = self.counter.inspect_log

        def mutate(log, session):
            self.plan_path.write_text('{"changed": true}', encoding="utf-8")
            return original(log, session)

        self.counter.inspect_log = mutate
        code, _, _, _ = self.run_cli(["--apply"])
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(self.plan_path.read_text()), {"changed": True})
        self.assertFalse(self.audit_path.exists())

    def test_counter_prints_and_exception_details_are_redacted(self):
        def leak(token, logs):
            print(token)
            print(token, file=sys.stderr)
            raise RuntimeError(token)

        self.counter.session_ids_in_token = leak
        code, stdout, stderr, _ = self.run_cli(["--apply"])
        self.assertEqual(code, 1)
        self.assertNotIn(self.token, stdout + stderr)
        self.assertEqual(stderr, "")

    def test_counter_system_exit_is_a_redacted_failure(self):
        def exit_with_token(token, logs):
            raise SystemExit(token)

        self.counter.session_ids_in_token = exit_with_token
        code, stdout, stderr, _ = self.run_cli(["--apply"])
        self.assertEqual(code, 1)
        self.assertNotIn(self.token, stdout + stderr)
        self.assertEqual(stderr, "")

    def test_audit_symlink_is_rejected_before_writing_plan(self):
        original = self.plan_path.read_bytes()
        self.audit_path.symlink_to(self.logs[0])
        code, _, _, _ = self.run_cli(["--apply"])
        self.assertEqual(code, 1)
        self.assertEqual(self.plan_path.read_bytes(), original)

    def test_task_state_changed_during_verification_is_rejected(self):
        path = self.task_state()
        original = self.counter.inspect_log

        def mutate(log, session):
            path.write_text('{"changed": true}', encoding="utf-8")
            return original(log, session)

        self.counter.inspect_log = mutate
        code, _, _, _ = self.run_cli(["--apply", "--task-state", str(path)])
        self.assertEqual(code, 1)
        self.assertEqual(json.loads(path.read_text()), {"changed": True})
        self.assertFalse(self.audit_path.exists())

    def test_actual_counter_with_synthetic_logs_and_workspace_database(self):
        script = capture.default_counter_script()
        if not script.is_file():
            self.skipTest("installed Trae counter unavailable")
        self.counter = capture.load_counter(script)
        app_root = self.root / "Trae"
        storage = app_root / "User/workspaceStorage/fixture"
        storage.mkdir(parents=True)
        (storage / "workspace.json").write_text(json.dumps({"folder": self.workspace.as_uri()}), encoding="utf-8")
        with sqlite3.connect(storage / "state.vscdb") as connection:
            connection.execute("CREATE TABLE ItemTable (key TEXT, value TEXT)")
            connection.execute("INSERT INTO ItemTable VALUES (?, ?)", ("ai-chat-v2.lastActiveSessionId", "c" * 24))
        lines = [
            '[PlanItemHandler] New plan item created ' + json.dumps({"sessionId": "c" * 24, "planItemId": plan}, separators=(",", ":"))
            for plan in ["plan-1", "plan-2", "plan-1"]
        ]
        for window in ["window1", "window2"]:
            log = app_root / "logs/20261005" / window / "renderer.log"
            log.parent.mkdir(parents=True)
            log.write_text("\n".join(lines), encoding="utf-8")
        self.counter.DEFAULT_TRAE_ROOTS = (app_root,)
        self.plan["payload"]["trae_session_id"] = self.token
        self.plan_path.write_text(json.dumps(self.plan), encoding="utf-8")
        code, stdout, stderr, _ = self.run_cli(["--from-plan"])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(stdout)["effective_round_count"], 2)
        self.assertEqual(json.loads(stdout)["matching_log_count"], 2)
        self.assertNotIn(self.token, stdout + stderr)
        self.plan["payload"].pop("effective_rounds")
        self.plan_path.write_text(json.dumps(self.plan), encoding="utf-8")
        state = self.task_state(status="trae_running")
        code, stdout, stderr, _ = self.run_cli(["--apply", "--update-rounds", "--task-state", str(state)])
        self.assertEqual(code, 0)
        self.assertEqual(json.loads(self.plan_path.read_text())["payload"]["effective_rounds"], 2)
        self.assertTrue(json.loads(stdout)["rounds_updated"])
        self.assertNotIn(self.token, stdout + stderr)

    def test_atomic_write_failure_preserves_original_and_removes_temporary(self):
        original = self.plan_path.read_bytes()
        with patch.object(capture.os, "replace", side_effect=OSError("fixture")):
            with self.assertRaises(OSError):
                capture.save_private_json(self.plan_path, {"updated": True})
        self.assertEqual(self.plan_path.read_bytes(), original)
        self.assertEqual(list(self.root.glob(".session-*")), [])

    def test_serialization_failure_and_symlink_do_not_overwrite(self):
        original = self.plan_path.read_bytes()
        with self.assertRaises(TypeError):
            capture.save_private_json(self.plan_path, {"unsupported": object()})
        self.assertEqual(self.plan_path.read_bytes(), original)
        self.assertEqual(list(self.root.glob(".session-*")), [])
        link = self.root / "link.json"
        link.symlink_to(self.plan_path)
        with self.assertRaises(capture.CaptureError):
            capture.save_private_json(link, {"updated": True})
        self.assertEqual(self.plan_path.read_bytes(), original)


class HistoricalSnapshotTests(unittest.TestCase):
    """Historical runs are counted from Trae's per-session snapshot repository.

    That repository outlives the rotated renderer log, so it is the only local
    evidence for a finished run. Rounds follow the tc-hook-kit rule: unique
    tool_use_id values, with the exclusion list recorded as not applied because
    these tags carry no tool name.
    """

    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.root = Path(self.temporary.name).resolve()
        self.workspace = self.root / "workspace"
        self.workspace.mkdir()
        self.counter = CounterFixture(())
        self.session = "c" * 24
        self.turn = "e" * 24
        self.plan = {"payload": {}}
        self.token = synthetic_token()

    def tearDown(self):
        self.temporary.cleanup()

    def tag_date(self):
        moment = datetime.fromtimestamp(capture.token_timestamp(self.token)).astimezone()
        return moment.strftime("%Y-%m-%dT%H:%M:%S%z")

    def make_snapshot(self, session=None, turn=None, calls=("1" * 24, "2" * 24, "3" * 24), stamp=None):
        session = self.session if session is None else session
        turn = self.turn if turn is None else turn
        repo = self.root / "ModularData/ai-agent/snapshot" / session / "v2"
        repo.mkdir(parents=True, exist_ok=True)
        moment = self.tag_date() if stamp is None else stamp
        environment = dict(
            os.environ,
            GIT_AUTHOR_NAME="fixture", GIT_AUTHOR_EMAIL="fixture@example.test",
            GIT_COMMITTER_NAME="fixture", GIT_COMMITTER_EMAIL="fixture@example.test",
            GIT_AUTHOR_DATE=moment, GIT_COMMITTER_DATE=moment,
        )

        def git(*arguments):
            subprocess.run(["git", "-C", str(repo), *arguments], check=True, capture_output=True, env=environment)

        subprocess.run(["git", "init", "-q", str(repo)], check=True, capture_output=True, env=environment)
        (repo / "file.txt").write_text("fixture", encoding="utf-8")
        git("add", ".")
        git("commit", "-q", "-m", "fixture")
        tags = [f"before-chat-turn-{turn}"]
        tags += [f"toolcall-{turn}-{call}" for call in calls]
        tags += [f"after-chat-turn-{turn}"]
        for tag in tags:
            git("tag", "-a", tag, "-m", tag)
        return repo

    def verify(self):
        return capture.verify_capture(
            self.plan, self.token, self.workspace, self.counter, (self.root,),
            update=True, historical=True,
        )

    def test_counts_unique_toolcall_tags(self):
        self.make_snapshot()
        audit = self.verify()
        self.assertEqual(audit["effective_round_count"], 3)
        self.assertEqual(audit["rounds_source"], "trae_session_snapshot_toolcall_tags")
        self.assertFalse(audit["exclusion_applied"])
        self.assertEqual(audit["chat_turns"], 1)
        self.assertEqual(audit["session_source"], "trae_session_snapshot")
        self.assertEqual(audit["matching_log_count"], 0)

    def test_ignores_toolcalls_from_other_turns(self):
        repo = self.make_snapshot()
        environment = dict(
            os.environ, GIT_AUTHOR_NAME="fixture", GIT_AUTHOR_EMAIL="fixture@example.test",
            GIT_COMMITTER_NAME="fixture", GIT_COMMITTER_EMAIL="fixture@example.test",
            GIT_AUTHOR_DATE=self.tag_date(), GIT_COMMITTER_DATE=self.tag_date(),
        )
        subprocess.run(
            ["git", "-C", str(repo), "tag", "-a", "toolcall-" + "f" * 24 + "-" + "9" * 24, "-m", "other"],
            check=True, capture_output=True, env=environment,
        )
        self.assertEqual(self.verify()["effective_round_count"], 3)

    def test_requires_a_snapshot_for_the_session(self):
        with self.assertRaises(capture.CaptureError) as raised:
            self.verify()
        self.assertIn("未找到该会话的历史快照", str(raised.exception))

    def test_requires_the_token_timestamp_on_the_turn_start(self):
        drifted = (datetime.fromtimestamp(capture.token_timestamp(self.token)) - timedelta(hours=3)).astimezone()
        self.make_snapshot(stamp=drifted.strftime("%Y-%m-%dT%H:%M:%S%z"))
        with self.assertRaises(capture.CaptureError) as raised:
            self.verify()
        self.assertIn("历史快照与 Session 原串时间不一致", str(raised.exception))

    def test_rejects_a_snapshot_without_toolcalls(self):
        self.make_snapshot(calls=())
        with self.assertRaises(capture.CaptureError) as raised:
            self.verify()
        self.assertIn("历史快照中没有可核实的工具调用", str(raised.exception))

    def test_rejects_a_session_bound_to_another_workspace(self):
        self.make_snapshot()
        self.counter.workspace_session = "f" * 24
        with self.assertRaises(capture.CaptureError) as raised:
            self.verify()
        self.assertIn("历史会话与指定工作区不一致", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
