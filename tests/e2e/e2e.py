"""Live end-to-end validation: two bridges (accounts A and B) talking to each other.

Run through tests/e2e/run.sh. Order matters (test_NN): later tests reuse ids from earlier ones.
Only accounts configured in tests/e2e/.env are touched; messages carry a per-run tag.
"""
import asyncio
import base64
import glob
import hashlib
import json
import os
import subprocess
import time
import unittest
import uuid

import requests

HERE = os.path.dirname(os.path.abspath(__file__))
ENV = {}
for line in open(os.path.join(HERE, ".env")):
    line = line.strip()
    if line and not line.startswith("#") and "=" in line:
        k, v = line.split("=", 1)
        ENV[k] = v

TOKEN = ENV["E2E_TOKEN"]
PORT = {"a": ENV["E2E_A_PORT"], "b": ENV["E2E_B_PORT"]}
PHONE = {"a": ENV["E2E_A_PHONE"], "b": ENV["E2E_B_PHONE"]}
RUN = uuid.uuid4().hex[:8]
TAG = f"e2e-{RUN}"
MEDIA = os.path.join(HERE, ".media")
COMPOSE = ["docker", "compose", "--env-file", os.path.join(HERE, ".env"),
           "-f", os.path.join(HERE, "docker-compose.e2e.yml")]
MODE = os.environ.get("E2E_MODE") or ENV.get("E2E_MODE", "docker")  # "native": bridges are plain processes started by native.sh
SEND_GAP = 2  # seconds between sends: keep the automation footprint small

MCP_TOOLS = {
    "search_contacts", "list_messages", "list_chats", "get_chat", "get_direct_chat_by_contact",
    "get_contact_chats", "get_last_interaction", "get_message_context", "send_message", "send_file",
    "send_audio_message", "download_media", "create_group", "leave_group", "mark_chat_as_read",
    "mark_chat_as_unread", "get_group_info", "archive_chat", "resolve_contact", "react_to_message",
    "edit_message", "delete_message", "update_group_participants", "send_chat_presence", "check_whatsapp",
    "get_group_invite_link", "join_group_with_link", "update_group_settings",
}


def media_path(name):
    return f"/media/{name}" if MODE == "docker" else os.path.join(MEDIA, name)


def restart(acct):
    cmd = COMPOSE + ["restart", acct] if MODE == "docker" else [os.path.join(HERE, "native.sh"), "restart", acct]
    subprocess.run(cmd, check=True, capture_output=True)


def api(acct, method, path, auth=True, **kw):
    headers = {"Authorization": f"Bearer {TOKEN}"} if auth else {}
    return requests.request(method, f"http://127.0.0.1:{PORT[acct]}/api{path}",
                            headers=headers, timeout=60, **kw)


def post(acct, path, body):
    r = api(acct, "POST", path, json=body)
    try:
        return r.status_code, r.json()
    except ValueError:
        return r.status_code, {"raw": r.text}


def post_retry(acct, path, body, tries=3):
    """App-state calls (read/unread/archive) occasionally get a transient server-side 500 from WhatsApp."""
    for i in range(tries):
        code, d = post(acct, path, body)
        if code != 500 or i == tries - 1:
            return code, d
        time.sleep(5)


def get(acct, path, **params):
    r = api(acct, "GET", path, params=params)
    try:
        return r.status_code, r.json()
    except ValueError:
        return r.status_code, {"raw": r.text}


def eventually(fn, timeout=60, step=3):
    """Poll fn until it returns something truthy; return that value (or None on timeout)."""
    end = time.time() + timeout
    while time.time() < end:
        try:
            v = fn()
        except Exception:
            v = None
        if v:
            return v
        time.sleep(step)
    return None


def find_msg(acct, text, from_me=None):
    code, d = post(acct, "/messages", {"query": text, "limit": 10})
    for m in d.get("messages", []) if code == 200 else []:
        if text in (m.get("content") or "") and (from_me is None or m["is_from_me"] == from_me):
            return m
    return None


def send(acct, to, text="", media=""):
    time.sleep(SEND_GAP)
    body = {"recipient": to, "message": text}
    if media:
        body["media_path"] = media
    return post(acct, "/send", body)


PNG = base64.b64decode("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")


def sha(path):
    with open(path, "rb") as f:
        return hashlib.sha256(f.read()).hexdigest()


class E2E(unittest.TestCase):
    state = {}

    @classmethod
    def setUpClass(cls):
        os.makedirs(MEDIA, exist_ok=True)
        with open(os.path.join(MEDIA, f"{TAG}.png"), "wb") as f:
            f.write(PNG)
        with open(os.path.join(MEDIA, f"{TAG}.pdf"), "wb") as f:
            f.write(
            b"%PDF-1.1\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n"
            b"3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\ntrailer<</Root 1 0 R/Size 4>>\n%%EOF\n")
        cmd = ["ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=2",
               "-c:a", "libopus", media_path(f"{TAG}.ogg")]
        subprocess.run(cmd if MODE == "native" else COMPOSE + ["exec", "-T", "a"] + cmd, check=True)

    def need(self, key):
        v = self.state.get(key)
        if not v:
            self.skipTest(f"prerequisite '{key}' not available (earlier test failed)")
        return v

    # ---- 00 auth and pairing ------------------------------------------------
    def test_00_auth_guard(self):
        for acct in "ab":
            self.assertEqual(api(acct, "POST", "/chats", auth=False, json={"limit": 1}).status_code, 401)
            self.assertEqual(requests.get(f"http://127.0.0.1:{PORT[acct]}/qr", timeout=10).status_code, 200)

    def test_01_both_connected(self):
        for acct in "ab":
            ok = eventually(lambda: "WhatsApp connected" in requests.get(
                f"http://127.0.0.1:{PORT[acct]}/qr", timeout=10).text, timeout=90)
            self.assertTrue(ok, f"account {acct} not connected (scan its QR at /qr)")

    # ---- 02 contact lookups --------------------------------------------------
    def test_02_is_on_whatsapp(self):
        code, d = post("a", "/is_on_whatsapp", {"phones": [PHONE["b"], "559999999999"]})
        self.assertEqual(code, 200, d)
        res = {r["query"].lstrip("+"): r["is_in"] for r in d.get("results", [])}
        self.assertTrue(res.get(PHONE["b"]), d)
        self.assertFalse(res.get("559999999999", False), d)

    def test_03_resolve_contact(self):
        code, d = get("a", "/resolve_contact", phone=PHONE["b"])
        self.assertEqual(code, 200, d)
        self.assertTrue(d.get("success"), d)

    # ---- 04 text messages ----------------------------------------------------
    def test_04_send_text_a_to_b(self):
        text = f"{TAG} hello from A"
        code, d = send("a", PHONE["b"], text)
        self.assertEqual((code, d.get("success")), (200, True), d)
        sent = eventually(lambda: find_msg("a", text, from_me=True), timeout=20)
        got = eventually(lambda: find_msg("b", text, from_me=False))
        self.assertTrue(sent, "A did not store its own message")
        self.assertTrue(got, "B did not receive the message")
        self.state.update(a_msg=sent, b_msg=got, a_chat_b=sent["chat_jid"], b_chat_a=got["chat_jid"], text=text)

    def test_05_send_text_b_to_a(self):
        text = f"{TAG} hello from B"
        code, d = send("b", PHONE["a"], text)
        self.assertEqual((code, d.get("success")), (200, True), d)
        self.assertTrue(eventually(lambda: find_msg("a", text, from_me=False)), "A did not receive the message")

    # ---- 06 actions on a message ---------------------------------------------
    def test_06_react(self):
        m, chat = self.need("a_msg"), self.need("a_chat_b")
        code, d = post("a", "/react", {"chat_jid": chat, "message_id": m["id"], "emoji": "👍", "from_me": True})
        self.assertEqual((code, d.get("success")), (200, True), d)
        code, d = post("b", "/react", {"chat_jid": self.need("b_chat_a"), "message_id": self.need("b_msg")["id"],
                                       "emoji": "❤️", "from_me": False})
        self.assertEqual((code, d.get("success")), (200, True), d)

    # KNOWN GAP: the bridge does not apply incoming edits (ProtocolMessage MESSAGE_EDIT) to the stored
    # message, so the receiver keeps the original text. Drop this decorator when that is implemented.
    @unittest.expectedFailure
    def test_07_edit(self):
        m, chat = self.need("a_msg"), self.need("a_chat_b")
        new = f"{TAG} edited"
        code, d = post("a", "/edit", {"chat_jid": chat, "message_id": m["id"], "new_text": new, "from_me": True})
        self.assertEqual((code, d.get("success")), (200, True), d)
        self.assertTrue(eventually(lambda: find_msg("b", new)), "B never saw the edited text")

    def test_08_chat_presence_read_unread_archive(self):
        chat_b = self.need("b_chat_a")
        for state in ("composing", "paused"):
            code, d = post("a", "/chat_presence", {"chat_jid": self.need("a_chat_b"), "state": state, "media": ""})
            self.assertEqual((code, d.get("success")), (200, True), d)

        def app_state(path, body):
            code, d = post_retry("b", path, body)
            msg = str(d.get("message", ""))
            if code == 500 and "app state" in msg.lower() and ("conflict" in msg or "internal-server-error" in msg):
                self.skipTest(f"WhatsApp-side app-state error, not our code: {msg[:140]}")
            self.assertEqual((code, d.get("success")), (200, True), d)

        app_state("/mark_chat_read", {"chat_jid": chat_b, "message_ids": [self.need("b_msg")["id"]], "sender_jid": chat_b})
        app_state("/mark_chat_unread", {"chat_jid": chat_b})
        for archive in (True, False):
            app_state("/archive_chat", {"chat_jid": chat_b, "archive": archive})

    # ---- 09 media ------------------------------------------------------------
    def _media_ids(self):
        chat = self.need("b_chat_a")
        code, d = post("b", "/messages", {"chat_jid": chat, "limit": 50})
        return {m["id"]: m for m in d.get("messages", []) if m.get("media_type")} if code == 200 else {}

    def _media_roundtrip(self, filename, label, media_type):
        before = set(self._media_ids())
        code, d = send("a", PHONE["b"], f"{TAG} {label}", media=media_path(filename))
        self.assertEqual((code, d.get("success")), (200, True), d)
        got = eventually(lambda: next((m for i, m in self._media_ids().items()
                                       if i not in before and m["media_type"] == media_type and not m["is_from_me"]), None))
        self.assertTrue(got, f"B did not receive the {label}")
        code, d = post("b", "/download", {"message_id": got["id"], "chat_jid": got["chat_jid"]})
        self.assertEqual((code, d.get("success")), (200, True), d)
        base = os.path.basename(d.get("path") or d.get("filename") or "")
        files = glob.glob(os.path.join(HERE, ".data", "b", "**", base), recursive=True)
        self.assertTrue(files, f"downloaded file {base!r} not found under .data/b")
        return files[0], got

    def test_09_image(self):
        path, _ = self._media_roundtrip(f"{TAG}.png", "image", "image")
        self.assertEqual(sha(path), sha(os.path.join(MEDIA, f"{TAG}.png")), "downloaded image differs from sent")

    def test_10_document(self):
        path, _ = self._media_roundtrip(f"{TAG}.pdf", "document", "document")
        self.assertEqual(sha(path), sha(os.path.join(MEDIA, f"{TAG}.pdf")), "downloaded pdf differs from sent")

    def test_11_audio(self):
        path, got = self._media_roundtrip(f"{TAG}.ogg", "audio", "audio")
        self.assertEqual(got["media_type"], "audio")
        self.assertGreater(os.path.getsize(path), 500)

    # ---- 12 reads --------------------------------------------------------------
    def test_12_read_endpoints(self):
        chat_a, chat_b, m = self.need("a_chat_b"), self.need("b_chat_a"), self.need("b_msg")
        code, d = post("a", "/chats", {"limit": 50})
        self.assertEqual(code, 200)
        jids = [c["jid"] for c in d["chats"]]
        self.assertIn(chat_a, jids)
        self.assertFalse([j for j in jids if j.endswith("@lid")], "LID chats were not migrated to phone JIDs")
        code, d = post("a", "/chat", {"chat_jid": chat_a, "include_last_message": False})
        self.assertEqual((code, d["chat"]["jid"]), (200, chat_a))
        code, d = post("a", "/chat/by_contact", {"sender_phone_number": PHONE["b"]})
        self.assertEqual((code, (d.get("chat") or {}).get("jid")), (200, chat_a))
        code, d = post("b", "/message_context", {"message_id": m["id"], "before": 2, "after": 2})
        self.assertEqual((code, d["message"]["id"]), (200, m["id"]))
        code, d = post("a", "/contacts/chats", {"jid": chat_a, "limit": 5})
        self.assertEqual(code, 200)
        self.assertIn(chat_a, [c["jid"] for c in d["chats"]])
        code, d = post("a", "/contacts/last_interaction", {"jid": chat_a})
        self.assertEqual(code, 200)
        self.assertTrue(d["message"])
        code, d = post("a", "/sender_name", {"sender_jid": chat_a})
        self.assertEqual(code, 200, d)
        code, d = post("a", "/contacts/search", {"query": PHONE["b"][-6:]})
        self.assertEqual(code, 200)
        self.assertTrue(d["contacts"], "contacts/search found nobody")
        code, d = get("a", "/search_contacts", query=PHONE["b"][-6:])
        self.assertEqual(code, 200, d)

    # ---- 13 groups --------------------------------------------------------------
    def test_13_group_lifecycle(self):
        name = f"{TAG} group"
        time.sleep(SEND_GAP)
        code, d = post("a", "/create_group", {"name": name, "participants": [PHONE["b"]]})
        self.assertEqual((code, d.get("success")), (200, True), d)
        gjid = d["jid"]
        self.state["group"] = gjid
        code, d = get("a", "/group_info", jid=gjid)
        self.assertEqual(code, 200, d)
        self.assertIn(name, json.dumps(d))
        text = f"{TAG} group hello"
        code, d = send("a", gjid, text)
        self.assertEqual((code, d.get("success")), (200, True), d)
        self.assertTrue(eventually(lambda: find_msg("b", text, from_me=False)), "B did not get the group message")
        # settings round trip, read back through group_info
        time.sleep(SEND_GAP)
        new_name, topic = f"{TAG} renamed", f"{TAG} topic"
        code, d = post("a", "/group_settings", {"group_jid": gjid, "name": new_name, "topic": topic,
                                                "announce": True, "locked": True})
        self.assertEqual((code, d.get("success")), (200, True), d)

        def settings_applied():
            c, i = get("a", "/group_info", jid=gjid)
            return c == 200 and (i.get("name"), i.get("topic"), i.get("announce"), i.get("locked")) == (new_name, topic, True, True)
        self.assertTrue(eventually(settings_applied, timeout=30), "group settings not read back")
        code, d = post("a", "/group_settings", {"group_jid": gjid, "announce": False, "locked": False})
        self.assertEqual((code, d.get("success")), (200, True), d)
        self.assertEqual(post("a", "/group_settings", {"group_jid": gjid})[0], 400)
        # invite link: shape, stable on re-get, different after reset
        code, d = get("a", "/group_invite", group_jid=gjid)
        self.assertEqual((code, d.get("success")), (200, True), d)
        link = d["link"]
        self.assertRegex(link, r"^https://chat\.whatsapp\.com/[A-Za-z0-9_-]{10,}$")
        time.sleep(SEND_GAP)
        code, d = post("a", "/group_invite_reset", {"group_jid": gjid})
        self.assertEqual((code, d.get("success")), (200, True), d)
        self.assertRegex(d["link"], r"^https://chat\.whatsapp\.com/[A-Za-z0-9_-]{10,}$")
        self.assertNotEqual(d["link"], link, "reset did not rotate the invite link")
        # B is already a member (joined at creation), so no live join: only validate the input path
        self.assertEqual(post("b", "/group_join", {"link": "not a link"})[0], 400)
        for action in ("promote", "demote", "remove", "add"):
            time.sleep(SEND_GAP)
            code, d = post("a", "/group_participants", {"group_jid": gjid, "participants": [PHONE["b"]], "action": action})
            self.assertEqual(code, 200, (action, d))
        code, d = post("b", "/leave_group", {"jid": gjid})
        self.assertEqual((code, d.get("success")), (200, True), d)
        code, d = post("a", "/leave_group", {"jid": gjid})
        self.assertEqual((code, d.get("success")), (200, True), d)

    # ---- 14 revoke (last, it deletes a message) ----------------------------------
    def test_14_revoke(self):
        text = f"{TAG} to be revoked"
        code, d = send("a", PHONE["b"], text)
        self.assertEqual((code, d.get("success")), (200, True), d)
        m = eventually(lambda: find_msg("a", text, from_me=True), timeout=20)
        self.assertTrue(m)
        code, d = post("a", "/revoke", {"chat_jid": m["chat_jid"], "message_id": m["id"], "from_me": True})
        self.assertEqual((code, d.get("success")), (200, True), d)

    # ---- 15 restart persistence ---------------------------------------------------
    def test_15_restart_keeps_session(self):
        restart("a")
        ok = eventually(lambda: "WhatsApp connected" in requests.get(
            f"http://127.0.0.1:{PORT['a']}/qr", timeout=10).text, timeout=90)
        self.assertTrue(ok, "A asked for a new QR after restart")
        code, d = post("a", "/chats", {"limit": 5})
        self.assertTrue(code == 200 and d["chats"], "history lost after restart")

    # ---- 16 MCP layer ---------------------------------------------------------------
    def test_16_mcp_tools_registered_and_wired(self):
        from mcp import ClientSession, StdioServerParameters
        from mcp.client.stdio import stdio_client

        server_dir = os.path.join(HERE, "..", "..", "whatsapp-mcp-server")
        params = StdioServerParameters(
            command="uv", args=["run", "--project", server_dir, "python", os.path.join(server_dir, "main.py")],
            env={**os.environ, "WHATSAPP_API_BASE_URL": f"http://127.0.0.1:{PORT['a']}/api",
                 "WHATSAPP_API_AUTH_TOKEN": TOKEN})

        async def run():
            async with stdio_client(params) as (r, w):
                async with ClientSession(r, w) as s:
                    await s.initialize()
                    names = {t.name for t in (await s.list_tools()).tools}
                    chats = await s.call_tool("list_chats", {"limit": 3})
                    chk = await s.call_tool("check_whatsapp", {"phones": [PHONE["b"]]})
                    return names, chats, chk

        names, chats, chk = asyncio.run(run())
        self.assertEqual(names, MCP_TOOLS, f"tool set changed: +{names - MCP_TOOLS} -{MCP_TOOLS - names}")
        self.assertFalse(chats.isError)
        self.assertFalse(chk.isError)


if __name__ == "__main__":
    unittest.main(verbosity=2)
