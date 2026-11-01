import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

SCRIPTS = Path(__file__).parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))
import batch_pool


def git(cwd, *args):
    return subprocess.run(["git", "-C", str(cwd), *args], check=True, capture_output=True, text=True).stdout.strip()


class BatchPoolTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.base = Path(self.tmp.name)
        self.root = self.base / "workflow"
        self.root.mkdir()

    def tearDown(self):
        self.tmp.cleanup()

    def add_task(self, name, status, workspace=None, checkpoint=None):
        source = workspace or self.base / "sources" / name
        if not source.exists():
            source.mkdir(parents=True)
            git(source, "init", "-q")
            git(source, "config", "user.email", "t@example.com")
            git(source, "config", "user.name", "t")
            (source / "app.py").write_text("x = 1\n")
            (source / ".gitignore").write_text("build/\n")
            git(source, "add", "-A")
            git(source, "commit", "-qm", "base")
        task = self.root / name
        task.mkdir()
        state = {
            "task_id": name,
            "status": status,
            "workspace": str(source),
            "project": {"baseline_commit": git(source, "rev-parse", "HEAD")},
        }
        (task / "task-state.json").write_text(json.dumps(state))
        if checkpoint is not None:
            (task / "monitor-checkpoint.json").write_text(json.dumps(checkpoint))
        return task, source

    def pool(self, max_parallel=2, tasks=None):
        args = SimpleNamespace(root=self.root, task=tasks, max_parallel=max_parallel, pool=self.root / ".batches" / "p.json")
        batch_pool.cmd_init(args)
        return args.pool

    def tick(self, pool, no_freeze=False):
        return batch_pool.cmd_tick(SimpleNamespace(pool=pool, no_freeze=no_freeze))

    def test_init_queues_only_precheck_passed_tasks(self):
        self.add_task("1-a", "precheck_passed")
        self.add_task("2-b", "qc_accepted")
        self.add_task("3-c", "precheck_passed")
        data = json.loads(self.pool().read_text())
        self.assertEqual(data["tasks"], ["1-a", "3-c"])

    def test_free_slots_start_next_queued_tasks_in_order(self):
        self.add_task("1-a", "trae_running", checkpoint={"binding": {"session": "s1"}, "logs": {"/l1": {}}, "last_check": 0})
        for name in ("2-b", "3-c", "4-d"):
            self.add_task(name, "precheck_passed")
        result = self.tick(self.pool(max_parallel=3, tasks=["1-a", "2-b", "3-c", "4-d"]))
        starts = [a["task"] for a in result["actions"] if a["action"] == "start_trae"]
        self.assertEqual(result["running"], 1)
        self.assertEqual(starts, ["2-b", "3-c"])
        monitors = [a for a in result["actions"] if a["action"] == "run_monitor"]
        self.assertEqual(monitors[0]["session_id"], "s1")

    def test_pool_full_starts_nothing(self):
        self.add_task("1-a", "trae_running")
        self.add_task("2-b", "trae_running")
        self.add_task("3-c", "precheck_passed")
        result = self.tick(self.pool(max_parallel=2, tasks=["1-a", "2-b", "3-c"]))
        self.assertEqual(result["free_slots"], 0)
        self.assertFalse([a for a in result["actions"] if a["action"] == "start_trae"])

    def test_finished_run_saves_changed_code_including_new_files(self):
        task, source = self.add_task("1-a", "trae_running", checkpoint={"terminal_event": "completed"})
        (source / "app.py").write_text("x = 2\n")
        (source / "new_module.py").write_text("y = 3\n")
        (source / "build").mkdir()
        (source / "build" / "out.bin").write_text("ignored")
        index_before = (source / ".git" / "index").read_bytes()
        result = self.tick(self.pool(tasks=["1-a"]))
        self.assertEqual(result["frozen_now"][0]["action"], "frozen")
        patch = (task / "verification" / "frozen-source.patch").read_text()
        self.assertIn("new_module.py", patch)
        self.assertIn("+x = 2", patch)
        self.assertNotIn("out.bin", patch)
        hashes = (task / "verification" / "frozen-source-files.sha256").read_text()
        self.assertIn("app.py", hashes)
        self.assertIn("new_module.py", hashes)
        # The model workspace itself is untouched.
        self.assertEqual((source / ".git" / "index").read_bytes(), index_before)
        self.assertIn("?? new_module.py", git(source, "status", "--porcelain"))
        follow = [a for a in result["actions"] if a["action"] == "confirm_completion_then_collect_verify_submit"]
        self.assertEqual(len(follow), 1)
        # The patch reproduces the run on a clean baseline checkout.
        clone = self.base / "replay"
        git(self.base, "clone", "-q", str(source), str(clone))
        subprocess.run(["git", "-C", str(clone), "apply", str(task / "verification" / "frozen-source.patch")], check=True)
        self.assertEqual((clone / "new_module.py").read_text(), "y = 3\n")

    def test_existing_different_snapshot_is_not_overwritten(self):
        task, source = self.add_task("1-a", "trae_running", checkpoint={"terminal_event": "completed"})
        (source / "app.py").write_text("x = 2\n")
        batch_pool.freeze(task)
        (source / "app.py").write_text("x = 9\n")
        with self.assertRaisesRegex(batch_pool.PoolError, "不覆盖"):
            batch_pool.freeze(task)
        (source / "app.py").write_text("x = 2\n")
        self.assertEqual(batch_pool.freeze(task)["action"], "already_frozen")

    def test_shared_or_dirty_sources_block_start(self):
        _, shared = self.add_task("1-a", "trae_running")
        self.add_task("2-b", "precheck_passed", workspace=shared)
        _, dirty = self.add_task("3-c", "precheck_passed")
        (dirty / "stray.txt").write_text("old answer")
        result = self.tick(self.pool(max_parallel=3, tasks=["1-a", "2-b", "3-c"]))
        joined = "\n".join(result["isolation_problems"])
        self.assertIn("共用或嵌套同一个源码目录", joined)
        self.assertIn("3-c 尚未开跑但源码目录不干净", joined)
        self.assertFalse([a for a in result["actions"] if a["action"] == "start_trae"])
        self.assertEqual(result["blocked_tasks"], ["1-a", "2-b", "3-c"])

    def test_abnormal_stop_is_frozen_and_reported_not_resumed(self):
        _, source = self.add_task("1-a", "trae_running", checkpoint={"terminal_event": "error"})
        (source / "app.py").write_text("x = 5\n")
        result = self.tick(self.pool(tasks=["1-a"]))
        self.assertEqual(result["frozen_now"][0]["action"], "frozen")
        self.assertEqual([a["action"] for a in result["actions"]], ["report_abnormal_stop"])
        self.assertEqual(result["free_slots"], 2)

    def test_parallel_limit_is_bounded(self):
        self.add_task("1-a", "precheck_passed")
        with self.assertRaises(batch_pool.PoolError):
            self.pool(max_parallel=4)


if __name__ == "__main__":
    unittest.main()
