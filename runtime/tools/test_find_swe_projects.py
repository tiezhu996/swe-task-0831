import json
from pathlib import Path
import subprocess
import tempfile
import unittest

from find_swe_projects import exclude_used, find_projects, save_candidates, validate_options


def response_for(nodes, returncode=0):
    body = {"data": {"search": {"repositoryCount": len(nodes), "pageInfo": {"hasNextPage": False}, "nodes": nodes}}}
    return subprocess.CompletedProcess([], returncode, json.dumps(body), "")


def node(name, stars, issues, sha, archived=False, fork=False):
    owner, repo = name.split("/")
    return {
        "nameWithOwner": name, "url": "https://github.com/" + name,
        "description": "test", "stargazerCount": stars, "isArchived": archived, "isFork": fork,
        "primaryLanguage": {"name": "Go"}, "issueTotal": {"totalCount": issues},
        "pushedAt": "2026-10-05T00:00:00Z", "licenseInfo": {"spdxId": "MIT", "name": "MIT", "url": None},
        "defaultBranchRef": {"name": "main", "target": {"oid": sha, "committedDate": "2026-10-05T00:00:00Z"}},
    }


class FindProjectsTests(unittest.TestCase):
    def test_query_is_bounded_and_sorted_by_real_issue_count(self):
        good_sha = "a" * 40
        nodes = [node("acme/high-issues", 2000, 12, good_sha), node("acme/low-issues", 1800, 2, good_sha)]
        calls = []

        def runner(command, **kwargs):
            calls.append((command, kwargs))
            return response_for(nodes)

        result = find_projects(limit=2, runner=runner)
        self.assertEqual([p["repo"] for p in result["projects"]], ["acme/low-issues", "acme/high-issues"])
        self.assertEqual(result["projects"][0]["issue_total_count"], 2)
        self.assertEqual(result["network_writes"], 0)
        self.assertIn("OPEN+CLOSED", result["issue_count_semantics"])
        self.assertNotIn("--limit", " ".join(calls[0][0]))
        self.assertIn("first=2", calls[0][0])
        self.assertEqual(calls[0][1]["timeout"], 60)

    def test_rejects_unbounded_or_bad_candidates(self):
        with self.assertRaises(ValueError):
            validate_options("Go", 51, 1500, 3000)
        with self.assertRaises(ValueError):
            find_projects(limit=1, runner=lambda *args, **kwargs: response_for([node("acme/bad", 2000, 1, "bad")]))
        with self.assertRaises(ValueError):
            find_projects(limit=1, runner=lambda *args, **kwargs: response_for([node("acme/archived", 2000, 1, "a" * 40, archived=True)]))

    def test_used_projects_are_dropped_and_listed(self):
        result = {"projects": [{"repo": "acme/old"}, {"repo": "acme/new"}]}
        table = type("T", (), {"TABLE": None, "lookup": staticmethod(
            lambda repo, table, roots: {"matched_by": "table", "tasks": ["1-old"]} if repo == "acme/old" else None)})
        filtered = exclude_used(result, table)
        self.assertEqual([p["repo"] for p in filtered["projects"]], ["acme/new"])
        self.assertEqual(filtered["excluded_used"], [{"repo": "acme/old", "matched_by": "table", "tasks": ["1-old"]}])

    def test_save_is_private_and_never_overwrites(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "projects.json"
            result = {"schema_version": 1, "projects": []}
            save_candidates(path, result)
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                save_candidates(path, result)
            self.assertEqual(json.loads(path.read_text())["schema_version"], 1)

    def test_command_failure_does_not_create_output(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "projects.json"
            with self.assertRaises(ValueError):
                find_projects(limit=1, runner=lambda *args, **kwargs: response_for([], 1))
            self.assertFalse(path.exists())


if __name__ == "__main__":
    unittest.main()
