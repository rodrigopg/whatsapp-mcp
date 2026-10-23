import asyncio
import unittest
from unittest import mock

from test_mcp_modes import load_main


class ToolOutputIsNotValidatedAgainstHints(unittest.TestCase):
    """The tools' return hints are loose (list_messages returns a formatted string, search_contacts dataclasses);
    the SDK must hand the values back as text, not reject them against an output schema built from the hints."""

    def call(self, mod, name, args):
        return asyncio.run(mod.mcp.call_tool(name, args))

    def test_values_that_do_not_match_the_hint_still_reach_the_client(self):
        from whatsapp import Contact

        mod = load_main()
        with mock.patch.object(mod, "whatsapp_list_messages", return_value="[2026-10-22] Alice: hi"), \
             mock.patch.object(mod, "whatsapp_search_contacts", return_value=[Contact("111", "Alice", "111@s.whatsapp.net")]):
            out = self.call(mod, "list_messages", {})
            self.assertIn("Alice: hi", str(out))
            out = self.call(mod, "search_contacts", {"query": "ali"})
            self.assertIn("Alice", str(out))


if __name__ == "__main__":
    unittest.main()
