import unittest
from unittest.mock import MagicMock, patch

import requests

import whatsapp


def resp(body):
    r = MagicMock()
    r.json.return_value = body
    return r


class TestMatterCall(unittest.TestCase):
    def test_posts_payload_and_returns_the_bridge_body(self):
        with patch("whatsapp._api_request", return_value=resp({"id": 7, "state": "open"})) as m:
            self.assertEqual(whatsapp.matter_call("/matters/record", {"chat_jid": "1@s.whatsapp.net"}), {"id": 7, "state": "open"})
        self.assertEqual(m.call_args.args, ("POST", "/matters/record"))
        self.assertEqual(m.call_args.kwargs["json"], {"chat_jid": "1@s.whatsapp.net"})

    def test_a_refusal_reaches_the_ai_client_as_text_it_can_act_on(self):
        with patch("whatsapp._api_request", return_value=resp({"error": "message m1 not found in chat 1@s.whatsapp.net"})):
            self.assertIn("not found", whatsapp.matter_call("/matters/record", {})["error"])

    def test_connection_failure_is_an_error_not_an_exception(self):
        with patch("whatsapp._api_request", side_effect=requests.RequestException("down")):
            self.assertIn("down", whatsapp.matter_call("/matters/list", {})["error"])


if __name__ == "__main__":
    unittest.main()
