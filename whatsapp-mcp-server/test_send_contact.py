import unittest
from unittest.mock import MagicMock, patch

import whatsapp


def resp(body):
    r = MagicMock()
    r.json.return_value = body
    return r


class TestSendContact(unittest.TestCase):
    def test_posts_name_phone_and_recipient(self):
        with patch("whatsapp._api_request", return_value=resp({"success": True, "message_id": "M1"})) as m:
            ok, msg = whatsapp.send_contact("5562911112222", "Joao", "5562999998888")
        self.assertTrue(ok)
        self.assertIn("M1", msg)
        self.assertEqual(m.call_args.args, ("POST", "/send_contact"))
        self.assertEqual(m.call_args.kwargs["json"],
                         {"recipient": "5562911112222", "name": "Joao", "phone_number": "5562999998888"})

    def test_bridge_error_text_is_returned_and_blank_recipient_is_refused(self):
        with patch("whatsapp._api_request", return_value=resp({"error": "invalid phone_number"})):
            self.assertEqual(whatsapp.send_contact("1", "Joao", "abc"), (False, "invalid phone_number"))
        with patch("whatsapp._api_request") as m:
            self.assertFalse(whatsapp.send_contact(" ", "Joao", "5562999998888")[0])
            m.assert_not_called()


if __name__ == "__main__":
    unittest.main()
