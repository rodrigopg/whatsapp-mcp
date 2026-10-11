import unittest
from unittest.mock import patch

import whatsapp


class TestListActiveChats(unittest.TestCase):
    def test_sends_window_and_returns_rows_as_is(self):
        rows = [{"jid": "1@s.whatsapp.net", "name": "Alice", "link": "https://wa.me/1"}]
        with patch("whatsapp._api_post", return_value={"chats": rows}) as m:
            out = whatsapp.list_active_chats("2026-05-21T00:00:00-03:00", "2026-05-21T23:59:59-03:00", True)
        self.assertEqual(out, rows)
        self.assertEqual(m.call_args.args, ("/chats/active", {
            "after": "2026-05-21T00:00:00-03:00", "before": "2026-05-21T23:59:59-03:00", "include_groups": True}))

    def test_bridge_failure_is_an_empty_list(self):
        with patch("whatsapp._api_post", return_value=None):
            self.assertEqual(whatsapp.list_active_chats(), [])


if __name__ == "__main__":
    unittest.main()
