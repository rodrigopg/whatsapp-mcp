import os
import re
import unittest

CMD = os.path.join(os.path.dirname(__file__), "..", "commands", "triage.md")
TOOLS = ("review_next_messages", "record_matter", "close_matter", "reopen_matter", "mark_reviewed")


class TestTriageCommand(unittest.TestCase):
    def setUp(self):
        with open(CMD, encoding="utf-8") as f:
            self.text = f.read()

    def test_uses_the_matter_tools_and_only_them(self):
        for t in TOOLS:
            self.assertIn(f"`{t}`", self.text)
        import main  # the real tool names
        known = set(main.mcp._tool_manager._tools)
        named = set(re.findall(r"`([a-z]+(?:_[a-z]+)+)`", self.text)) - {"is_from_me", "next_cursor", "matter_id", "owed_by", "open_matters"}
        self.assertTrue(named <= known, f"unknown tools: {named - known}")

    def test_never_sends_and_has_no_external_destination(self):
        self.assertNotIn("http", self.text)
        self.assertNotIn("send_message", self.text)
        self.assertIn("never send a message", self.text)

    def test_rules_that_must_not_drift(self):
        for rule in ('"confirmed"', "Never close", "uncertain", "mark_reviewed"):
            self.assertIn(rule, self.text)


if __name__ == "__main__":
    unittest.main()
