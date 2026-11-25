import json
import unittest
from unittest.mock import patch

import requests
from mcp import types

import whatsapp
from test_mcp_modes import load_main


def bridge_response(status, body, plain=False):
    response = requests.Response()
    response.status_code = status
    response._content = (body if plain else json.dumps(body)).encode()
    return response


class BridgeErrorAllowlist(unittest.IsolatedAsyncioTestCase):
    async def call_vote(self, response):
        mod = load_main()
        handler = mod.mcp._mcp_server.request_handlers[types.CallToolRequest]
        with patch("whatsapp.requests.post", return_value=response), \
                self.assertLogs("whatsapp", level="WARNING") as logs:
            result = (await handler(types.CallToolRequest(
                method="tools/call", params=types.CallToolRequestParams(
                    name="vote_in_poll", arguments={"chat_jid": "chat", "message_id": "M1", "options": ["unknown"]}
                )
            ))).root
        return " ".join(content.text for content in result.content), " ".join(logs.output)

    async def test_stored_poll_options_do_not_reach_mcp_clients(self):
        # poll_vote.go validateVote -> writeJSONError includes options read from the poll store.
        body = {"error": '"unknown" is not an option of this poll (options: PRIVATE_MESSAGE, other)'}
        text, logs = await self.call_vote(bridge_response(400, body))
        self.assertIn("Invalid bridge request", text)
        self.assertIn("HTTP 400", text)
        self.assertNotIn("PRIVATE_MESSAGE", text)
        self.assertNotIn("PRIVATE_MESSAGE", logs)

    async def test_static_poll_error_reaches_mcp_clients(self):
        detail = "poll not found: this bridge has not seen that poll"
        text, _ = await self.call_vote(bridge_response(400, {"error": detail}))
        self.assertIn(detail, text)

    def test_only_exact_static_errors_are_exposed(self):
        static = "poll message not found in local history"
        for detail in ("PRIVATE_MESSAGE", static + " PRIVATE_MESSAGE", "PRIVATE_MESSAGE " + static,
                       "unknown input", [static], {"message": static}):
            for field in ("error", "message"):
                with self.subTest(detail=detail, field=field), \
                        patch("whatsapp.requests.post", return_value=bridge_response(400, {field: detail})), \
                        self.assertLogs("whatsapp", level="WARNING"):
                    ok, reason = whatsapp.vote_poll("chat", "M1", ["unknown"])
                self.assertFalse(ok)
                self.assertEqual(reason, "Invalid bridge request; check the tool arguments (HTTP 400)")

    def test_static_plain_text_http_errors_are_exposed(self):
        detail = "Invalid request: new_text must not be empty"
        with patch("whatsapp.requests.post", return_value=bridge_response(400, detail + "\n", plain=True)), \
                self.assertLogs("whatsapp", level="WARNING"):
            ok, reason = whatsapp.edit_message("chat", "M1", "text")
        self.assertFalse(ok)
        self.assertEqual(reason, detail + " (HTTP 400)")

    def test_unknown_plain_text_errors_are_hidden(self):
        with patch("whatsapp.requests.post", return_value=bridge_response(400, "PRIVATE_MESSAGE\n", plain=True)), \
                self.assertLogs("whatsapp", level="WARNING"):
            ok, reason = whatsapp.vote_poll("chat", "M1", ["unknown"])
        self.assertFalse(ok)
        self.assertEqual(reason, "Invalid bridge request; check the tool arguments (HTTP 400)")
