import json
import unittest
from unittest.mock import Mock, patch

import requests
from mcp import types

import whatsapp
from test_mcp_modes import load_main


SECRET = "private-response-or-url"


def response(status=200, body=None):
    return Mock(status_code=status, text=SECRET, json=Mock(return_value=body))


class BridgeErrors(unittest.IsolatedAsyncioTestCase):
    async def call(self, mod, name, arguments):
        handler = mod.mcp._mcp_server.request_handlers[types.CallToolRequest]
        return (await handler(types.CallToolRequest(
            method="tools/call", params=types.CallToolRequestParams(name=name, arguments=arguments)
        ))).root

    async def test_bridge_failures_are_safe_tool_errors(self):
        mod = load_main()
        cases = (
            ({"return_value": response(401)}, "HTTP 401"),
            ({"return_value": response(500)}, "HTTP 500"),
            ({"side_effect": requests.Timeout(SECRET)}, "timed out"),
            ({"side_effect": requests.ConnectionError(SECRET)}, "unavailable"),
            ({"return_value": Mock(status_code=200, json=Mock(side_effect=json.JSONDecodeError(SECRET, SECRET, 0)))}, "invalid JSON"),
            ({"return_value": response(body=[])}, "invalid response"),
        )
        for kwargs, reason in cases:
            with self.subTest(reason), patch("whatsapp.requests.post", **kwargs), self.assertNoLogs("whatsapp", level="WARNING"):
                result = await self.call(mod, "list_messages", {"include_context": False})
                self.assertTrue(result.isError)
                text = " ".join(c.text for c in result.content)
                self.assertIn(reason, text)
                self.assertNotIn(SECRET, text)

    async def test_empty_success_is_not_an_error(self):
        with patch("whatsapp.requests.post", return_value=response(body={"messages": []})):
            result = await self.call(load_main(), "list_messages", {"include_context": False})
        self.assertFalse(result.isError)

    async def test_context_and_sender_lookup_failures_are_not_hidden(self):
        message = {"id": "M1", "chat_jid": "chat", "sender": "sender", "content": "hello"}
        for include_context in (True, False):
            with self.subTest(include_context), patch.dict(whatsapp._sender_name_cache, clear=True), patch(
                "whatsapp.requests.post", side_effect=[response(body={"messages": [message]}), response(500)]
            ):
                result = await self.call(load_main(), "list_messages", {"include_context": include_context})
                self.assertTrue(result.isError)
                self.assertIn("HTTP 500", result.content[0].text)
                self.assertNotIn(SECRET, result.content[0].text)

    def test_action_auth_error_does_not_include_response_body(self):
        with patch("whatsapp.requests.request", return_value=response(401)):
            ok, reason = whatsapp.send_contact("test", "name", "test")
        self.assertFalse(ok)
        self.assertIn("HTTP 401", reason)
        self.assertNotIn(SECRET, reason)

    def test_download_auth_log_does_not_include_response_body(self):
        with patch("whatsapp.requests.post", return_value=response(401)), self.assertLogs("whatsapp", level="WARNING") as logs:
            path, _ = whatsapp.download_media("test", "chat")
        self.assertIsNone(path)
        self.assertIn("HTTP 401", " ".join(logs.output))
        self.assertNotIn(SECRET, " ".join(logs.output))
