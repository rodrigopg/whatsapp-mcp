import unittest
from unittest.mock import Mock, patch

import requests
import whatsapp


def message(mid, **fields):
    return dict(id=mid, chat_jid="chat@lid", chat_name="Chat", sender="sender",
                content=mid, timestamp="2026-10-01T08:00:00Z", **fields)


class BatchContextTest(unittest.TestCase):
    def test_one_request_and_existing_format(self):
        messages = [message("before"), message("target", media_type="image"),
                    message("after", reactions=[{"sender": "reactor", "emoji": "ok"}])]
        with patch("whatsapp._api_post", return_value={"messages": messages, "context_included": True}) as post, \
                patch("whatsapp.get_sender_name", return_value="Alice"):
            output = whatsapp.list_messages(limit=50, context_before=2, context_after=3)
        post.assert_called_once_with("/messages", {
            "after": None, "before": None, "sender_phone_number": None, "chat_jid": None,
            "query": None, "limit": 50, "page": 0,
            "include_context": True, "context_before": 2, "context_after": 3,
        })
        self.assertEqual(output,
                         "[2026-10-01 08:00:00] Chat: Chat From: Alice: [Message ID: before - Chat JID: chat@lid] before\n"
                         "[2026-10-01 08:00:00] Chat: Chat From: Alice: [image - Message ID: target - Chat JID: chat@lid] target\n"
                         "[2026-10-01 08:00:00] Chat: Chat From: Alice: [Message ID: after - Chat JID: chat@lid] after\nReactions: ok (Alice)\n")

    def test_without_context(self):
        with patch("whatsapp._api_post", return_value={"messages": [message("target", is_from_me=True)]}) as post:
            self.assertIn("From: Me: [Message ID: target - Chat JID: chat@lid] target", whatsapp.list_messages(include_context=False))
        post.assert_called_once()
        self.assertFalse(post.call_args.args[1].get("include_context", False))

    def test_empty_batch(self):
        with patch("whatsapp._api_post", return_value={"messages": [], "context_included": True}) as post:
            self.assertEqual(whatsapp.list_messages(), "No messages to display.")
        post.assert_called_once()

    def test_malformed_data_are_safe_errors(self):
        for result in ({},
                       {"messages": None, "context_included": True},
                       {"messages": ["private-response"], "context_included": True}):
            with self.subTest(result=result), patch("whatsapp._api_post", return_value=result):
                with self.assertRaises(whatsapp.BridgeError) as caught:
                    whatsapp.list_messages()
                self.assertNotIn("private-response", str(caught.exception))

    def test_batch_request_failure_is_safe_error(self):
        for failure in (requests.Timeout("private-response"), requests.ConnectionError("private-response")):
            with self.subTest(failure=failure), patch("whatsapp.requests.post", side_effect=failure) as post:
                with self.assertRaises(whatsapp.BridgeError) as caught:
                    whatsapp.list_messages()
                self.assertNotIn("private-response", str(caught.exception))
            post.assert_called_once()
        with patch("whatsapp.requests.post", return_value=Mock(status_code=500, text="private-response")):
            with self.assertRaisesRegex(whatsapp.BridgeError, "HTTP 500"):
                whatsapp.list_messages()
