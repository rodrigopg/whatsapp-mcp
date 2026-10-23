import unittest
from unittest.mock import patch

import whatsapp


class TestUnknownHistorySender(unittest.TestCase):
    def test_unknown_sender_is_explicit_without_contact_lookup(self):
        for sender in ("", None):
            with self.subTest(sender=sender), patch("whatsapp._api_post") as api:
                message = whatsapp._message_from_dict({
                    "sender": sender, "content": "preserved history", "id": "history",
                })
                self.assertIn("From: Unknown sender: preserved history", whatsapp.format_message(message))
                api.assert_not_called()

    def test_own_message_still_displays_me(self):
        message = whatsapp._message_from_dict({"sender": "", "is_from_me": True, "content": "own"})
        with patch("whatsapp._api_post") as api:
            self.assertIn("From: Me: own", whatsapp.format_message(message))
            api.assert_not_called()
