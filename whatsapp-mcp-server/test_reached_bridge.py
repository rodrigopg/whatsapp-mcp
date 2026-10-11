"""The transcription sweep may retire an audio for good (SENTINEL_UNAVAILABLE,
never retried) only when the bridge reports the actual CDN signal: media gone
(403/404/410), surfaced as reason == "media_gone" in the download response.
Any other failure - 401 from a misconfigured token, 5xx, a non-JSON body, a
connection error - says nothing about the media and must leave it for the next
sweep.

Run: python -m unittest test_reached_bridge -v"""

import os
import sqlite3
import tempfile
import unittest
import unittest.mock
from datetime import datetime, timedelta, timezone

import requests

import transcribe


class FakeResponse:
    def __init__(self, status_code, json_body=None, text=""):
        self.status_code = status_code
        self._json = json_body
        self.text = text

    def json(self):
        if self._json is None:
            raise ValueError("not json")
        return self._json


GONE = FakeResponse(500, {
    "success": False,
    "message": "Failed to download media: failed to download media: download failed with status code 410",
    "reason": "media_gone",
})
UNAUTHORIZED_PLAIN = FakeResponse(401, text="Unauthorized\n")
NOT_JSON_200 = FakeResponse(200, text="<html>proxy page</html>")
SERVER_ERROR_JSON = FakeResponse(500, {"success": False, "message": "Failed to download media: boom"})
SERVER_ERROR_PLAIN = FakeResponse(502, text="Bad Gateway")
JSON_LIST = FakeResponse(500, json_body=["not", "a", "dict"])
CONN_ERROR = requests.ConnectionError("connection refused")
TIMEOUT = requests.Timeout("read timed out")

CASES_NOT_GONE = [
    ("401 plain text", UNAUTHORIZED_PLAIN),
    ("200 non-JSON body", NOT_JSON_200),
    ("500 JSON without the CDN signal", SERVER_ERROR_JSON),
    ("502 plain text", SERVER_ERROR_PLAIN),
    ("JSON body that is not an object", JSON_LIST),
    ("connection error", CONN_ERROR),
    ("timeout", TIMEOUT),
]


def _post(outcome):
    if isinstance(outcome, Exception):
        return unittest.mock.patch.object(transcribe.requests, "post", side_effect=outcome)
    return unittest.mock.patch.object(transcribe.requests, "post", return_value=outcome)


class DownloadMediaGoneTest(unittest.TestCase):
    def test_cdn_signal_is_media_gone(self):
        with _post(GONE):
            path, err, media_gone = transcribe.download("MSG", "chat@s.whatsapp.net")
        self.assertIsNone(path)
        self.assertTrue(media_gone)
        self.assertIn("410", err)

    def test_everything_else_is_not_media_gone(self):
        for name, outcome in CASES_NOT_GONE:
            with self.subTest(name), _post(outcome):
                path, err, media_gone = transcribe.download("MSG", "chat@s.whatsapp.net")
                self.assertIsNone(path)
                self.assertTrue(err)
                self.assertFalse(media_gone)

    def test_success_is_not_media_gone(self):
        ok = FakeResponse(200, {"success": True, "path": "/tmp/x.ogg"})
        with _post(ok):
            path, err, media_gone = transcribe.download("MSG", "chat@s.whatsapp.net")
        self.assertEqual(path, "/tmp/x.ogg")
        self.assertIsNone(err)
        self.assertFalse(media_gone)


class SweepMarkerTest(unittest.TestCase):
    """End to end through main(): what lands in messages.content for an audio
    older than the CDN window, per bridge outcome."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.db = os.path.join(self.tmp.name, "messages.db")
        conn = sqlite3.connect(self.db)
        conn.execute("CREATE TABLE messages (id TEXT, chat_jid TEXT, content TEXT, "
                     "timestamp TEXT, media_type TEXT, file_sha256 BLOB)")
        old = (datetime.now(timezone.utc) - transcribe.CDN_EXPIRY - timedelta(days=5)).isoformat()
        conn.execute("INSERT INTO messages VALUES ('OLDAUDIO', 'chat@s.whatsapp.net', '', ?, 'audio', NULL)",
                     (old,))
        conn.commit()
        conn.close()

    def _run_sweep(self, outcome):
        with unittest.mock.patch.object(transcribe, "DB_PATH", self.db), \
                unittest.mock.patch.object(transcribe, "engine_ready", return_value=(True, "test")), \
                unittest.mock.patch.object(transcribe, "log"), \
                unittest.mock.patch("sys.argv", ["transcribe.py"]), \
                _post(outcome):
            transcribe.main()
        conn = sqlite3.connect(self.db)
        try:
            return conn.execute("SELECT content FROM messages WHERE id='OLDAUDIO'").fetchone()[0]
        finally:
            conn.close()

    def test_old_audio_with_cdn_signal_is_marked_unavailable(self):
        self.assertEqual(self._run_sweep(GONE), transcribe.SENTINEL_UNAVAILABLE)

    def test_old_audio_is_left_for_retry_on_any_other_failure(self):
        for name, outcome in CASES_NOT_GONE:
            with self.subTest(name):
                self.assertEqual(self._run_sweep(outcome), "")


if __name__ == "__main__":
    unittest.main()
