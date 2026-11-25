import asyncio
import json
import unittest
from unittest.mock import Mock, patch

import whatsapp
from test_mcp_modes import load_main


def settings_response(status, applied, message=None):
    """A /group_settings reply shaped like the bridge's GroupActionResponse."""
    if message is None:
        message = ("updated: " + ", ".join(applied)) if status == 200 else (
            "Set topic error: Outbound rate limit reached; retry in 30 seconds (applied: name)"
            if status == 429 else "Set topic error: boom (applied: name)")
    body = {"success": status == 200, "message": message}
    if applied is not None:
        body["applied"] = applied
    response = Mock(status_code=status, headers={"Retry-After": "30"} if status == 429 else {})
    response.text = json.dumps(body)
    response.json.return_value = body
    return response


class GroupSettingsProgressTests(unittest.TestCase):
    def test_partial_rate_limit_and_retry_remaining(self):
        with patch.object(whatsapp.requests, "request", side_effect=[
            settings_response(429, ["name"]), settings_response(200, ["topic"]),
        ]) as request:
            ok, message = whatsapp.update_group_settings("123@g.us", name="test", topic="topic")
            self.assertFalse(ok)
            self.assertIn("rate limit", message.lower())
            self.assertIn("30", message)
            self.assertIn("applied fields: name", message.lower())
            self.assertEqual(request.call_count, 1)
            ok, message = whatsapp.update_group_settings("123@g.us", topic="topic")
            self.assertTrue(ok)
            self.assertEqual(request.call_count, 2)
            self.assertIn("applied fields: topic", message.lower())
            self.assertEqual(request.call_args.kwargs["json"], {"group_jid": "123@g.us", "topic": "topic"})

    def test_partial_server_failure_reaches_mcp_tool(self):
        mod = load_main()
        with patch.object(whatsapp.requests, "request", return_value=settings_response(500, ["name"])):
            result = asyncio.run(mod.mcp.call_tool("update_group_settings", {
                "group_jid": "123@g.us", "name": "test", "topic": "topic",
            }))
        text = " ".join(item.text for item in result)
        self.assertIn('"success": false', text)
        self.assertIn("applied fields: name", text.lower())

    def test_partial_rate_error_reaches_mcp_tool_without_retry(self):
        mod = load_main()
        with patch.object(whatsapp.requests, "request", return_value=settings_response(429, ["name"])) as request:
            result = asyncio.run(mod.mcp.call_tool("update_group_settings", {
                "group_jid": "123@g.us", "name": "test", "topic": "topic",
            }))
        text = " ".join(item.text for item in result)
        self.assertIn('"success": false', text)
        self.assertIn("30 seconds", text)
        self.assertIn("applied fields: name", text.lower())
        request.assert_called_once()

    def test_older_bridge_without_applied_omits_progress(self):
        for status in (200, 429, 500):
            with self.subTest(status=status), patch.object(
                whatsapp.requests, "request", return_value=settings_response(status, None, "updated: name" if status == 200 else None)
            ) as request:
                ok, message = whatsapp.update_group_settings("123@g.us", name="test")
            self.assertEqual(ok, status == 200)
            self.assertNotIn("applied fields", message.lower())
            request.assert_called_once()

    def test_applied_fields_are_allowlisted(self):
        for status in (200, 429, 500):
            for applied in (["name", "bogus", {"topic": "x"}, "name", "locked"],
                            [], "bogus", None, {"name": "bogus"}):
                with self.subTest(status=status, applied=applied), patch.object(
                    whatsapp.requests, "request", return_value=settings_response(status, applied, "result")
                ):
                    ok, message = whatsapp.update_group_settings("123@g.us", name="test", locked=True)
                    self.assertEqual(ok, status == 200)
                    if applied is None:
                        self.assertEqual(message, "result")
                    elif isinstance(applied, list) and applied:
                        self.assertEqual(message, "result; applied fields: name, locked")
                    else:
                        self.assertEqual(message, "result; applied fields: none")


if __name__ == "__main__":
    unittest.main()
