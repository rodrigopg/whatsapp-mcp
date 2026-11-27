import unittest
from unittest.mock import MagicMock, patch

import whatsapp


def resp(body):
    r = MagicMock(status_code=200)
    r.json.return_value = body
    return r


class TestMatterCall(unittest.TestCase):
    def test_posts_payload_and_returns_the_bridge_body(self):
        with patch("whatsapp._api_request", return_value=resp({"id": 7, "state": "open"})) as m:
            self.assertEqual(whatsapp.matter_call("/matters/record", {"chat_jid": "1@s.whatsapp.net"}), {"id": 7, "state": "open"})
        self.assertEqual(m.call_args.args, ("POST", "/matters/record"))
        self.assertEqual(m.call_args.kwargs["json"], {"chat_jid": "1@s.whatsapp.net"})

    def test_a_refusal_is_returned_as_a_safe_error(self):
        with patch("whatsapp._api_request", return_value=resp({"error": "message m1 not found in chat 1@s.whatsapp.net"})):
            self.assertIn("operation failed", whatsapp.matter_call("/matters/record", {})["error"])

    def test_connection_failure_is_an_error_not_an_exception(self):
        with patch("whatsapp._api_request", side_effect=whatsapp.BridgeError("Bridge unavailable")):
            self.assertIn("unavailable", whatsapp.matter_call("/matters/list", {})["error"])


if __name__ == "__main__":
    unittest.main()
