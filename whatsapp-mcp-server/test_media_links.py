"""Download and upload links for the web connector (media_links.py), through the real HTTP app.

Run: python -m unittest test_media_links -v"""

import asyncio
import logging
import os
import shutil
import stat
import tempfile
import time
import unittest
import urllib.parse
from unittest import mock

from starlette.testclient import TestClient

import media_files
import media_links
from test_mcp_modes import load_main

BASE = "https://connector.example.test"
# MCP_PUBLIC_URL comes with OAuth (oauth_from_env): the connector setup that serves web chats.
HTTP_ENV = {"MCP_TRANSPORT": "streamable-http", "MCP_PUBLIC_URL": BASE, "MCP_AUTH_TOKEN": "t" * 32,
            "MCP_OAUTH_PASSPHRASE": "p" * 24}


def reset_links():
    with media_links._lock:
        media_links._downloads.clear()
        for u in media_links._uploads.values():
            media_links._remove(u.get("path"))
        media_links._uploads.clear()


class Links(unittest.TestCase):
    def setUp(self):
        reset_links()
        self.addCleanup(reset_links)
        db = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, db, True)
        self.main = load_main(**HTTP_ENV, MCP_OAUTH_DB=os.path.join(db, "oauth.db"))
        self.addCleanup(lambda: load_main())  # links off again for the other test modules
        self.client = TestClient(self.main.build_http_app())
        self.work = tempfile.mkdtemp()
        self.addCleanup(shutil.rmtree, self.work, True)

    def path_of(self, url):
        self.assertTrue(url.startswith(BASE + media_links.PREFIX), url)
        return url[len(BASE):]

    def tools(self):
        return {t.name for t in asyncio.run(self.main.mcp.list_tools())}

    # ---- download --------------------------------------------------------------------------
    def test_download_link_serves_that_file_only_until_it_expires(self):
        path = os.path.join(self.work, "ID_photo.jpg")
        with open(path, "wb") as f:
            f.write(b"jpeg bytes")
        link = self.path_of(media_links.download_link(path, "ID_photo.jpg"))
        token = link.rsplit("/", 1)[1]
        self.assertGreaterEqual(len(token), 22)  # 16 random bytes, url-safe base64
        for _ in range(2):  # a browser may fetch twice
            r = self.client.get(link)  # no Authorization header: the token is the credential
            self.assertEqual(r.status_code, 200)
            self.assertEqual(r.content, b"jpeg bytes")
        self.assertTrue(r.headers["content-disposition"].startswith("attachment"))
        self.assertEqual(r.headers["content-type"], "application/octet-stream")
        self.assertEqual(r.headers["x-content-type-options"], "nosniff")
        self.assertEqual(r.headers["cache-control"], "no-store")
        self.assertEqual(self.client.get(media_links.PREFIX + "d/" + "x" * 22).status_code, 404)
        self.assertNotEqual(self.client.get(media_links.PREFIX + "d/../../mcp").status_code, 200)
        with mock.patch.object(media_links.time, "time", return_value=time.time() + media_links.LINK_TTL + 1):
            self.assertEqual(self.client.get(link).status_code, 404)

    def test_other_paths_still_need_the_token(self):
        self.assertEqual(self.client.post("/mcp", json={}).status_code, 401)

    def test_download_media_adds_a_link_and_a_small_image(self):
        path = os.path.join(self.work, "ID_photo.jpg")
        with open(path, "wb") as f:
            f.write(b"\xff\xd8\xff jpeg")
        with mock.patch.object(self.main, "whatsapp_download_media", return_value=(path, "ok")):
            result = self.main.download_media("ID", "chat@g.us")
        info, image = result
        self.assertEqual(info["file_path"], path)
        self.assertEqual(self.client.get(self.path_of(info["download_url"])).content, b"\xff\xd8\xff jpeg")
        self.assertEqual(info["download_url_expires_in_minutes"], 15)
        self.assertEqual(image.to_image_content().mimeType, "image/jpeg")
        big = os.path.join(self.work, "ID_big.png")
        with open(big, "wb") as f:
            f.truncate(self.main._INLINE_MAX + 1)
        with mock.patch.object(self.main, "whatsapp_download_media", return_value=(big, "ok")):
            self.assertIsInstance(self.main.download_media("ID", "chat@g.us"), dict)

    def test_stdio_has_no_links(self):
        stdio = load_main()
        self.assertFalse(media_links.enabled())
        self.assertNotIn("create_upload_link", {t.name for t in asyncio.run(stdio.mcp.list_tools())})
        path = os.path.join(self.work, "ID_photo.jpg")
        with open(path, "wb") as f:
            f.write(b"x")
        with mock.patch.object(stdio, "whatsapp_download_media", return_value=(path, "ok")):
            self.assertNotIn("download_url", stdio.download_media("ID", "chat@g.us"))
        self.assertEqual(media_links.configured({"MCP_TRANSPORT": "streamable-http", "MCP_PUBLIC_URL": "http://x"}), "")

    # ---- upload ----------------------------------------------------------------------------
    def upload(self, link, data, name="clip.mp4"):
        return self.client.post(link, content=data, headers={"X-Filename": urllib.parse.quote(name)})

    def test_upload_once_then_send_once(self):
        self.assertIn("create_upload_link", self.tools())
        made = self.main.create_upload_link()
        link = self.path_of(made["upload_url"])
        page = self.client.get(link)
        self.assertEqual(page.status_code, 200)
        self.assertIn("script-src 'nonce-", page.headers["content-security-policy"])
        self.assertIn("frame-ancestors 'none'", page.headers["content-security-policy"])
        self.assertEqual(self.main.send_file("123", upload_id=made["upload_id"])["success"], False)  # not uploaded yet

        self.assertEqual(self.upload(link, b"video bytes", "férias.mp4").status_code, 200)
        self.assertEqual(self.upload(link, b"other").status_code, 404)  # single use
        self.assertEqual(self.client.get(link).status_code, 404)
        [stored] = [u for u in media_links._uploads.values()]
        folder = os.path.dirname(stored["path"])
        self.assertEqual(stat.S_IMODE(os.stat(folder).st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(os.stat(stored["path"]).st_mode), 0o600)

        sent = []
        with mock.patch.object(self.main, "whatsapp_send_uploaded_file",
                               side_effect=lambda r, p, n: (sent.append((r, open(p, "rb").read(), n)), (True, "sent"))[1]):
            self.assertTrue(self.main.send_file("123", upload_id=made["upload_id"])["success"])
        self.assertEqual(sent, [("123", b"video bytes", "férias.mp4")])
        self.assertFalse(self.main.send_file("123", upload_id=made["upload_id"])["success"])
        self.assertFalse(self.main.send_file("123", media_path="/x", upload_id=made["upload_id"])["success"])

    def test_sending_deletes_the_upload(self):
        made = self.main.create_upload_link()
        self.upload(self.path_of(made["upload_url"]), b"doc")
        path, name = media_links.take_upload(made["upload_id"])
        with mock.patch("whatsapp._send_media", return_value=(True, "sent")) as send:
            self.assertTrue(self.main.whatsapp_send_uploaded_file("123", path, name)[0])
        self.assertEqual(send.call_args[0][:3], ("123", "clip.mp4", b"doc"))
        self.assertFalse(os.path.exists(path))

    def test_upload_refusals_leave_nothing_on_disk(self):
        before = set(os.listdir(media_links.upload_dir()))
        with mock.patch.object(media_files, "MAX_MEDIA_BYTES", 10):
            made = self.main.create_upload_link()
            link = self.path_of(made["upload_url"])
            self.assertEqual(self.upload(link, b"x", "../../etc/passwd").status_code, 400)
            self.assertEqual(self.upload(link, b"x", "a\nb").status_code, 400)
            self.assertEqual(self.upload(link, b"x" * 11).status_code, 413)
            self.assertEqual(self.upload(link, b"x").status_code, 404)  # the failed upload used the link
            empty = self.path_of(self.main.create_upload_link()["upload_url"])
            self.assertEqual(self.upload(empty, b"").status_code, 400)
        self.assertEqual(set(os.listdir(media_links.upload_dir())), before)

    def test_pending_uploads_are_capped_and_expire(self):
        made = [self.main.create_upload_link() for _ in range(media_links.MAX_PENDING_UPLOADS)]
        self.assertFalse(self.main.create_upload_link()["success"])
        self.upload(self.path_of(made[0]["upload_url"]), b"doc")
        [path] = [u["path"] for u in media_links._uploads.values() if u.get("path")]
        media_links._sweep(now=time.time() + media_links.LINK_TTL + 1)
        self.assertFalse(os.path.exists(path))
        self.assertEqual(media_links._uploads, {})
        self.assertTrue(self.main.create_upload_link()["success"])

    def test_links_never_reach_the_access_log(self):
        record = logging.LogRecord("uvicorn.access", logging.INFO, __file__, 1, '%s - "%s %s HTTP/%s" %d',
                                   ("1.2.3.4:5", "GET", "/media/d/SECRETTOKEN", "1.1", 200), None)
        for f in logging.getLogger("uvicorn.access").filters:
            f.filter(record)
        self.assertNotIn("SECRETTOKEN", record.getMessage())
        self.assertIn("/media/<link>", record.getMessage())


if __name__ == "__main__":
    unittest.main()
