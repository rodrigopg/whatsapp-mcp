import asyncio
import json
import unittest
from unittest.mock import Mock, patch

import whatsapp
from test_mcp_modes import load_main

RATE_ERROR = "Outbound rate limit reached; retry in 30 seconds"


def rate_response(body=None):
    """A 429 the way the bridge writes it: Retry-After header plus a JSON error body."""
    body = body if body is not None else {"success": False, "error": RATE_ERROR, "message": RATE_ERROR}
    response = Mock(status_code=429, headers={"Retry-After": "30"})
    response.text = json.dumps(body)
    response.json.return_value = body
    return response


class SendRateTests(unittest.TestCase):
    def assert_rate_limited(self, ok, message):
        self.assertFalse(ok)
        self.assertIn("rate limit", message.lower())
        self.assertIn("30", message)

    def test_direct_sends_report_rate_error_without_retry(self):
        for call in (
            lambda: whatsapp.send_message("peer", "hello"),
            lambda: whatsapp.send_file("peer", "test.ogg"),
            lambda: whatsapp.send_audio_message("peer", "test.ogg"),
            lambda: whatsapp.create_group("test", ["peer"]),
        ):
            with self.subTest(call=call), patch.object(whatsapp.os.path, "isfile", return_value=True), patch.object(
                whatsapp.requests, "post", return_value=rate_response()
            ) as post, patch.object(whatsapp.requests, "request", return_value=rate_response()) as request:
                result = call()
                self.assert_rate_limited(result[0], result[1])
                self.assertEqual(post.call_count + request.call_count, 1)

    def test_shared_actions_and_drafts_report_rate_error_without_retry(self):
        for call in (
            lambda: whatsapp.send_contact("peer", "name", "number"),
            lambda: whatsapp.create_poll("peer", "question", ["yes", "no"]),
            lambda: whatsapp.vote_poll("peer", "id", ["yes"]),
            lambda: whatsapp.react_to_message("peer", "id", ""),
            lambda: whatsapp.edit_message("peer", "id", "new text"),
            lambda: whatsapp.delete_message("peer", "id"),
            lambda: whatsapp.matter_call("/drafts/approve", {"draft_id": 1}),
        ):
            with self.subTest(call=call), patch.object(
                whatsapp.requests, "post", return_value=rate_response()
            ) as post, patch.object(whatsapp.requests, "request", return_value=rate_response()) as request:
                result = call()
                if isinstance(result, dict):
                    self.assert_rate_limited(result.get("success", False), result["error"])
                else:
                    self.assert_rate_limited(result[0], result[1])
                self.assertEqual(post.call_count + request.call_count, 1)

    def test_rate_error_reaches_mcp_tool(self):
        mod = load_main()
        with patch.object(whatsapp.requests, "post", return_value=rate_response()) as post:
            result = asyncio.run(mod.mcp.call_tool("send_message", {"recipient": "peer", "message": "hello"}))
        text = " ".join(item.text for item in result)
        self.assertIn('"success": false', text)
        self.assertIn("rate limit", text.lower())
        self.assertIn("30", text)
        post.assert_called_once()


if __name__ == "__main__":
    unittest.main()
