import asyncio
import importlib
import os
import subprocess
import sys
import unittest
from unittest import mock

from starlette.applications import Starlette
from starlette.responses import PlainTextResponse
from starlette.routing import Route
from starlette.testclient import TestClient

HERE = os.path.dirname(os.path.abspath(__file__))

READ_TOOLS = {
    "search_contacts", "list_messages", "list_chats", "get_chat", "get_direct_chat_by_contact",
    "get_contact_chats", "get_last_interaction", "get_message_context", "get_group_info",
    "resolve_contact", "check_whatsapp", "get_poll_votes",
}


def load_main(**env):
    clean = {k: v for k, v in os.environ.items() if not k.startswith("MCP_")}
    with mock.patch.dict(os.environ, {**clean, **env}, clear=True):
        sys.modules.pop("main", None)
        return importlib.import_module("main")


def tool_names(mod):
    return {t.name for t in asyncio.run(mod.mcp.list_tools())}


class ReadonlyRegistration(unittest.TestCase):
    def test_default_exposes_read_and_write(self):
        names = tool_names(load_main())
        self.assertTrue(READ_TOOLS < names)
        for w in ("send_message", "download_media", "delete_message", "create_group"):
            self.assertIn(w, names)

    def test_readonly_exposes_exactly_read_subset(self):
        for val in ("true", "1", "TRUE"):
            self.assertEqual(tool_names(load_main(MCP_READONLY=val)), READ_TOOLS)

    def test_readonly_false_is_default(self):
        self.assertEqual(tool_names(load_main(MCP_READONLY="false")), tool_names(load_main()))


class HttpStartup(unittest.TestCase):
    def test_refuses_without_token(self):
        for host in ("127.0.0.1", "0.0.0.0"):
            with self.assertRaises(SystemExit):
                load_main().check_http_config(host, "")

    def test_accepts_with_token(self):
        load_main().check_http_config("127.0.0.1", "t")

    def test_process_exits_without_token(self):
        env = {k: v for k, v in os.environ.items() if not k.startswith("MCP_")}
        env.update(MCP_TRANSPORT="streamable-http", MCP_HOST="0.0.0.0")
        r = subprocess.run([sys.executable, os.path.join(HERE, "main.py")], env=env,
                           capture_output=True, text=True, timeout=60)
        self.assertNotEqual(r.returncode, 0)
        self.assertIn("MCP_AUTH_TOKEN", r.stderr)

    def test_unknown_transport_rejected(self):
        with mock.patch.dict(os.environ, {"MCP_TRANSPORT": "carrier-pigeon"}):
            with self.assertRaises(SystemExit):
                load_main(MCP_TRANSPORT="carrier-pigeon").main()


class BearerAuthMiddleware(unittest.TestCase):
    def setUp(self):
        app = Starlette(routes=[Route("/", lambda r: PlainTextResponse("ok"))])
        self.client = TestClient(load_main().BearerAuth(app, "s3cret"))

    def test_valid_token(self):
        r = self.client.get("/", headers={"Authorization": "Bearer s3cret"})
        self.assertEqual((r.status_code, r.text), (200, "ok"))

    def test_rejects_missing_wrong_and_wrong_scheme(self):
        for h in ({}, {"Authorization": "Bearer nope"}, {"Authorization": "Basic s3cret"},
                  {"Authorization": "s3cret"}, {"Authorization": "Bearer "}):
            r = self.client.get("/", headers=h)
            self.assertEqual(r.status_code, 401, h)
            self.assertEqual(r.headers["www-authenticate"], "Bearer")

    def test_only_lifespan_bypasses_auth(self):
        """A non-http scope must never reach the app unauthenticated; only the ASGI lifespan may pass through."""
        import asyncio
        mod = load_main()
        seen = []

        async def inner(scope, receive, send):
            seen.append(scope["type"])

        mw = mod.BearerAuth(inner, "s3cret")
        sent = []

        async def send(msg):
            sent.append(msg)

        async def run(scope):
            await mw(scope, None, send)

        asyncio.run(run({"type": "lifespan"}))
        self.assertEqual(seen, ["lifespan"])
        asyncio.run(run({"type": "websocket", "headers": []}))
        self.assertEqual(seen, ["lifespan"], "websocket scope reached the app without auth")
        self.assertEqual(sent, [{"type": "websocket.close", "code": 1008}])
        asyncio.run(run({"type": "somethingelse"}))
        self.assertEqual(seen, ["lifespan"])


if __name__ == "__main__":
    unittest.main()
