import json
from pathlib import Path
import tempfile
import unittest

from collect_trae_export import check_prompt_messages, collect, verify_existing


class ExportTests(unittest.TestCase):
    def fixture(self, root):
        token = "synthetic-token"
        plan = root / "plan.json"
        plan.write_text(json.dumps({"execution_prompt": "真实执行题面", "payload": {"trae_session_id": token}}))
        source = root / "app-export.md"
        source.write_text("# 任务\n\n> **Workspace:** /Users/example/repo\n\n---\n\n**User**\n\n真实执行题面\n\n**Assistant**\n\nAgent actual response\n" + token + "\n/Users/example/repo\nme@example.com\nCookie: secret\n")
        return source, plan, root / "public.md"

    def test_preserves_original_and_redacts_uploaded_copy(self):
        with tempfile.TemporaryDirectory() as temporary:
            source, plan, output = self.fixture(Path(temporary))
            original = source.read_bytes()
            receipt = collect(source, plan, output)
            self.assertEqual((output.parent / "trae-export-original.private.md").read_bytes(), original)
            self.assertEqual(receipt["source"], "trae_cn_app_export")
            self.assertTrue(receipt["prompt_exact_match"])
            self.assertEqual(receipt["user_message_count"], 1)
            for secret in ("synthetic-token", "example.com", "/Users/", "Cookie: secret"):
                self.assertNotIn(secret, output.read_text())
            self.assertEqual(output.stat().st_mode & 0o777, 0o600)
            verified = verify_existing(output.parent / "trae-export-original.private.md", plan, output, output.with_suffix(".receipt.json"))
            self.assertTrue(verified["verified"])

    def test_wrong_conversation_never_creates_output(self):
        with tempfile.TemporaryDirectory() as temporary:
            source, plan, output = self.fixture(Path(temporary))
            source.write_text("另一个会话")
            with self.assertRaises(ValueError):
                collect(source, plan, output)
            self.assertFalse(output.exists())

    def test_rejects_wrong_first_prompt_even_if_prompt_appears_later(self):
        text = "**User**\n\n另一道题\n\n**Assistant**\n\n真实执行题面\n"
        with self.assertRaises(ValueError):
            check_prompt_messages(text, "真实执行题面")

    def test_rejects_second_human_message_and_unclear_format(self):
        for text in (
            "**User**\n\n真实执行题面\n\n**Assistant**\n\n回答\n\n**User**\n\n继续",
            "真实执行题面\n回答",
            "**User**\n\n真实执行题面\n\n**Assistant**\n\n",
        ):
            with self.subTest(text=text[:20]), self.assertRaises(ValueError):
                check_prompt_messages(text, "真实执行题面")

    def test_existing_verification_detects_modified_trajectory(self):
        with tempfile.TemporaryDirectory() as temporary:
            source, plan, output = self.fixture(Path(temporary))
            collect(source, plan, output)
            private = output.parent / "trae-export-original.private.md"
            private.write_text(private.read_text() + "\n**User**\n\n继续\n")
            with self.assertRaises(ValueError):
                verify_existing(output.parent / "trae-export-original.private.md", plan, output, output.with_suffix(".receipt.json"))


if __name__ == "__main__":
    unittest.main()
