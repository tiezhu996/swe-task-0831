import subprocess
import sys
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))
from check_prompt_rubric_scope import prediction_outcome
from check_prompt_style import check
from trajectory_metrics import archive, metrics, read_events


class PromptGateSelfTests(unittest.TestCase):
    def run_self_test(self, script):
        result = subprocess.run(
            [sys.executable, str(SCRIPTS / script), "--self-test"],
            capture_output=True,
            text=True,
        )
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertNotIn("FAIL", result.stdout)

    def test_style_self_test(self):
        self.run_self_test("check_prompt_style.py")

    def test_scope_and_depth_self_test(self):
        self.run_self_test("check_prompt_rubric_scope.py")


class PromptStyleTests(unittest.TestCase):
    def test_product_nouns_are_allowed(self):
        errors, _ = check(
            "团队共用这台加速服务，拉镜像、下 GitHub 文件和打离线包都从同一份额度里扣，"
            "Redis 和 SQLite 后端读到的已用量要一样。"
        )
        self.assertEqual(errors, [])

    def test_template_tail_is_rejected(self):
        errors, _ = check("黑白名单改了之后，希望能直接生效。\n\n改设置的过程中，正在进行的请求不受影响。")
        joined = "\n".join(errors)
        self.assertIn("不受影响", joined)
        self.assertIn("末段只剩", joined)


def plan_line(index, tool, session="s1"):
    return (
        f"2026-10-26T10:00:{index:02d}.000+08:00 [info] [ai-chat/v2] [PlanItemHandler] New plan item created "
        f'{{"sessionId":"{session}","planItemId":"p{index}","toolCallName":"{tool}"}}\n'
    )


class TrajectoryMetricsTests(unittest.TestCase):
    def write_log(self, root, tools, session="s1"):
        log = root / "renderer.log"
        lines = [plan_line(i, tool, session) for i, tool in enumerate(tools)]
        log.write_text("".join(lines + lines[:2]), encoding="utf-8")  # duplicated lines are de-duplicated
        return log

    def test_counts_and_run_fix_loops(self):
        import tempfile

        tools = ["", "Read", "", "Edit", "Shell", "", "Edit", "Shell", "Edit", "Grep"]
        with tempfile.TemporaryDirectory() as tmp:
            log = self.write_log(Path(tmp), tools)
            result = metrics(read_events([log])["s1"])
        self.assertEqual(result["effective_round_count"], 10)
        self.assertEqual(result["reasoning_rounds"], 3)
        self.assertEqual(result["tool_calls"], 7)
        self.assertEqual(result["sequence"], "RESESEG")
        self.assertEqual(result["edit_shell_edit_loops"], 2)
        self.assertEqual(result["exploration_calls_before_first_edit"], 1)

    def test_archive_is_private_and_not_overwritten(self):
        import os
        import tempfile

        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            log = self.write_log(root, ["Read", "Edit"])
            other = root / "other.log"
            other.write_text(plan_line(9, "Read", "s2"), encoding="utf-8")
            result = metrics(read_events([log])["s1"])
            receipt = archive([log, other], "s1", result, root / "archive")
            archived = Path(receipt["archived_log"]["path"])
            self.assertEqual(oct(os.stat(archived).st_mode & 0o777), "0o600")
            self.assertNotIn('"s2"', archived.read_text(encoding="utf-8"))
            archive([log], "s1", result, root / "archive")  # identical rerun is fine
            log.write_text(plan_line(1, "Shell"), encoding="utf-8")
            with self.assertRaises(FileExistsError):
                archive([log], "s1", metrics(read_events([log])["s1"]), root / "archive")


class PredictionOutcomeTests(unittest.TestCase):
    def test_actual_rounds_compared_with_registered_range(self):
        contract = {
            "depth": {"predicted_rounds": {"low": 110, "high": 180}},
            "submission": {"phase": "final", "effective_rounds": 95},
        }
        self.assertEqual(prediction_outcome(contract), {"predicted": [110, 180], "actual": 95, "within_range": False})
        self.assertIsNone(prediction_outcome({"submission": {"effective_rounds": 95}}))


if __name__ == "__main__":
    unittest.main()
