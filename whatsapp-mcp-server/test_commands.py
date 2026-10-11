import os
import re
import unittest

CMD_DIR = os.path.join(os.path.dirname(__file__), "..", "commands")
# command -> the tools it must use. Words that look like tool names but are response fields are listed in FIELDS.
COMMANDS = {
    "triage.md": ("review_next_messages", "record_matter", "close_matter", "reopen_matter", "mark_reviewed"),
    "favorites.md": ("list_favorites", "suggest_favorites", "decide_favorite"),
    "morning.md": ("review_next_messages", "mark_reviewed", "morning_summary", "reopen_matter", "create_chase_draft", "list_chase_drafts",
                   "approve_chase_draft", "discard_chase_draft"),
}
FIELDS = {"is_from_me", "next_cursor", "matter_id", "owed_by", "open_matters", "as_of"}


def read(name):
    with open(os.path.join(CMD_DIR, name), encoding="utf-8") as f:
        return f.read()


class TestCommands(unittest.TestCase):
    def test_use_their_tools_and_only_tools_that_exist(self):
        import main  # the real tool names
        known = set(main.mcp._tool_manager._tools)
        for name, tools in COMMANDS.items():
            text = read(name)
            for t in tools:
                self.assertIn(f"`{t}`", text, f"{name} must use {t}")
            named = set(re.findall(r"`([a-z]+(?:_[a-z]+)+)`", text)) - FIELDS
            self.assertTrue(named <= known, f"{name} names unknown tools: {named - known}")

    def test_never_send_and_have_no_external_destination(self):
        for name in COMMANDS:
            text = read(name)
            self.assertNotIn("http", text)
            self.assertNotIn("send_message", text)

    def test_rules_that_must_not_drift(self):
        triage, fav = read("triage.md"), read("favorites.md")
        for rule in ('"confirmed"', "Never close", "uncertain", "mark_reviewed", "never send a message", "@-mention"):
            self.assertIn(rule, triage)
        for rule in ("only propose", "Do not decide for the user"):
            self.assertIn(rule, fav)
        morning = read("morning.md")
        for rule in ("Do not create a schedule yourself", "all: true", "uncertain", "Never approve on your own", "exactly"):
            self.assertIn(rule, morning)
        self.assertIn("/whats:favorites", read("setup.md"))


if __name__ == "__main__":
    unittest.main()
