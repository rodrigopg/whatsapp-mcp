"""send_file / send_audio_message / download_media when the bridge runs on another machine.

The client "machine" and the bridge "machine" are two separate temp folders: the fake bridge only
knows its own folder, so a path from the client reaching it would be a bug.

Run: python -m unittest test_remote_media -v"""

import json
import os
import stat
import tempfile
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, HTTPServer
from unittest.mock import patch

import media_files
import whatsapp


class FakeBridge:
    """A bridge on another machine. old=True: a bridge from before /api/send/file existed."""

    def __init__(self, root, old=False, filename="photo.jpg"):
        self.root, self.old, self.filename = root, old, filename
        self.requests = []
        outer = self

        class Handler(BaseHTTPRequestHandler):
            def do_POST(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
                path, _, query = self.path.partition("?")
                outer.requests.append({"path": path, "query": urllib.parse.parse_qs(query), "body": body})
                status, headers, payload = outer.respond(path, body)
                self.send_response(status)
                for k, v in headers.items():
                    self.send_header(k, v)
                self.send_header("Content-Length", str(len(payload)))
                self.end_headers()
                self.wfile.write(payload)

            def log_message(self, *args):
                pass

        self.httpd = HTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)

    def respond(self, path, body):
        ok = {"Content-Type": "application/json"}, json.dumps({"success": True, "message": "Message sent"}).encode()
        if path == "/api/send/file" and not self.old:
            return (200, *ok)
        if path == "/api/download/file" and not self.old:
            return 200, {"Content-Type": "application/octet-stream",
                         "X-Media-Filename": urllib.parse.quote(self.filename)}, b"jpeg bytes"
        if path == "/api/send" and self.old:
            return (200, *ok)
        if path == "/api/download" and self.old:
            return 200, {"Content-Type": "application/json"}, json.dumps(
                {"success": True, "path": os.path.join(self.root, "store", "chat", "ID_photo.jpg")}).encode()
        return 404, {"Content-Type": "text/plain"}, b"404 page not found"

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *exc):
        self.httpd.shutdown()
        self.httpd.server_close()


class RemoteMedia(unittest.TestCase):
    def setUp(self):
        self.client = os.path.realpath(tempfile.mkdtemp(prefix="client-"))
        self.bridge_root = os.path.realpath(tempfile.mkdtemp(prefix="bridge-"))
        self.addCleanup(lambda: __import__("shutil").rmtree(self.client, ignore_errors=True))
        self.addCleanup(lambda: __import__("shutil").rmtree(self.bridge_root, ignore_errors=True))
        self.home = os.path.join(self.client, "home")
        self.downloads = os.path.join(self.home, "Downloads")
        self.tmp = os.path.join(self.client, "tmp")
        for d in (self.downloads, self.tmp, os.path.join(self.home, ".ssh")):
            os.makedirs(d)
        env = patch.dict(os.environ, {"HOME": self.home, "MEDIA_ALLOWED_ROOTS": "", "WHATSAPP_DOWNLOAD_DIR": "",
                                      "XDG_CONFIG_HOME": os.path.join(self.home, ".config")})
        env.start()
        self.addCleanup(env.stop)
        roots = patch.object(media_files, "default_roots", lambda: [self.tmp, self.downloads])
        roots.start()
        self.addCleanup(roots.stop)

    def write(self, path, data=b"file bytes"):
        os.makedirs(os.path.dirname(path), exist_ok=True)
        with open(path, "wb") as f:
            f.write(data)
        return path

    def bridge(self, **kw):
        server = FakeBridge(self.bridge_root, **kw)
        base = patch.object(whatsapp, "WHATSAPP_API_BASE_URL", f"http://127.0.0.1:{server.httpd.server_address[1]}/api")
        base.start()
        self.addCleanup(base.stop)
        return server

    def assertNoClientPath(self, server):
        for r in server.requests:
            self.assertNotIn(self.client.encode(), r["body"] + json.dumps(r["query"]).encode())

    # ---- sending -----------------------------------------------------------------------------
    def test_send_file_uploads_the_bytes(self):
        path = self.write(os.path.join(self.downloads, "clip.mp4"), b"\x00mp4 bytes")
        with self.bridge() as server:
            ok, message = whatsapp.send_file("123456789@s.whatsapp.net", path)
        self.assertTrue(ok, message)
        [r] = server.requests
        self.assertEqual(r["path"], "/api/send/file")
        self.assertEqual(r["body"], b"\x00mp4 bytes")
        self.assertEqual(r["query"]["filename"], ["clip.mp4"])
        self.assertEqual(r["query"]["recipient"], ["123456789@s.whatsapp.net"])
        self.assertNoClientPath(server)

    def test_refused_files_never_leave_this_machine(self):
        secret = self.write(os.path.join(self.home, ".ssh", "id_ed25519"), b"PRIVATE KEY")
        os.symlink(secret, os.path.join(self.downloads, "innocent.jpg"))
        os.symlink(os.path.join(self.home, ".ssh"), os.path.join(self.downloads, "keys"))
        self.write(os.path.join(self.downloads, ".env"), b"TOKEN=1")
        self.write(os.path.join(self.tmp, ".cache", "k"), b"hidden")
        self.write(os.path.join(self.home, "notes.txt"), b"private")
        os.mkdir(os.path.join(self.downloads, "folder"))
        os.mkfifo(os.path.join(self.downloads, "pipe.ogg"))
        big = os.path.join(self.downloads, "big.mp4")
        with open(big, "wb") as f:
            f.truncate(media_files.MAX_MEDIA_BYTES + 1)
        cases = [secret, os.path.join(self.downloads, "innocent.jpg"), os.path.join(self.downloads, "keys", "id_ed25519"),
                 os.path.join(self.downloads, ".env"), os.path.join(self.tmp, ".cache", "k"),
                 os.path.join(self.home, "notes.txt"), os.path.join(self.downloads, "folder"),
                 os.path.join(self.downloads, "pipe.ogg"), big, os.path.join(self.downloads, "missing.pdf"),
                 os.path.join(self.downloads, "..", ".ssh", "id_ed25519")]
        with self.bridge() as server:
            for path in cases:
                for send in (whatsapp.send_file, whatsapp.send_audio_message):
                    with self.subTest(path=os.path.relpath(path, self.client), send=send.__name__):
                        ok, message = send("123", path)
                        self.assertFalse(ok)
                        self.assertNotIn(self.client, message)
        self.assertEqual(server.requests, [])

    def test_configured_roots_replace_the_defaults(self):
        mine = self.write(os.path.join(self.client, "work", ".hidden-ok", "a.pdf"))
        downloaded = self.write(os.path.join(self.downloads, "b.pdf"))
        with self.bridge() as server, patch.dict(os.environ, {"MEDIA_ALLOWED_ROOTS": os.path.join(self.client, "work")}):
            self.assertTrue(whatsapp.send_file("123", mine)[0])
            ok, message = whatsapp.send_file("123", downloaded)
        self.assertFalse(ok)
        self.assertIn("MEDIA_ALLOWED_ROOTS", message)
        self.assertEqual(len(server.requests), 1)

    def test_a_component_swapped_after_the_check_is_refused(self):
        target = self.write(os.path.join(self.downloads, "d", "a.pdf"))
        other = os.stat(self.write(os.path.join(self.downloads, "other.pdf")))
        real_stat = os.stat

        def swapped(path, *a, **kw):
            st = real_stat(path, *a, **kw)
            return other if kw.get("dir_fd") is not None and path == "a.pdf" else st

        with patch.object(media_files.os, "stat", swapped), self.assertRaises(media_files.MediaError) as e:
            media_files.read_outbound(target)
        self.assertIn("changed", str(e.exception))

    def test_a_symlink_inside_the_walk_is_never_followed(self):
        self.write(os.path.join(self.home, ".ssh", "id_ed25519"), b"PRIVATE KEY")
        os.symlink(os.path.join(self.home, ".ssh"), os.path.join(self.downloads, "keys"))
        with self.assertRaises(media_files.MediaError):
            media_files._open_walk(self.downloads, ["keys", "id_ed25519"], True)

    def test_audio_is_checked_before_ffmpeg_reads_it(self):
        secret = self.write(os.path.join(self.home, ".ssh", "voice.wav"), b"RIFF secret")
        with self.bridge() as server, patch.object(whatsapp.audio, "convert_to_opus_ogg_temp") as convert:
            self.assertFalse(whatsapp.send_audio_message("123", secret)[0])
        convert.assert_not_called()
        self.assertEqual(server.requests, [])

    # ---- downloading -------------------------------------------------------------------------
    def test_download_lands_on_this_machine(self):
        with self.bridge() as server:
            path, message = whatsapp.download_media("ABC123", "123456789@s.whatsapp.net")
        self.assertEqual(message, "Media downloaded successfully")
        self.assertEqual(path, os.path.join(self.downloads, "whatsapp", "ABC123_photo.jpg"))
        with open(path, "rb") as f:
            self.assertEqual(f.read(), b"jpeg bytes")
        self.assertEqual(stat.S_IMODE(os.stat(os.path.dirname(path)).st_mode), 0o700)
        self.assertEqual(stat.S_IMODE(os.stat(path).st_mode), 0o600)
        self.assertEqual(os.listdir(os.path.dirname(path)), ["ABC123_photo.jpg"])
        self.assertEqual(server.requests[0]["path"], "/api/download/file")

        # Forwarding what was downloaded works, also with other roots configured.
        with self.bridge() as server, patch.dict(os.environ, {"MEDIA_ALLOWED_ROOTS": self.tmp}):
            self.assertTrue(whatsapp.send_file("123", path)[0])
        self.assertEqual(server.requests[0]["body"], b"jpeg bytes")

    def test_download_folder_is_configurable(self):
        folder = os.path.join(self.client, "media")
        with self.bridge(), patch.dict(os.environ, {"WHATSAPP_DOWNLOAD_DIR": folder}):
            path, _ = whatsapp.download_media("ABC123", "chat@g.us")
        self.assertEqual(path, os.path.join(folder, "ABC123_photo.jpg"))

    def test_download_folder_never_the_home(self):
        for folder in (self.home, self.client):
            with self.subTest(folder=folder), self.bridge(), patch.dict(os.environ, {"WHATSAPP_DOWNLOAD_DIR": folder}):
                path, message = whatsapp.download_media("ABC123", "chat@g.us")
                self.assertIsNone(path)
                self.assertIn("home", message)

    def test_unsafe_names_from_whatsapp_are_refused(self):
        for name in ("../../evil.sh", "a/b.jpg", "a\\b.jpg", "..", "", "x\nb.jpg", "a\x00b"):
            with self.subTest(name=name), self.bridge(filename=name):
                path, message = whatsapp.download_media("ABC123", "chat@g.us")
                self.assertIsNone(path)
        for message_id in ("../x", ".hidden", "a/b"):
            with self.subTest(message_id=message_id), self.bridge():
                self.assertIsNone(whatsapp.download_media(message_id, "chat@g.us")[0])
        self.assertFalse(os.path.exists(os.path.join(self.downloads, "evil.sh")))
        leftovers = os.listdir(os.path.join(self.downloads, "whatsapp")) if os.path.isdir(os.path.join(self.downloads, "whatsapp")) else []
        self.assertEqual(leftovers, [])

    # ---- older bridge ------------------------------------------------------------------------
    def test_old_bridge_gets_the_old_calls(self):
        path = self.write(os.path.join(self.downloads, "doc.pdf"))
        with self.bridge(old=True) as server:
            self.assertTrue(whatsapp.send_file("123", path)[0])
            got, _ = whatsapp.download_media("ID", "chat")
        self.assertEqual([r["path"] for r in server.requests], ["/api/send/file", "/api/send", "/api/download/file", "/api/download"])
        self.assertEqual(json.loads(server.requests[1]["body"])["media_path"], path)
        self.assertTrue(got.startswith(self.bridge_root))


if __name__ == "__main__":
    unittest.main()
