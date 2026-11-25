import unittest
from unittest.mock import patch

import whatsapp
import main


class UnreadTest(unittest.TestCase):
    def test_chat_metadata_survives_python_conversion(self):
        data = {"jid": "111@s.whatsapp.net", "unread_count": 3, "marked_as_unread": True}
        with patch.object(whatsapp, "_api_post", return_value={"chat": data}):
            chat = whatsapp.get_chat(data["jid"])
        self.assertEqual(chat.unread_count, 3)
        self.assertTrue(chat.marked_as_unread)

    def test_unread_filter_reaches_bridge(self):
        with patch.object(whatsapp, "_api_post", return_value={"chats": []}) as post:
            whatsapp.list_chats(unread_only=True)
        self.assertTrue(post.call_args.args[1]["unread_only"])

    def test_tool_passes_unread_filter(self):
        with patch.object(main, "whatsapp_list_chats", return_value=[]) as chats:
            main.list_chats(unread_only=True)
        self.assertTrue(chats.call_args.kwargs["unread_only"])

    def test_old_bridge_defaults(self):
        chat = whatsapp._chat_from_dict({"jid": "111@s.whatsapp.net"})
        self.assertEqual(chat.unread_count, 0)
        self.assertFalse(chat.marked_as_unread)
