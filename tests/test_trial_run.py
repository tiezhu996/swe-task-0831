import hashlib
import json
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

SCRIPTS = Path(__file__).parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))
import trial_run


def assistant(*blocks):
    return {"type": "assistant", "message": {"content": list(blocks)}}


def tool(name):
    return {"type": "tool_use", "name": name}


class StreamMetricsTests(unittest.TestCase):
    def test_counts_reasoning_tools_and_run_fix_loops(self):
        events = [
            assistant({"type": "thinking", "thinking": "x"}),
            assistant(tool("Read")), assistant(tool("Grep")),
            assistant(tool("Edit")), assistant(tool("Bash")), assistant(tool("Edit")),
            assistant({"type": "text", "text": "完成"}),
            {"type": "result", "num_turns": 7, "usage": {"input_tokens": 5, "output_tokens": 2}},
        ]
        m = trial_run.stream_metrics(events)
        self.assertEqual(m["tool_calls"], 5)
        self.assertEqual(m["reasoning_steps"], 2)
        self.assertEqual(m["assistant_steps"], 7)
        self.assertEqual(m["edit_shell_edit_loops"], 1)
        self.assertEqual(m["sequence"], "RGESE")
        self.assertEqual(m["usage"]["input_tokens"], 5)


class VerdictTests(unittest.TestCase):
    calibration = {"metric": "tool_calls", "slope": 2.0, "intercept": 20.0, "loo_mae": 15}

    def runs(self, *values, stop="finished"):
        return [{"tool_calls": v, "stop_reason": stop} for v in values]

    def test_window_bands(self):
        for values, expected in (((30, 34), "too_small"), ((50, 54), "in_window"), ((95, 100), "too_large")):
            runs = self.runs(*values)
            status, _ = trial_run.verdict(trial_run.predict(self.calibration, runs), runs, (110, 200))
            self.assertEqual(status, expected, values)

    def test_circuit_breaker_is_too_large(self):
        runs = self.runs(40) + self.runs(151, stop="max_tool_calls")
        status, _ = trial_run.verdict(None, runs, (110, 200))
        self.assertEqual(status, "too_large")

    def test_sprawling_edits_are_too_large(self):
        runs = [{"tool_calls": 50, "stop_reason": "finished", "changed_source_files": n} for n in (13, 16)]
        status, reasons = trial_run.verdict(trial_run.predict(self.calibration, runs), runs, (110, 200))
        self.assertEqual(status, "too_large")
        self.assertIn("源码文件中位数", reasons[0])
        runs[1]["changed_source_files"] = 14.9 - 2
        status, _ = trial_run.verdict(trial_run.predict(self.calibration, runs), runs, (110, 200))
        self.assertEqual(status, "in_window")

    def test_uncalibrated(self):
        status, _ = trial_run.verdict(None, self.runs(40, 41), (110, 200))
        self.assertEqual(status, "uncalibrated")


class GuardTests(unittest.TestCase):
    def test_refuses_trial_dir_inside_sources_or_workflow(self):
        for sub in ("sources", "workflow"):
            with self.assertRaises(SystemExit):
                trial_run.guard_out_dir(trial_run.SWE_ROOT / sub / "1-x" / "trial")
        trial_run.guard_out_dir(trial_run.SWE_ROOT / "trials" / "1-x")


class CheckTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.prompt = self.root / "PROMPT.md"
        self.prompt.write_text("题面", encoding="utf-8")
        self.sha = hashlib.sha256(self.prompt.read_bytes()).hexdigest()

    def tearDown(self):
        self.tmp.cleanup()

    def write(self, verdict="in_window", sha=None, confirm_errors=(), mode="hard", band=(120, 160), stop="finished",
              files=(3, 4)):
        runs = [{"stop_reason": "finished", "changed_source_files": files[0]},
                {"stop_reason": stop, "changed_source_files": files[1]}]
        (self.root / "trial-run.json").write_text(json.dumps(
            {"prompt_sha256": sha or self.sha, "runs": runs, "verdict": verdict, "reasons": [],
             "gate_mode": mode, "window": [110, 200], "prediction": {"band": list(band)}}))
        (self.root / "trial-confirm.json").write_text(json.dumps(
            {"prompt_sha256": sha or self.sha, "errors": list(confirm_errors)}))

    def check(self):
        return trial_run.cmd_check(SimpleNamespace(trial_dir=self.root, prompt=self.prompt))

    def test_pass(self):
        self.write()
        self.assertEqual(self.check(), 0)

    def test_missing_files_fail(self):
        self.assertEqual(self.check(), 1)

    def test_out_of_window_fails(self):
        self.write(verdict="too_large")
        self.assertEqual(self.check(), 1)

    def test_advisory_only_blocks_clearly_out_band(self):
        self.write(verdict="too_small", mode="advisory", band=(85, 125))
        self.assertEqual(self.check(), 0)
        self.write(verdict="too_small", mode="advisory", band=(60, 95))
        self.assertEqual(self.check(), 1)
        self.write(verdict="too_large", mode="advisory", band=(205, 260))
        self.assertEqual(self.check(), 1)

    def test_breaker_only_still_blocks_breaker(self):
        self.write(verdict="too_small", mode="breaker_only", band=(40, 70))
        self.assertEqual(self.check(), 0)
        self.write(verdict="too_large", mode="breaker_only", stop="max_tool_calls")
        self.assertEqual(self.check(), 1)
        self.write(verdict="too_large", mode="breaker_only", files=(15, 16))
        self.assertEqual(self.check(), 1)

    def test_gate_mode_thresholds(self):
        self.assertEqual(trial_run.gate_mode(0.85), "hard")
        self.assertEqual(trial_run.gate_mode(0.7), "advisory")
        self.assertEqual(trial_run.gate_mode(0.5), "breaker_only")
        self.assertEqual(trial_run.gate_mode(None), "breaker_only")

    def test_single_decisive_run_is_enough(self):
        self.write(files=(3, 3))
        data = json.loads((self.root / "trial-run.json").read_text())
        data["runs"] = data["runs"][:1]
        (self.root / "trial-run.json").write_text(json.dumps(data))
        self.assertEqual(self.check(), 0)
        data["runs"][0]["changed_source_files"] = 12
        (self.root / "trial-run.json").write_text(json.dumps(data))
        self.assertEqual(self.check(), 1)

    def test_trap_warnings_do_not_block(self):
        self.write()
        data = json.loads((self.root / "trial-run.json").read_text())
        data["warnings"] = ["没有一个停止陷阱让试跑返工"]
        (self.root / "trial-run.json").write_text(json.dumps(data))
        self.assertEqual(self.check(), 0)

    def test_stale_prompt_fails(self):
        self.write(sha="0" * 64)
        self.assertEqual(self.check(), 1)

    def test_confirm_drift_fails(self):
        self.write(confirm_errors=["模型打算新增new_pages"])
        self.assertEqual(self.check(), 1)


class RecoverTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.root = Path(self.tmp.name)
        self.run_dir = self.root / "trial" / "run-2"
        self.run_dir.mkdir(parents=True)
        (self.run_dir / "sandbox.sb").write_text("")
        self.ws = self.root / "scratch" / "ws"
        self.ws.mkdir(parents=True)

    def tearDown(self):
        self.tmp.cleanup()

    def write_events(self, cwd, finished=True):
        events = [{"type": "system", "subtype": "init", "cwd": str(cwd)}, assistant(tool("Bash"))]
        if finished:
            events.append({"type": "result", "duration_ms": 600000, "num_turns": 2})
        (self.run_dir / "events.private.jsonl").write_text("\n".join(json.dumps(e) for e in events) + "\n")

    def test_rebuilds_from_events_and_workspace(self):
        self.write_events(self.ws)
        captured = {}
        original = trial_run.finish_run
        trial_run.finish_run = lambda *a: captured.setdefault("args", a)
        try:
            trial_run.recover_run(self.run_dir, SimpleNamespace(max_tool_calls=400), set())
        finally:
            trial_run.finish_run = original
        index, _, _, stop_reason, scratch, _, minutes = captured["args"][:7]
        self.assertEqual((index, stop_reason, scratch, minutes), (2, "finished", self.ws.parent, 10.0))

    def test_backend_error_is_not_finished(self):
        events = [assistant({"type": "text", "text": "API Error: 402 Insufficient Balance"}),
                  {"type": "result", "is_error": True}]
        self.assertEqual(trial_run.final_stop_reason(events, "finished", "exit_0"), "api_error")
        self.assertEqual(trial_run.final_stop_reason(events[:1], "finished", "exit_0"), "exit_0")
        self.assertEqual(trial_run.final_stop_reason(events, "timeout", "exit_0"), "timeout")

    def test_missing_workspace_refuses(self):
        self.write_events(self.root / "gone" / "ws")
        with self.assertRaises(SystemExit):
            trial_run.recover_run(self.run_dir, SimpleNamespace(max_tool_calls=400), set())


class AdaptiveTests(unittest.TestCase):
    def test_single_run_decisive_band(self):
        run = lambda files, stop="finished": {"stop_reason": stop, "changed_source_files": files}
        self.assertTrue(trial_run.single_run_decisive(run(10)))
        self.assertTrue(trial_run.single_run_decisive(run(18)))
        self.assertFalse(trial_run.single_run_decisive(run(11)))
        self.assertFalse(trial_run.single_run_decisive(run(17)))
        self.assertFalse(trial_run.single_run_decisive(run(3, "timeout")))
        self.assertTrue(trial_run.single_run_decisive(run(3, "max_tool_calls")))


class TrapJudgeTests(unittest.TestCase):
    traps = [{"looks_done": "只改了主入口", "exposed_by": "从列表入口读回"},
             {"looks_done": "旧客户端解析失败", "exposed_by": "用基线客户端调用"}]

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.run_dir = Path(self.tmp.name) / "run-1"
        self.run_dir.mkdir()
        events = [
            assistant({"type": "tool_use", "id": "t1", "name": "Edit",
                       "input": {"file_path": "pkg/read.go", "old_string": "a", "new_string": "b"}}),
            assistant({"type": "tool_use", "id": "t2", "name": "Bash", "input": {"command": "go test ./..."}}),
            {"type": "user", "message": {"content": [
                {"type": "tool_result", "tool_use_id": "t2", "is_error": True, "content": "FAIL list_test.go"}]}},
        ]
        (self.run_dir / "events.private.jsonl").write_text("\n".join(json.dumps(e) for e in events))

    def tearDown(self):
        self.tmp.cleanup()

    def test_loose_parse_of_malformed_json(self):
        reply = '{"traps":[{"index":1,"status":"missed","evidence":"写了 "x" 没转义"},{"index": 2, "status": "fixed_upfront"}]}'
        self.assertIsNone(trial_run.parse_json_object(reply))
        found = trial_run.loose_trap_statuses(reply)
        self.assertEqual([(t["index"], t["status"]) for t in found], [(1, "missed"), (2, "fixed_upfront")])

    def test_judge_without_json_is_not_cached(self):
        original = trial_run.call_backend
        trial_run.call_backend = lambda *a, **k: ""
        try:
            self.assertIsNone(trial_run.judge_traps(self.run_dir, "题面", self.traps,
                                                    SimpleNamespace(env_file=Path("x"), model="m")))
        finally:
            trial_run.call_backend = original
        self.assertFalse((self.run_dir / "trap-judgement.json").exists())
        self.assertTrue((self.run_dir / "trap-judgement.raw.txt").exists())

    def test_compact_trajectory_keeps_edits_and_failures(self):
        text = trial_run.compact_trajectory(trial_run.read_events(self.run_dir / "events.private.jsonl"))
        self.assertIn("[1] Edit: pkg/read.go", text)
        self.assertIn("[2] Bash: go test ./...", text)
        self.assertIn("-> 失败: FAIL list_test.go", text)

    def test_judge_normalises_and_caches(self):
        calls = []
        reply = '{"traps": [{"index": 1, "status": "hit_then_fixed", "evidence": "步骤 2 失败后返工"},' \
                ' {"index": 2, "status": "bogus"}]}'
        original = trial_run.call_backend
        trial_run.call_backend = lambda *a, **k: calls.append(a) or reply
        args = SimpleNamespace(env_file=Path("x"), model="m")
        try:
            first = trial_run.judge_traps(self.run_dir, "题面", self.traps, args)
            second = trial_run.judge_traps(self.run_dir, "题面", self.traps, args)
        finally:
            trial_run.call_backend = original
        self.assertEqual([t["status"] for t in first["traps"]], ["hit_then_fixed", "unclear"])
        self.assertEqual(second, first)
        self.assertEqual(len(calls), 1)

    def test_summary_warnings(self):
        judged = lambda *statuses: {"traps": [{"status": s} for s in statuses]}
        shallow = trial_run.trap_summary([judged("fixed_upfront", "missed"), judged("fixed_upfront", "missed")],
                                         self.traps)
        self.assertEqual(len(shallow["warnings"]), 2)
        unclear = trial_run.trap_summary([judged("unclear", "unclear")], self.traps)
        self.assertIn("看不出结论", unclear["warnings"][0])
        good = trial_run.trap_summary([judged("hit_then_fixed", "fixed_upfront")], self.traps)
        self.assertEqual((good["warnings"], good["hit_then_fixed"]), ([], 1))


class CalibrateFitTests(unittest.TestCase):
    def test_fit_recovers_line(self):
        slope, intercept = trial_run.fit([(10, 40), (20, 60), (30, 80)])
        self.assertAlmostEqual(slope, 2.0)
        self.assertAlmostEqual(intercept, 20.0)


if __name__ == "__main__":
    unittest.main()
