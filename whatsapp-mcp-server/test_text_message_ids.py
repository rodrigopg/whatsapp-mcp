import unittest
from unittest.mock import patch

import whatsapp


class TextMessageIdsTest(unittest.TestCase):
    def test_text_message_shows_its_id_and_chat(self):
        with patch("whatsapp._api_post", return_value={"messages": [{
            "id": "plain", "chat_jid": "test@g.us", "content": "hello", "is_from_me": True,
        }]}):
            output = whatsapp.list_messages(include_context=False)
        self.assertIn("From: Me: [Message ID: plain - Chat JID: test@g.us] hello", output)

    def test_media_message_keeps_its_prefix(self):
        message = whatsapp._message_from_dict({
            "id": "pic", "chat_jid": "test@g.us", "content": "", "media_type": "image", "is_from_me": True,
        })
        self.assertIn("[image - Message ID: pic - Chat JID: test@g.us]", whatsapp.format_message(message))


if __name__ == "__main__":
    unittest.main()
