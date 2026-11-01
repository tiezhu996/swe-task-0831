import json
import subprocess
import sys
import tempfile
import unittest
from argparse import Namespace
from datetime import datetime, timezone
from pathlib import Path
from types import SimpleNamespace

SCRIPTS = Path(__file__).parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))
import check_issue_feature_overlap as overlap
import screen_project as screen


def git(repo, *args):
    subprocess.run(["git", "-C", str(repo), *args], check=True, capture_output=True)


class FeatureOverlapTests(unittest.TestCase):
    def test_self_test(self):
        result = subprocess.run(
            [sys.executable, str(SCRIPTS / "check_issue_feature_overlap.py"), "--self-test"],
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("FAIL", result.stdout)

    def test_fetch_writes_sorted_corpus_and_refuses_overwrite(self):
        lines = [
            {"number": 2, "title": "b", "body": None, "state": "open", "url": "u2", "is_pull_request": True},
            {"number": 1, "title": "a", "body": "x", "state": "closed", "url": "u1", "is_pull_request": False},
        ]
        stdout = "\n".join(json.dumps(line) for line in lines)
        runner = lambda *a, **k: SimpleNamespace(returncode=0, stdout=stdout, stderr="")
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp) / "corpus.json"
            overlap.fetch("o/n", out, runner=runner)
            self.assertEqual([item["number"] for item in json.loads(out.read_text())], [1, 2])
            with self.assertRaises(ValueError):
                overlap.fetch("o/n", out, runner=runner)

    def test_term_hit_pull_request_must_be_classified(self):
        corpus = [
            {"number": 1, "title": "ARM image", "body": "", "is_pull_request": False},
            {"number": 2, "title": "Auth system", "body": "JWT login for pulls", "is_pull_request": True},
            {"number": 3, "title": "Typo", "body": "", "is_pull_request": True},
        ]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "corpus.json"
            path.write_text(json.dumps(corpus))
            import hashlib

            ledger = {
                "schema_version": 1,
                "corpus_sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
                "prompt_sha256": hashlib.sha256("题面".encode()).hexdigest(),
                "feature": "按人登录后扣月度流量",
                "search_terms": ["login", "auth", "quota", "登录", "额度", "流量"],
                "entries": [{"number": 1, "feature": "ARM 镜像", "relation": "unrelated"}],
            }
            errors, summary = overlap.check(path, ledger, "题面")
            self.assertTrue(any("#2" in e for e in errors), errors)
            self.assertFalse(any("#3" in e for e in errors), errors)  # untouched PRs need no entry
            self.assertEqual(summary["hit_items"], {"#2": ["login", "auth"]})

    def test_siblings_are_same_repo_prompts_except_own(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name, url, has_prompt in (
                ("1-a", "https://github.com/o/a.git", True),
                ("2-a", "https://github.com/o/a", True),
                ("3-a", "https://github.com/o/a", False),
                ("4-ab", "https://github.com/o/ab", True),
            ):
                (root / name).mkdir()
                (root / name / "task-state.json").write_text(json.dumps({"repo_url": url}))
                if has_prompt:
                    (root / name / "PROMPT.md").write_text(f"题面 {name}")
            siblings = overlap.discover_siblings(root, "o/a", root / "2-a" / "PROMPT.md")
        self.assertEqual(siblings, {"1-a": "题面 1-a"})

    def test_english_terms_match_whole_words_only(self):
        self.assertIsNone(overlap.term_pattern("auth").search("author name"))
        self.assertIsNotNone(overlap.term_pattern("auth").search("basic Auth support"))


class ScreenHelperTests(unittest.TestCase):
    def test_tree_excludes_vendor_and_generated_and_splits_tests(self):
        files = [
            "cmd/main.go", "internal/a.go", "internal/a_test.go", "vendor/x/y.go",
            "api/x.pb.go", "examples/demo.go", "Dockerfile", "deploy/compose.yaml",
            ".github/workflows/test.yml",
        ]
        tree = screen.analyze_tree(files, "Go")
        self.assertEqual((tree["source_files"], tree["test_files"]), (2, 1))
        self.assertEqual(tree["root_docker"], ["Dockerfile"])
        self.assertEqual(tree["compose_files"], ["deploy/compose.yaml"])

    def test_failing_tests_parses_names_and_packages(self):
        log = "--- FAIL: TestA (0.01s)\n    --- FAIL: TestA/sub (0.00s)\nFAIL\tgithub.com/x/pkg\t0.2s\nok  \tgithub.com/x/ok\n"
        self.assertEqual(
            screen.failing_tests(log),
            ["TestA", "TestA/sub", "package github.com/x/pkg"],
        )

    def test_history_counts_only_matching_repo(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name, url, rounds in (
                ("1-a", "https://github.com/o/a.git", 120),
                ("2-a", "https://github.com/o/a", 80),
                ("3-ab", "https://github.com/o/ab", 200),
            ):
                (root / name).mkdir()
                (root / name / "task-state.json").write_text(
                    json.dumps({"repo_url": url, "trae": {"effective_rounds": rounds}})
                )
            rows = screen.history(root, "o/a")
        self.assertEqual([r["task"] for r in rows], ["1-a", "2-a"])


class ScreenEndToEndTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.repo = Path(self.tmp.name) / "repo"
        self.repo.mkdir()
        git(self.repo, "init", "-q")
        git(self.repo, "config", "user.email", "t@t")
        git(self.repo, "config", "user.name", "t")
        (self.repo / "go.mod").write_text("module x\n\ngo 1.22\n")
        pkg = self.repo / "pkg"
        pkg.mkdir()
        for i in range(4):
            (pkg / f"f{i}.go").write_text("package pkg\n// uses ffmpeg\n" if i == 0 else "package pkg\n")
        (pkg / "f_test.go").write_text("package pkg\n")
        (pkg / "gen.go").write_text("package pkg\n\n//go:generate go run tool.go\n")
        workflows = self.repo / ".github" / "workflows"
        workflows.mkdir(parents=True)
        (workflows / "test.yml").write_text("jobs:\n  t:\n    steps:\n      - run: go generate ./...\n      - run: go test ./... --tags=test\n      - run: echo hi\n")
        git(self.repo, "add", ".")
        git(self.repo, "commit", "-qm", "base", "--date", "2024-01-01T00:00:00Z")
        self.commit = subprocess.run(
            ["git", "-C", str(self.repo), "rev-parse", "HEAD"], capture_output=True, text=True, check=True
        ).stdout.strip()

    def tearDown(self):
        self.tmp.cleanup()

    def args(self, **overrides):
        values = dict(
            repo="o/x", source=self.repo, commit=self.commit, language="Go",
            out_dir=Path(self.tmp.name) / "out", workflow_root=Path(self.tmp.name) / "none",
            min_source_files=3, min_test_files=1, min_age_days=0, max_idle_days=99999,
            max_project_tasks=10, used_projects=Path(self.tmp.name) / "used.json", waive_external=[], run_container=False,
            image=None, prepare_cmd=None, test_cmd=None, go_tags=None, timeout=60,
        )
        values.update(overrides)
        return Namespace(**values)

    def runner(self, issues=10):
        meta = {
            "data": {"repository": {
                "nameWithOwner": "o/x", "url": "https://github.com/o/x", "isArchived": False, "isFork": False,
                "stargazerCount": 2000, "createdAt": "2020-01-01T00:00:00Z", "pushedAt": "2026-10-01T00:00:00Z",
                "primaryLanguage": {"name": "Go"}, "licenseInfo": {"spdxId": "MIT"},
                "issues": {"totalCount": issues}, "openIssues": {"totalCount": 1},
            }}
        }

        def run(command, **kwargs):
            if command[:2] == ["gh", "api"]:
                return SimpleNamespace(returncode=0, stdout=json.dumps(meta), stderr="")
            return subprocess.run(command, **kwargs)

        return run

    def test_without_container_is_incomplete_and_reports_ci_commands(self):
        report = screen.screen(self.args(), runner=self.runner(), now=datetime(2026, 10, 26, tzinfo=timezone.utc))
        self.assertEqual(report["verdict"], "INCOMPLETE")
        self.assertEqual(report["gates"]["container"]["status"], "pending")
        self.assertEqual(report["preferences"]["ci_test_commands"], ["go generate ./...", "go test ./... --tags=test"])
        self.assertNotIn("baseline_health", report)
        self.assertTrue(screen.has_go_generate(self.repo, self.commit, subprocess.run))

    def test_issue_count_is_informational(self):
        report = screen.screen(self.args(), runner=self.runner(issues=900))
        self.assertEqual(report["gates"]["issues"]["status"], "pass")
        self.assertEqual(report["gates"]["issues"]["total"], 900)

    def test_used_project_fails_screening(self):
        report = screen.screen(self.args(), runner=self.runner())
        self.assertEqual(report["gates"]["used"]["status"], "pass")
        (Path(self.tmp.name) / "used.json").write_text(json.dumps({"schema_version": 1, "projects": [
            {"key": "o/x", "repo": "o/x", "source": "manual", "reason": "做过", "tasks": ["1-x"]}]}))
        report = screen.screen(self.args(), runner=self.runner())
        self.assertEqual(report["gates"]["used"]["status"], "fail")
        self.assertIn("1-x", report["gates"]["used"]["detail"])
        self.assertEqual(report["verdict"], "FAIL")

    def test_external_runtime_below_threshold_passes(self):
        report = screen.screen(self.args(), runner=self.runner())
        self.assertEqual(report["gates"]["external_deps"]["hits"], {"ffmpeg": 1})
        self.assertEqual(report["gates"]["external_deps"]["status"], "pass")


if __name__ == "__main__":
    unittest.main()
