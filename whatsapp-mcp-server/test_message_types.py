import unittest
from unittest.mock import patch

import whatsapp


class MessageTypesTest(unittest.TestCase):
    def test_readable_types_and_sticker_download_identifier(self):
        for content, media in [("[sticker]", "sticker"), ("[location] 55.75,37.62 Park", ""),
                               ("[live location] 0,0 Walking", ""), ("[contact] Alice +12025550123", "")]:
            with self.subTest(content=content), patch("whatsapp._api_post", return_value={"messages": [{
                "id": "typed", "chat_jid": "test@g.us", "content": content,
                "media_type": media, "is_from_me": True,
            }]}):
                output = whatsapp.list_messages(include_context=False)
                self.assertIn(content, output)
                if media:
                    self.assertIn("sticker - Message ID: typed - Chat JID: test@g.us", output)

    def test_reactions_in_list_and_context(self):
        message = {"id": "target", "chat_jid": "test@g.us", "content": "Original", "is_from_me": True,
                   "reactions": [{"sender": "111", "emoji": "👍", "timestamp": "2026-10-22T10:00:00Z"},
                                 {"sender": "222", "emoji": "❤️", "timestamp": "2026-10-22T10:00:01Z"}]}
        def api(endpoint, payload):
            if endpoint == "/messages":
                return {"messages": [message], "context_included": True}
            if endpoint == "/message_context":
                return {"message": message, "before": [message], "after": [message]}
            return {"name": {"111": "Alice", "222": "Bob"}[payload["sender_jid"]]}

        with patch("whatsapp._api_post", side_effect=api), patch.dict("whatsapp._sender_name_cache", {}, clear=True):
            for include_context in (False, True):
                output = whatsapp.list_messages(include_context=include_context)
                self.assertIn("Reactions: 👍 (Alice), ❤️ (Bob)", output)
                self.assertIn("Original", output)
            context = whatsapp.get_message_context("target")
            self.assertEqual(context.message.reactions, message["reactions"])
            self.assertEqual(context.before[0].reactions, message["reactions"])
            self.assertEqual(context.after[0].reactions, message["reactions"])

    def test_old_bridge_without_reactions(self):
        message = whatsapp._message_from_dict({"is_from_me": True, "content": "Original"})
        self.assertEqual(whatsapp.format_message(message), "[unknown time] From: Me: Original\n")
