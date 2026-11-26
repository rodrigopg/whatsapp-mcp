import unittest
from unittest.mock import Mock, patch

import requests
import whatsapp
from test_batch_context import message


class LegacyContextTest(unittest.TestCase):
    def test_old_bridge_expands_each_message_with_original_order(self):
        replies = [
            {"messages": [message("target"), message("other")]},
            {"message": message("target"), "before": [message("before")], "after": [message("after")]},
            {"message": message("other"), "before": [message("after")], "after": []},
        ]
        with patch("whatsapp._api_post", side_effect=replies) as post, \
                patch("whatsapp.get_sender_name", return_value="Alice"):
            output = whatsapp.list_messages(context_before=2, context_after=3)
        self.assertEqual([line.rsplit("] ", 1)[-1] for line in output.splitlines()],
                         ["before", "target", "after", "after", "other"])
        self.assertEqual(post.call_count, 3)
        self.assertEqual(post.call_args_list[1].args, (
            "/message_context", {"message_id": "target", "before": 2, "after": 3}))
        self.assertEqual(post.call_args_list[2].args, (
            "/message_context", {"message_id": "other", "before": 2, "after": 3}))

    def test_old_bridge_empty_page_does_not_fetch_context(self):
        with patch("whatsapp._api_post", return_value={"messages": []}) as post:
            self.assertEqual(whatsapp.list_messages(), "No messages to display.")
        post.assert_called_once()

    def test_old_bridge_missing_context_warns_and_keeps_message(self):
        with patch("whatsapp._api_post", side_effect=[{"messages": [message("target")]}, {}]), \
                patch("whatsapp.get_sender_name", return_value="Alice"):
            output = whatsapp.list_messages()
        self.assertIn("From: Alice: [Message ID: target - Chat JID: chat@lid] target\n", output)
        self.assertIn("context lookup failed for 1 message(s)", output)
        self.assertIn("Message with ID target not found", output)

    def test_old_bridge_context_failure_remains_safe_error(self):
        for failure, reason in ((401, "HTTP 401"), (500, "HTTP 500"),
                                (requests.Timeout("private-response"), "timed out"),
                                (requests.ConnectionError("private-response"), "unavailable")):
            def post(url, **kwargs):
                if url.endswith("/messages"):
                    return Mock(status_code=200, json=lambda: {"messages": [message("target")]})
                if isinstance(failure, Exception):
                    raise failure
                return Mock(status_code=failure, text="private-response")

            with self.subTest(failure=failure), patch("whatsapp.requests.post", side_effect=post):
                with self.assertRaises(whatsapp.BridgeError) as caught:
                    whatsapp.list_messages()
                self.assertNotIn("private-response", str(caught.exception))
                self.assertIn(reason, str(caught.exception))
