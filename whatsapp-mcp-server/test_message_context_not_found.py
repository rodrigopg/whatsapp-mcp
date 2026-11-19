import unittest
from unittest.mock import Mock, patch

import whatsapp


class MessageContextNotFound(unittest.TestCase):
    def test_bridge_400_is_not_found(self):
        with patch("whatsapp.requests.post", return_value=Mock(status_code=400, text="Message with ID x not found")):
            with self.assertRaisesRegex(ValueError, "not found"):
                whatsapp.get_message_context("x")

    def test_bridge_500_stays_bridge_error(self):
        with patch("whatsapp.requests.post", return_value=Mock(status_code=500, text="boom")):
            with self.assertRaises(whatsapp.BridgeError):
                whatsapp.get_message_context("x")


if __name__ == "__main__":
    unittest.main()
