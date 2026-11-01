import json
import sys
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

SCRIPTS = Path(__file__).parents[1] / "scripts"
sys.path.insert(0, str(SCRIPTS))
import used_projects


class UsedProjectsTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.base = Path(self.tmp.name)
        self.table = self.base / "data" / "used.json"
        self.root = self.base / "workflow"
        self.root.mkdir()

    def tearDown(self):
        self.tmp.cleanup()

    def task(self, name, url):
        (self.root / name).mkdir()
        (self.root / name / "task-state.json").write_text(json.dumps({"project": {"repo_url": url}}))

    def sync(self):
        return used_projects.cmd_sync(SimpleNamespace(table=self.table, workflow_root=[self.root, self.base / "missing"]))

    def test_normalize_accepts_urls_and_rejects_junk(self):
        for text in ("Owner/Repo", "https://github.com/Owner/Repo.git", "github.com/Owner/Repo/"):
            self.assertEqual(used_projects.normalize(text), "owner/repo")
        with self.assertRaises(used_projects.UsedProjectsError):
            used_projects.normalize("not a repo")

    def test_sync_adds_each_repo_once_and_only_grows(self):
        self.task("1-a", "https://github.com/O/A.git")
        self.task("2-a", "https://github.com/O/A")
        first = self.sync()
        self.assertEqual(first["added"], ["O/A"])
        self.task("3-b", "https://github.com/o/b")
        self.task("4-a", "https://github.com/o/a")
        second = self.sync()
        self.assertEqual(second["added"], ["o/b"])
        self.assertEqual(second["updated"], ["O/A"])
        rows = {row["key"]: row for row in used_projects.load(self.table)["projects"]}
        self.assertEqual(rows["o/a"]["tasks"], ["1-a", "2-a", "4-a"])
        # Deleting the workflow tasks never removes an entry.
        for name in ("1-a", "2-a", "3-b", "4-a"):
            (self.root / name / "task-state.json").unlink()
        self.assertEqual(self.sync()["total"], 2)

    def test_manual_add_and_lookup(self):
        args = SimpleNamespace(table=self.table, repo="https://github.com/X/Y", reason="平台上做过的 test 任务")
        self.assertEqual(used_projects.cmd_add(args)["action"], "added")
        self.assertEqual(used_projects.cmd_add(args)["action"], "already_listed")
        with self.assertRaises(used_projects.UsedProjectsError):
            used_projects.cmd_add(SimpleNamespace(table=self.table, repo="x/z", reason=" "))
        hit = used_projects.lookup("x/y", self.table, [self.root])
        self.assertEqual((hit["matched_by"], hit["source"]), ("table", "manual"))
        # A repo with a task in the live workflow is used even before sync.
        self.task("5-c", "https://github.com/o/c")
        self.assertEqual(used_projects.lookup("O/C", self.table, [self.root])["matched_by"], "workflow")
        self.assertIsNone(used_projects.lookup("o/new", self.table, [self.root]))


if __name__ == "__main__":
    unittest.main()
