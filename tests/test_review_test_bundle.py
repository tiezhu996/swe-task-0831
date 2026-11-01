import json
import subprocess
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path


SCRIPT = Path(__file__).parents[1] / "scripts" / "review_test_bundle.py"


class TestBundleReviewTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.repo = self.root / "base"
        self.repo.mkdir()
        subprocess.run(["git", "init", "-q", str(self.repo)], check=True)
        subprocess.run(["git", "-C", str(self.repo), "config", "user.name", "review-test"], check=True)
        subprocess.run(["git", "-C", str(self.repo), "config", "user.email", "review-test@example.invalid"], check=True)
        (self.repo / "base.txt").write_text("base\n")
        subprocess.run(["git", "-C", str(self.repo), "add", "base.txt"], check=True)
        subprocess.run(["git", "-C", str(self.repo), "commit", "-qm", "base"], check=True)
        self.base_commit = subprocess.run(
            ["git", "-C", str(self.repo), "rev-parse", "HEAD"],
            check=True, capture_output=True, text=True,
        ).stdout.strip()

        self.tests = self.root / "tests"
        self.tests.mkdir()
        files = {
            "Dockerfile": "FROM busybox@sha256:" + "0" * 64 + "\n",
            "config.json": json.dumps({
                "task_id": "T1",
                "base_commit": self.base_commit,
                "f2p_node_ids": ["pkg.TestFeature"],
                "p2p_node_ids": ["pkg.TestRegression"],
                "grade": {"reports": ["$RUN_DIR/results.json"]},
            }),
            "grader.py": "print('grader')\n",
            "test.patch": """diff --git a/new.txt b/new.txt
new file mode 100644
--- /dev/null
+++ b/new.txt
@@ -0,0 +1 @@
+test data
""",
            "test.sh": (
                "#!/usr/bin/env bash\nset -uo pipefail\nerrors=()\n"
                "(cd \"$PRISTINE\" && git apply \"$TESTS_DIR/test.patch\" && go test -c -o /bin-dir/suite.test ./pkg)\n"
                "TEST_SOURCE=packaged\n"
                "setpriv --reuid=10001 /bin-dir/suite.test -test.v=test2json || errors+=(\"candidate failed\")\n"
                "python3 grader.py grade ${errors[@]+\"${errors[@]}\"}\n"
            ),
        }
        for name, content in files.items():
            (self.tests / name).write_text(content)
        evidence = self.root / "evidence"
        evidence.mkdir()
        for name in ("baseline.json", "order.json", "enum.json", "error.json", "import_export.json"):
            (evidence / name).write_text("{}\n")
        self.fairness = self.root / "fairness.json"
        self.fairness.write_text(json.dumps({
            "schema_version": 1,
            "task_id": "T1",
            "probes": {
                "order_variant": {"result": "pass", "reward": 1, "evidence": "evidence/order.json"},
                "enum_variant": {"result": "pass", "reward": 1, "evidence": "evidence/enum.json"},
                "error_variant": {"result": "pass", "reward": 1, "evidence": "evidence/error.json"},
                "p2p_import_export": {"result": "pass", "reward": 1, "evidence": "evidence/import_export.json"},
                "json_field_alias": {"result": "pass", "reward": 1, "evidence": "evidence/import_export.json"},
                "git_fallback": {"result": "pass", "reward": 1, "evidence": "evidence/order.json"},
                "zero_hit": {"result": "pass", "reward": 1, "evidence": "evidence/import_export.json"},
                "multi_kind_counts": {"result": "pass", "reward": 1, "evidence": "evidence/import_export.json"},
                "existing_test_regression": {"result": "pass", "reward": 1, "evidence": "evidence/import_export.json"},
                "pretty_json": {"result": "pass", "reward": 1, "evidence": "evidence/import_export.json"},
                "error_vague": {"result": "fail", "reward": 0, "evidence": "evidence/error.json"},
                "target_echo_only": {"result": "fail", "reward": 0, "evidence": "evidence/error.json"},
                "tempfile_write": {"result": "fail", "reward": 0, "evidence": "evidence/error.json"},
                "error_panic": {"result": "fail", "reward": 0, "evidence": "evidence/error.json"},
                "error_generic": {"result": "fail", "reward": 0, "evidence": "evidence/error.json"},
                "stub_output": {"result": "fail", "reward": 0, "evidence": "evidence/error.json"},
                "merged_kinds": {"result": "fail", "reward": 0, "evidence": "evidence/error.json"},
            },
        }))
        self.bundle = self.root / "T1-testzip.zip"
        self.write_zip()
        self.defense = self.root / "T1-defense.md"
        self.defense.write_text("# T1\n题号：T1\n")
        self.report = self.root / "review.json"

    def tearDown(self):
        self.temp.cleanup()

    def write_zip(self, extra_member=None):
        with zipfile.ZipFile(self.bundle, "w", zipfile.ZIP_DEFLATED) as archive:
            archive.writestr("tests/", "")
            for path in sorted(self.tests.iterdir()):
                archive.write(path, f"tests/{path.name}")
            if extra_member:
                archive.writestr(extra_member, "unexpected")

    def run_review(self, tests_dir=None, bundle=None, fairness=None):
        return subprocess.run([
            sys.executable, str(SCRIPT),
            "--tests-dir", str(tests_dir or self.tests),
            "--zip", str(bundle or self.bundle),
            "--base-repo", str(self.repo),
            "--base-commit", self.base_commit,
            "--defense", str(self.defense),
            "--fairness-evidence", str(fairness or self.fairness),
            "--out", str(self.report),
        ], capture_output=True, text=True)

    def test_static_pass_never_claims_submission_ready(self):
        result = self.run_review()
        report = json.loads(self.report.read_text())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(report["status"], "STATIC_PASS")
        self.assertFalse(report["submission_ready"])

    def test_staging_change_after_packaging_is_blocked(self):
        (self.tests / "test.sh").write_text("#!/usr/bin/env bash\nexit 1\n")
        result = self.run_review()
        report = json.loads(self.report.read_text())
        self.assertEqual(result.returncode, 1)
        self.assertEqual(report["status"], "HOLD")
        self.assertIn("zip_matches_staging", report["blockers"])

    def test_path_traversal_member_is_blocked(self):
        self.write_zip("tests/../escape.txt")
        result = self.run_review()
        report = json.loads(self.report.read_text())
        self.assertEqual(result.returncode, 1)
        self.assertIn("zip_safety", report["blockers"])

    def assert_blocked(self, check, files):
        for name, content in files.items():
            (self.tests / name).write_text(content)
        self.write_zip()
        result = self.run_review()
        report = json.loads(self.report.read_text())
        self.assertEqual(result.returncode, 1, result.stdout)
        self.assertEqual(report["status"], "HOLD")
        self.assertIn(check, report["blockers"])

    def test_candidate_without_privilege_drop_is_blocked(self):
        self.assert_blocked("candidate_isolation", {"test.sh": "#!/usr/bin/env bash\ngo test ./...\npython3 grader.py grade\n"})

    def test_unpinned_base_image_is_blocked(self):
        self.assert_blocked("base_image_pinned", {"Dockerfile": "FROM busybox:latest\n"})

    def test_unguarded_empty_array_is_blocked(self):
        self.assert_blocked("empty_array_expansion", {"test.sh": (
            "#!/usr/bin/env bash\nset -uo pipefail\nerrors=()\nsetpriv --reuid=10001 true\n"
            "python3 grader.py grade \"${errors[@]}\"\n"
        )})

    def test_unconditional_success_exit_is_blocked(self):
        self.assert_blocked("entry_exit_status", {"test.sh": "#!/usr/bin/env bash\nsetpriv --reuid=10001 go test ./...\nexit 0\n"})

    def test_candidate_running_go_test_is_blocked(self):
        self.assert_blocked("scored_binaries_precompiled", {"test.sh": (
            "#!/usr/bin/env bash\nas_runner() { setpriv --reuid=10001 \"$@\"; }\n"
            "as_runner go test -json ./internal/deliverytests\npython3 grader.py grade\n"
        )})

    def test_scored_binary_built_in_candidate_tree_is_blocked(self):
        # Restoring baseline *_test.go still links submitted package code into the binary.
        self.assert_blocked("p2p_tests_from_trusted_source", {"test.sh": (
            "#!/usr/bin/env bash\nset -uo pipefail\nerrors=()\n"
            "git checkout \"$base\" -- internal/models/ApiKey_test.go\n"
            "go test -c -tags test -o /bin-dir/models.test ./internal/models\n"
            "(cd \"$PRISTINE\" && go test -c -o /bin-dir/suite.test ./pkg)\n"
            "setpriv --reuid=10001 /bin-dir/suite.test -test.v=test2json || errors+=(\"candidate failed\")\n"
            "python3 grader.py grade ${errors[@]+\"${errors[@]}\"}\n"
        )})

    def test_unused_test_patch_is_blocked(self):
        self.assert_blocked("test_patch_used", {"test.sh": (
            "#!/usr/bin/env bash\nset -uo pipefail\nerrors=()\n"
            "(cd \"$PRISTINE\" && go test -c -o /bin-dir/suite.test ./pkg)\n"
            "setpriv --reuid=10001 /bin-dir/suite.test -test.v=test2json || errors+=(\"candidate failed\")\n"
            "python3 grader.py grade ${errors[@]+\"${errors[@]}\"}\n"
        )})

    def test_literal_binding_is_blocked(self):
        self.assert_blocked("literal_binding_scan", {
            "harness.go": "package pkg\n// rules are not strictly sorted\n"
        })

    def test_missing_fairness_evidence_is_blocked(self):
        result = self.run_review(fairness=self.root / "missing-fairness.json")
        report = json.loads(self.report.read_text())
        self.assertEqual(result.returncode, 1)
        self.assertIn("fairness_evidence", report["blockers"])

    def test_git_archive_without_fallback_is_blocked(self):
        self.assert_blocked("git_fallback", {"test.sh": (
            "#!/usr/bin/env bash\nset -uo pipefail\nerrors=()\n"
            "git -C \"$APP\" archive \"$BASE_COMMIT\" | tar -x -C \"$PRISTINE\"\n"
            "git apply \"$TESTS_DIR/test.patch\"\n"
            "(cd \"$PRISTINE\" && go test -c -o /bin-dir/suite.test ./pkg)\n"
            "setpriv --reuid=10001 /bin-dir/suite.test -test.v=test2json || errors+=(\"candidate failed\")\n"
            "python3 grader.py grade ${errors[@]+\"${errors[@]}\"}\n"
        )})

    def test_fairness_negative_probe_must_be_blocked(self):
        data = json.loads(self.fairness.read_text())
        data["probes"]["error_vague"] = {"result": "pass", "reward": 1, "evidence": "evidence/error.json"}
        bad = self.root / "bad-fairness.json"
        bad.write_text(json.dumps(data))
        result = self.run_review(fairness=bad)
        report = json.loads(self.report.read_text())
        self.assertEqual(result.returncode, 1)
        self.assertIn("fairness_evidence", report["blockers"])

    def test_patch_source_drift_is_blocked(self):
        delivery = self.tests / "deliverytests"
        delivery.mkdir()
        (delivery / "foo.go").write_text("package deliverytests\nvar Value = 1\n")
        (self.tests / "test.patch").write_text(
            "diff --git a/internal/deliverytests/foo.go b/internal/deliverytests/foo.go\n"
            "new file mode 100644\n"
            "--- /dev/null\n"
            "+++ b/internal/deliverytests/foo.go\n"
            "@@ -0,0 +1,2 @@\n"
            "+package deliverytests\n"
            "+var Value = 2\n"
        )
        self.write_zip()
        result = self.run_review()
        report = json.loads(self.report.read_text())
        self.assertEqual(result.returncode, 1)
        self.assertIn("patch_source_sync", report["blockers"])

    def test_cache_artifacts_are_blocked(self):
        cache = self.tests / "__pycache__"
        cache.mkdir()
        (cache / "grader.cpython-312.pyc").write_bytes(b"cached")
        self.write_zip()
        result = self.run_review()
        report = json.loads(self.report.read_text())
        self.assertEqual(result.returncode, 1)
        self.assertIn("cache_artifact_scan", report["blockers"])

    def test_missing_input_still_writes_hold_report(self):
        result = self.run_review(tests_dir=self.root / "missing-tests")
        report = json.loads(self.report.read_text())
        self.assertEqual(result.returncode, 1)
        self.assertEqual(report["status"], "HOLD")
        self.assertFalse(report["submission_ready"])
        self.assertIn("tests_directory", report["blockers"])


if __name__ == "__main__":
    unittest.main()
