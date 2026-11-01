import hashlib
import json
import os
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

SCRIPTS = Path(__file__).parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))
import precheck_gate


class PrecheckGateTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.task = root / "workflow" / "9-x"
        self.task.mkdir(parents=True)
        (self.task / "PROMPT.md").write_text("  题面正文\r\n", encoding="utf-8")
        for name in ("rubric.yaml", "design-contract.json", "overlap-ledger.json"):
            (self.task / name).write_text("{}", encoding="utf-8")
        (root / "corpus.json").write_text("[]", encoding="utf-8")
        (root / "features.json").write_text("{}", encoding="utf-8")
        (root / "src").mkdir()
        sibling_dir = root / "workflow" / "8-x"
        sibling_dir.mkdir()
        (sibling_dir / "task-state.json").write_text(json.dumps({"repo_url": "https://github.com/o/x"}))
        (sibling_dir / "PROMPT.md").write_text("另一道题", encoding="utf-8")
        self.args = precheck_gate.build_parser().parse_args([
            "--task-dir", str(self.task), "--repo", "o/x", "--corpus", str(root / "corpus.json"),
            "--features", str(root / "features.json"), "--source", str(root / "src"),
            "--workflow-root", str(root / "workflow"),
        ])
        precheck_gate.resolve_defaults(self.args)

    def tearDown(self):
        self.tmp.cleanup()

    def runner(self, failing=()):
        def run(command, **kwargs):
            name = Path(command[1]).name
            code = 1 if any(part in name for part in failing) else 0
            return SimpleNamespace(returncode=code, stdout=f"{name}\n", stderr="")
        return run

    def test_all_pass_writes_private_receipt_bound_to_canonical_prompt(self):
        receipt = precheck_gate.run(self.args, runner=self.runner())
        self.assertTrue(receipt["passed"])
        self.assertEqual(receipt["prompt_sha256"], hashlib.sha256("题面正文".encode()).hexdigest())
        self.assertEqual([g["name"] for g in receipt["gates"]], ["independence", "feature_overlap", "style", "scope_depth", "trial"])
        path = self.task / "precheck-gate.json"
        self.assertEqual(oct(os.stat(path).st_mode & 0o777), "0o600")
        sibling = str((self.task.parent / "8-x" / "PROMPT.md").resolve())
        self.assertIn(sibling, receipt["inputs"])

    def test_one_failing_gate_fails_receipt(self):
        receipt = precheck_gate.run(self.args, runner=self.runner(failing=("overlap",)))
        self.assertFalse(receipt["passed"])
        self.assertEqual({g["name"]: g["exit_code"] for g in receipt["gates"]}["feature_overlap"], 1)

    def test_input_changed_during_run_is_refused(self):
        def run(command, **kwargs):
            (self.task / "rubric.yaml").write_text("changed", encoding="utf-8")
            return SimpleNamespace(returncode=0, stdout="", stderr="")
        with self.assertRaises(ValueError):
            precheck_gate.run(self.args, runner=run)

    def test_trial_results_are_receipt_inputs(self):
        trial = self.task.parent.parent / "trials" / "9-x"
        trial.mkdir(parents=True)
        (trial / "trial-run.json").write_text("{}", encoding="utf-8")
        receipt = precheck_gate.run(self.args, runner=self.runner())
        self.assertIn(str((trial / "trial-run.json").resolve()), receipt["inputs"])

    def test_commands_target_the_gate_scripts(self):
        names = [Path(cmd[1]).name for _, cmd in precheck_gate.gate_commands(self.args, self.task)]
        self.assertEqual(names, [
            "check_prompt_independence.py", "check_issue_feature_overlap.py",
            "check_prompt_style.py", "check_prompt_rubric_scope.py", "trial_run.py",
        ])
        for _, cmd in precheck_gate.gate_commands(self.args, self.task):
            self.assertTrue((SCRIPTS / Path(cmd[1]).name).is_file())


if __name__ == "__main__":
    unittest.main()
