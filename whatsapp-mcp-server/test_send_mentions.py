import unittest
from unittest.mock import MagicMock, patch

import main
import whatsapp


def resp(body):
    r = MagicMock(status_code=200)
    r.json.return_value = body
    return r


class TestSendMentions(unittest.TestCase):
    def test_mentions_reach_the_bridge_only_when_given(self):
        with patch("whatsapp.requests.post", return_value=resp({"success": True, "message": "sent"})) as m:
            self.assertEqual(whatsapp.send_message("1@g.us", "@123 hi", mentions=["123@lid"]), (True, "sent"))
            self.assertEqual(m.call_args.kwargs["json"], {"recipient": "1@g.us", "message": "@123 hi", "mentions": ["123@lid"]})
            whatsapp.send_message("1@g.us", "plain")
            self.assertEqual(m.call_args.kwargs["json"], {"recipient": "1@g.us", "message": "plain"})

    def test_tool_forwards_the_mentions(self):
        with patch.object(main, "whatsapp_send_message", return_value=(True, "sent")) as m:
            self.assertEqual(main.send_message("1@g.us", "@123 hi", mentions=["123@lid"]), {"success": True, "message": "sent"})
        self.assertEqual(m.call_args.args, ("1@g.us", "@123 hi"))
        self.assertEqual(m.call_args.kwargs["mentions"], ["123@lid"])


if __name__ == "__main__":
    unittest.main()
