import base64
import hashlib
import json
import os
import re
import secrets
import sys
import tempfile
import unittest
from urllib.parse import parse_qs, urlparse

from starlette.testclient import TestClient

from test_mcp_modes import load_main

PASS = "p" * 32
TOKEN = "static-token-123"
URL = "https://mcp.example.com"
CALLBACK = "https://claude.ai/api/mcp/auth_callback"


def pkce():
    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    return verifier, challenge


class OAuthFlow(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.mod = load_main(MCP_TRANSPORT="streamable-http", MCP_AUTH_TOKEN=TOKEN, MCP_PUBLIC_URL=URL,
                             MCP_OAUTH_PASSPHRASE=PASS, MCP_OAUTH_DB=os.path.join(self.tmp.name, "o.db"))
        self.client = TestClient(self.mod.build_http_app(), base_url=URL, follow_redirects=False)
        self.client.__enter__()

    def tearDown(self):
        self.client.__exit__(None, None, None)
        self.tmp.cleanup()

    def register(self, redirect=CALLBACK):
        return self.client.post("/register", json={"redirect_uris": [redirect], "client_name": "Claude",
                                                   "token_endpoint_auth_method": "none", "grant_types": ["authorization_code", "refresh_token"],
                                                   "response_types": ["code"]})

    def login(self, passphrase=PASS):
        """Run /authorize and the login page; returns (client_id, verifier, response of the login POST)."""
        cid = self.register().json()["client_id"]
        verifier, challenge = pkce()
        r = self.client.get("/authorize", params={"client_id": cid, "redirect_uri": CALLBACK, "response_type": "code", "state": "xyz",
                                                  "code_challenge": challenge, "code_challenge_method": "S256", "scope": "whatsapp"})
        self.assertEqual(r.status_code, 302, r.text)
        login_url = r.headers["location"]
        self.assertTrue(login_url.startswith(URL + "/oauth/login?req="), login_url)
        page = self.client.get(login_url.replace(URL, ""))
        self.assertEqual(page.status_code, 200)
        self.assertIn("no-store", page.headers["cache-control"])
        self.assertIn("claude.ai", page.text, "the page must say where the owner is sent back to")
        req = re.search(r'name="req" value="([^"]+)"', page.text).group(1)
        post = self.client.post("/oauth/login", content=f"req={req}&passphrase={passphrase}",
                                headers={"content-type": "application/x-www-form-urlencoded"})
        return cid, verifier, post

    def token(self, cid, verifier, code, **extra):
        return self.client.post("/token", data={"grant_type": "authorization_code", "client_id": cid, "code": code,
                                                "code_verifier": verifier, "redirect_uri": CALLBACK, **extra})

    def mcp_call(self, bearer):
        headers = {"accept": "application/json, text/event-stream", "content-type": "application/json"}
        if bearer:
            headers["authorization"] = f"Bearer {bearer}"
        return self.client.post("/mcp/", headers=headers, json={"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
            "protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "t", "version": "1"}}})

    def test_discovery_documents(self):
        meta = self.client.get("/.well-known/oauth-authorization-server").json()
        self.assertEqual(meta["issuer"].rstrip("/"), URL)
        self.assertIn("S256", meta["code_challenge_methods_supported"])
        for path in ("/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"):
            prm = self.client.get(path).json()
            self.assertEqual((prm["resource"], prm["authorization_servers"]), (URL + "/mcp", [URL]))

    def test_full_flow_with_pkce_then_mcp_and_refresh_rotation(self):
        cid, verifier, post = self.login()
        self.assertEqual(post.status_code, 302)
        loc = urlparse(post.headers["location"])
        self.assertEqual(loc._replace(query="").geturl(), CALLBACK)
        q = parse_qs(loc.query)
        self.assertEqual(q["state"], ["xyz"])
        tok = self.token(cid, verifier, q["code"][0])
        self.assertEqual(tok.status_code, 200, tok.text)
        body = tok.json()
        self.assertEqual(self.mcp_call(body["access_token"]).status_code, 200)
        # the code is single use
        self.assertNotEqual(self.token(cid, verifier, q["code"][0]).status_code, 200)
        # refresh rotates: the old refresh token stops working
        r1 = self.client.post("/token", data={"grant_type": "refresh_token", "client_id": cid, "refresh_token": body["refresh_token"]})
        self.assertEqual(r1.status_code, 200, r1.text)
        self.assertNotEqual(r1.json()["refresh_token"], body["refresh_token"])
        r2 = self.client.post("/token", data={"grant_type": "refresh_token", "client_id": cid, "refresh_token": body["refresh_token"]})
        self.assertNotEqual(r2.status_code, 200)

    def test_wrong_pkce_verifier_is_refused(self):
        cid, _, post = self.login()
        code = parse_qs(urlparse(post.headers["location"]).query)["code"][0]
        self.assertNotEqual(self.token(cid, pkce()[0], code).status_code, 200)

    def test_wrong_passphrase_never_issues_a_code_and_locks_out(self):
        _, _, post = self.login("wrong")
        self.assertEqual(post.status_code, 401)
        self.assertNotIn("code=", post.headers.get("location", ""))
        for _ in range(12):
            self.client.post("/oauth/login", content="req=x&passphrase=wrong", headers={"content-type": "application/x-www-form-urlencoded"})
        _, _, locked = self.login(PASS)
        self.assertEqual(locked.status_code, 429, "too many failures must block even the right passphrase for a while")

    def test_oversized_login_body_is_refused(self):
        r = self.client.post("/oauth/login", content="x=" + "a" * 5000, headers={"content-type": "application/x-www-form-urlencoded"})
        self.assertEqual(r.status_code, 413)

    def test_redirect_uris_outside_claude_and_loopback_are_refused(self):
        self.assertEqual(self.register("https://claude.ai/api/mcp/auth_callback").status_code, 201)
        self.assertEqual(self.register("http://localhost:54321/callback").status_code, 201)
        for chatgpt in ("https://chatgpt.com/connector_platform_oauth_redirect", "https://chatgpt.com/oauth/callback", "https://chat.openai.com/oauth/callback"):
            self.assertEqual(self.register(chatgpt).status_code, 201, chatgpt)
        for bad in ("https://evil.example/cb", "http://claude.ai/cb", "https://claude.ai.evil.example/cb", "https://chatgpt.com.evil.example/cb", "http://chatgpt.com/cb"):
            self.assertEqual(self.register(bad).status_code, 400, bad)

    def test_mcp_needs_a_valid_token_but_the_static_token_still_works(self):
        self.assertEqual(self.mcp_call(None).status_code, 401)
        self.assertEqual(self.mcp_call("nope").status_code, 401)
        self.assertEqual(self.mcp_call(TOKEN).status_code, 200)

    def test_tokens_are_stored_hashed_and_survive_a_restart(self):
        cid, verifier, post = self.login()
        code = parse_qs(urlparse(post.headers["location"]).query)["code"][0]
        access = self.token(cid, verifier, code).json()["access_token"]
        raw = open(os.path.join(self.tmp.name, "o.db"), "rb").read()
        self.assertNotIn(access.encode(), raw, "access tokens must not be stored in clear")
        again = TestClient(load_main(MCP_TRANSPORT="streamable-http", MCP_AUTH_TOKEN=TOKEN, MCP_PUBLIC_URL=URL, MCP_OAUTH_PASSPHRASE=PASS,
                                     MCP_OAUTH_DB=os.path.join(self.tmp.name, "o.db")).mcp.streamable_http_app(), base_url=URL)
        with again:
            h = {"accept": "application/json, text/event-stream", "content-type": "application/json", "authorization": f"Bearer {access}"}
            r = again.post("/mcp/", headers=h, json={"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
                "protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "t", "version": "1"}}})
            self.assertEqual(r.status_code, 200)


class McpPathWithoutSlash(unittest.TestCase):
    """Connector validators POST to the URL exactly as typed (`.../mcp`) and do not follow redirects."""

    def post(self, client, bearer):
        h = {"accept": "application/json, text/event-stream", "content-type": "application/json"}
        if bearer:
            h["authorization"] = f"Bearer {bearer}"
        return client.post("/mcp", headers=h, json={"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
            "protocolVersion": "2025-03-26", "capabilities": {}, "clientInfo": {"name": "t", "version": "1"}}})

    def test_mcp_without_slash_answers_directly_with_oauth_and_with_plain_bearer(self):
        with tempfile.TemporaryDirectory() as d:
            oauth = load_main(MCP_TRANSPORT="streamable-http", MCP_AUTH_TOKEN=TOKEN, MCP_PUBLIC_URL=URL, MCP_OAUTH_PASSPHRASE=PASS,
                              MCP_OAUTH_DB=os.path.join(d, "o.db"))
            for name, mod in (("oauth", oauth), ("plain", load_main(MCP_TRANSPORT="streamable-http", MCP_AUTH_TOKEN=TOKEN))):
                with TestClient(mod.build_http_app(), base_url=URL, follow_redirects=False) as c:
                    r = self.post(c, None)
                    self.assertEqual(r.status_code, 401, f"{name}: no token must be 401, not a redirect")
                    if name == "oauth":
                        self.assertEqual(r.headers["www-authenticate"], f'Bearer resource_metadata="{URL}/.well-known/oauth-protected-resource/mcp"')
                    else:
                        self.assertEqual(r.headers["www-authenticate"], "Bearer")
                    self.assertEqual(self.post(c, "wrong").status_code, 401, name)
                    self.assertEqual(self.post(c, TOKEN).status_code, 200, name)


class OAuthConfig(unittest.TestCase):
    def test_off_by_default_and_the_static_token_wrapper_is_unchanged(self):
        mod = load_main(MCP_TRANSPORT="streamable-http", MCP_AUTH_TOKEN=TOKEN)
        client = TestClient(mod.BearerAuth(mod.mcp.streamable_http_app(), TOKEN), base_url=URL)
        with client:
            self.assertEqual(client.get("/.well-known/oauth-authorization-server").status_code, 401)

    def test_incomplete_or_weak_config_refuses_to_start(self):
        for env in ({"MCP_PUBLIC_URL": URL}, {"MCP_OAUTH_PASSPHRASE": PASS},
                    {"MCP_PUBLIC_URL": "http://mcp.example.com", "MCP_OAUTH_PASSPHRASE": PASS},
                    {"MCP_PUBLIC_URL": URL, "MCP_OAUTH_PASSPHRASE": "short"}):
            with self.assertRaises(SystemExit, msg=str(env)):
                load_main(MCP_TRANSPORT="streamable-http", MCP_AUTH_TOKEN=TOKEN, **env)


if __name__ == "__main__":
    unittest.main()
