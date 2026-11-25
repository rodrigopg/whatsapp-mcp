import asyncio
import unittest
from unittest.mock import Mock, patch

import requests
import whatsapp
from test_mcp_modes import load_main, tool_names


class ChatBackfillTest(unittest.TestCase):
    def test_posts_chat_and_count(self):
        response = Mock(status_code=200)
        response.json.return_value = {"success": True, "message": "History requested"}
        with patch("whatsapp._api_request", return_value=response) as post:
            ok, message = whatsapp.request_chat_history("111@s.whatsapp.net", 20)
        self.assertTrue(ok)
        self.assertEqual(message, "History requested")
        post.assert_called_once_with("POST", "/chat/backfill", timeout=whatsapp.SEND_TIMEOUT,
                                     json={"chat_jid": "111@s.whatsapp.net", "count": 20})

    def test_invalid_input_does_not_call_bridge(self):
        for chat, count in [(" ", 1), ("111@s.whatsapp.net", 0), ("111@s.whatsapp.net", 51),
                            ("111@s.whatsapp.net", -1), ("111@s.whatsapp.net", True)]:
            with self.subTest(chat=chat, count=count), patch("whatsapp._api_request") as post:
                self.assertFalse(whatsapp.request_chat_history(chat, count)[0])
                post.assert_not_called()

    def test_bridge_errors_reach_the_caller(self):
        no_history = "No stored messages for this chat; cannot request earlier history"
        for status, body, expected in [
                (404, {"error": no_history}, no_history + " (HTTP 404)"),
                (500, {"error": "Failed to request chat history from phone: timed out"}, "HTTP 500"),
                (401, {}, "HTTP 401"),
                (200, {"success": False, "message": "refused"}, "operation failed"),
                (200, [], "invalid response")]:
            response = Mock(status_code=status)
            response.json.return_value = body
            with self.subTest(status=status), patch("whatsapp._api_request", return_value=response):
                ok, message = whatsapp.request_chat_history("111@s.whatsapp.net")
                self.assertFalse(ok)
                self.assertIn(expected, message)
        for status, text, expected in [
                (403, "Forbidden: API is read-only\n", "Bridge access denied; check API_AUTH_TOKEN and API_ALLOWED_HOSTS (HTTP 403)"),
                (200, "<html>", "Bridge returned invalid JSON")]:
            response = Mock(status_code=status, text=text)
            response.json.side_effect = requests.exceptions.JSONDecodeError("bad", "", 0)
            with self.subTest(status=status), patch("whatsapp._api_request", return_value=response):
                self.assertEqual(whatsapp.request_chat_history("111@s.whatsapp.net"), (False, expected))
        with patch("whatsapp._api_request", side_effect=whatsapp.BridgeError("Bridge unavailable")):
            self.assertEqual(whatsapp.request_chat_history("111@s.whatsapp.net"),
                             (False, "Bridge unavailable"))

    @patch.dict("sys.modules")
    def test_tool_registered_as_write_and_forwards_arguments(self):
        mod = load_main()
        self.assertIn("request_chat_history", tool_names(mod))
        with patch.object(mod, "whatsapp_request_chat_history", return_value=(True, "requested")) as call:
            tool = mod.mcp._tool_manager.get_tool("request_chat_history")
            result = tool.fn(chat_jid="111@s.whatsapp.net", count=7)
            if asyncio.iscoroutine(result):
                result = asyncio.run(result)
        self.assertEqual(result, {"success": True, "message": "requested"})
        call.assert_called_once_with("111@s.whatsapp.net", 7)
        self.assertNotIn("request_chat_history", tool_names(load_main(MCP_READONLY="true")))
