import os
import tempfile
import unittest
from unittest.mock import Mock, patch

import requests
from mcp import types

import whatsapp
from test_mcp_modes import load_main


SECRET = "PRIVATE_MESSAGE"


def response(status=200, body=None):
    return Mock(status_code=status, text=SECRET, headers={}, json=Mock(return_value=body))


class AllToolBridgeErrors(unittest.IsolatedAsyncioTestCase):
    async def call(self, mod, name, arguments):
        handler = mod.mcp._mcp_server.request_handlers[types.CallToolRequest]
        return (await handler(types.CallToolRequest(
            method="tools/call", params=types.CallToolRequestParams(name=name, arguments=arguments)
        ))).root

    async def test_every_tool_sanitizes_bridge_failures(self):
        mod = load_main()
        voice_path = os.path.join(self.enterContext(tempfile.TemporaryDirectory()), "voice.ogg")

        def synthesize(_text):
            # send_voice_message deletes the file after each send
            with open(voice_path, "wb") as f:
                f.write(b"voice")
            return voice_path
        values = {
            "query": "test", "chat_jid": "chat", "jid": "group@g.us", "group_jid": "group@g.us",
            "message_id": "M1", "recipient": "test", "message": "hello", "text": "hello",
            "new_text": "hello", "media_path": "test.ogg", "name": "test", "phone": "test",
            "phone_number": "test", "sender_phone_number": "test", "participants": ["test"],
            "message_ids": ["M1"], "phones": ["test"], "options": ["one", "two"],
            "question": "test?", "emoji": "", "action": "add", "link": "code",
            "state": "paused", "as_of": "2026-10-23T00:00:00Z", "up_to": "cursor",
            "matter_id": 1, "draft_id": 1, "by": "owner", "owed_by": "owner",
            "origin": "request", "decision": "keep",
        }
        secret = SECRET
        cases = [
            ({"return_value": response(400, {"error": secret})}, "Invalid bridge request"),
            ({"return_value": response(401, {"message": secret})}, "authentication"),
            ({"return_value": response(403, {"error": secret})}, "access denied"),
            ({"return_value": response(404, {"message": secret})}, "not found"),
            ({"return_value": response(429, {"error": secret, "retry_after": 17})}, "17 seconds"),
            ({"return_value": response(503, {"message": secret})}, "not connected"),
            ({"return_value": response(500, {"message": secret})}, "HTTP 500"),
            ({"return_value": response(200, {"success": False, "message": secret})}, "operation failed"),
            ({"return_value": response(200, {"error": secret})}, "operation failed"),
            ({"return_value": response(200, [])}, "invalid response"),
            ({"return_value": response(200, None)}, "invalid response"),
            ({"return_value": Mock(status_code=200, text=secret,
                json=Mock(side_effect=ValueError(secret)))}, "invalid JSON"),
            ({"side_effect": requests.ConnectionError(secret)}, "unavailable"),
            ({"side_effect": requests.ReadTimeout(secret)}, "timed out"),
        ]
        for status, expected in ((400, "Invalid bridge request"), (403, "access denied"),
                                 (429, "rate limit"), (500, "HTTP 500")):
            cases.append(({"return_value": Mock(status_code=status, text=secret, headers={},
                json=Mock(side_effect=ValueError(secret)))}, expected))
        for tool in await mod.mcp.list_tools():
            if tool.name == "download_media":
                # download_media keeps its own status-based messages (see test_bridge_errors.py).
                continue
            arguments = {name: values[name] for name in tool.inputSchema.get("required", [])}
            if tool.name == "update_group_settings":
                arguments["name"] = "test"
            if tool.name == "send_file":
                arguments["media_path"] = "test.ogg"  # optional since upload_id (#118)
            for mock_args, expected in cases:
                if tool.name == "get_message_context" and expected == "Invalid bridge request":
                    expected = "not found"  # the bridge answers an unknown message ID with HTTP 400
                with self.subTest(tool=tool.name, expected=expected), \
                     patch("whatsapp.requests.post", **mock_args) as post, \
                     patch("whatsapp.requests.request", **mock_args) as request, \
                     patch("whatsapp.media_files.read_outbound", return_value=b"audio"), \
                     patch("whatsapp.tts.engine_ready", return_value=(True, "test")), \
                     patch("whatsapp.tts.synthesize", side_effect=synthesize), \
                     patch.dict(whatsapp._sender_name_cache, clear=True), \
                     self.assertLogs("whatsapp", level="WARNING") as logs:
                    result = await self.call(mod, tool.name, arguments)
                    self.assertTrue(post.called or request.called, "tool did not reach the bridge")
                    text = " ".join(c.text for c in result.content)
                    self.assertIn(expected, text)
                    for private in ("/private/secret", "https://bridge.invalid", "PRIVATE_MESSAGE"):
                        self.assertNotIn(private, text)
                    self.assertNotIn("PRIVATE_MESSAGE", " ".join(logs.output))


class BridgeResponseContract(unittest.TestCase):
    def test_only_numeric_retry_delays_are_exposed(self):
        for retry, headers, expected in (
            (17, {}, "retry in 17 seconds"), (1.5, {}, "retry in 1.5 seconds"),
            (None, {"Retry-After": "23"}, "retry in 23 seconds"),
            (None, {"Retry-After": SECRET}, "retry later"),
            (SECRET, {}, "retry later"), (True, {}, "retry later"),
            (-1, {}, "retry later"), (float("inf"), {}, "retry later"),
            (float("nan"), {}, "retry later"),
        ):
            reply = response(429, {"error": SECRET, "retry_after": retry})
            reply.headers = headers
            with self.subTest(retry=retry, headers=headers), \
                 patch("whatsapp.requests.post", return_value=reply), \
                 self.assertLogs("whatsapp", level="WARNING"):
                ok, reason = whatsapp.request_chat_history("chat")
            self.assertFalse(ok)
            self.assertIn(expected, reason)
            self.assertNotIn(SECRET, reason)

    def test_missing_success_cannot_expose_an_action_error_message(self):
        with patch("whatsapp.requests.post", return_value=response(body={"message": SECRET})), \
             self.assertLogs("whatsapp", level="WARNING"):
            ok, reason = whatsapp.send_message("test", "hello")
        self.assertFalse(ok)
        self.assertIn("operation failed", reason)
        self.assertNotIn(SECRET, reason)

    def test_success_payloads_and_message_ids_are_preserved(self):
        body = {"success": True, "message": "Sent (message_id: M1)", "message_id": "M1",
                "path": "download.ogg", "jid": "group@g.us", "name": "group", "participant_count": 2}
        cases = (
            (lambda: whatsapp.send_message("test", "hello"), (True, body["message"])),
            (lambda: whatsapp.send_file("test", "audio.ogg"), (True, body["message"])),
            (lambda: whatsapp.send_audio_message("test", "audio.ogg"), (True, body["message"])),
            (lambda: whatsapp.request_chat_history("chat"), (True, body["message"])),
            (lambda: whatsapp.create_poll("chat", "question", ["one", "two"]),
             (True, "Poll sent (message_id: M1)")),
            (lambda: whatsapp.send_contact("test", "name", "test"), (True, "Contact sent (message_id: M1)")),
            (lambda: whatsapp.create_group("group", ["test"]), (True, body["message"],
             {"jid": "group@g.us", "name": "group", "participant_count": 2})),
            (lambda: whatsapp.download_media("M1", "chat"), ("download.ogg", "Media downloaded successfully")),
            (lambda: whatsapp.matter_call("/matters/list", {}), body),
        )
        for call, expected in cases:
            with self.subTest(expected=expected), patch("whatsapp.requests.post", return_value=response(body=body)), \
                 patch("whatsapp.media_files.read_outbound", return_value=b"audio"), \
                 patch("whatsapp.media_files.save_download", return_value="download.ogg"):
                self.assertEqual(call(), expected)


class BridgeInputErrors(unittest.TestCase):
    def test_bridge_input_error_reaches_the_agent_without_paths(self):
        author_error = ("the author is no longer in the group and the bridge does not know their address in it, "
                        "so WhatsApp would drop this action")
        cases = (
            (400, {"success": False, "message": author_error}, author_error),
            (404, {"error": "poll not found in the local database"}, "poll not found in the local database"),
            (400, {"message": "open /private/secret: no such file"}, "Invalid bridge request"),
            (400, {"message": "C:\\Users\\secret"}, "Invalid bridge request"),
            (400, {"message": "x" * 301}, "Invalid bridge request"),
            (500, {"message": "database is locked"}, "Bridge request failed"),
        )
        for status, body, want in cases:
            with self.subTest(body=body), self.assertLogs("whatsapp", level="WARNING"), \
                    patch("whatsapp.requests.post", return_value=response(status, body)):
                ok, message = whatsapp.edit_message("chat", "id", "text")
            self.assertFalse(ok)
            self.assertIn(want, message)
            self.assertNotIn("secret", message)
