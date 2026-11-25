import unittest
from unittest.mock import MagicMock, patch

import whatsapp


def resp(body):
    r = MagicMock(status_code=200)
    r.json.return_value = body
    return r


class TestVotePoll(unittest.TestCase):
    def test_posts_the_options_and_reports_success(self):
        with patch("whatsapp._api_request", return_value=resp({"success": True})) as m:
            self.assertEqual(whatsapp.vote_poll("1@s.whatsapp.net", "P1", ["pizza"]), (True, "Vote sent"))
        self.assertEqual(m.call_args.args, ("POST", "/poll_vote"))
        self.assertEqual(m.call_args.kwargs["json"], {"chat_jid": "1@s.whatsapp.net", "message_id": "P1", "options": ["pizza"]})

    def test_bridge_refusal_is_safe_and_blanks_are_refused_locally(self):
        with patch("whatsapp._api_request", return_value=resp({"error": '"tacos" is not an option of this poll'})):
            self.assertIn("operation failed", whatsapp.vote_poll("1@s.whatsapp.net", "P1", ["tacos"])[1])
        with patch("whatsapp._api_request") as m:
            self.assertFalse(whatsapp.vote_poll(" ", "P1", [])[0])
            self.assertFalse(whatsapp.vote_poll("1@s.whatsapp.net", "", [])[0])
            m.assert_not_called()


if __name__ == "__main__":
    unittest.main()
