import hashlib
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest
from datetime import datetime, timedelta, timezone
from unittest.mock import Mock, patch

import recover_audios
import transcribe


class TranscribeCacheTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.db = self.root / "messages.db"
        with sqlite3.connect(self.db) as conn:
            conn.execute("CREATE TABLE messages (id TEXT, chat_jid TEXT, content TEXT, "
                         "media_type TEXT, file_sha256 BLOB, file_length INTEGER, timestamp TEXT, "
                         "is_from_me BOOLEAN)")
        for module in (transcribe, recover_audios):
            self.enterContext(patch.object(module, "DB_PATH", str(self.db)))
            self.enterContext(patch.object(module, "log"))
        self.enterContext(patch.object(sys, "argv", ["transcribe.py"]))
        self.enterContext(patch.object(transcribe, "engine_ready", return_value=(True, "test")))
        self.tx = self.enterContext(patch.object(transcribe, "transcribe", return_value="transcript"))
        self.post = self.enterContext(patch.object(transcribe.requests, "post"))
        self.post.side_effect = AssertionError("unexpected bridge request")

    def audio(self, msg_id="audio", data=b"cached audio", sha=True, length=None,
              content="", timestamp="2000-01-01T00:00:00Z"):
        digest = hashlib.sha256(data).digest() if sha else None
        with sqlite3.connect(self.db) as conn:
            conn.execute("INSERT INTO messages VALUES (?, 'chat', ?, 'audio', ?, ?, ?, 0)",
                         (msg_id, content, digest, len(data) if length is None else length, timestamp))
        path = self.root / "chat" / (msg_id + "_audio.ogg")
        path.parent.mkdir(exist_ok=True)
        path.write_bytes(data)
        return path

    def content(self, msg_id="audio"):
        with sqlite3.connect(self.db) as conn:
            return conn.execute("SELECT content FROM messages WHERE id=?", (msg_id,)).fetchone()[0]

    def response(self, status, body):
        self.post.side_effect = None
        self.post.return_value = Mock(status_code=status, json=Mock(return_value=body))

    def test_valid_cache_survives_offline_bridge(self):
        path = self.audio()
        transcribe.main()
        self.assertEqual(path.read_bytes(), b"cached audio")
        self.assertEqual(self.content(), "transcript")
        self.tx.assert_called_once_with(str(path), "audio")
        self.post.assert_not_called()

    def test_cache_without_hash_uses_size_or_nonempty_file(self):
        for length in (len(b"cached audio"), 0, None):
            with self.subTest(length=length):
                msg_id = "audio" + str(length)
                path = self.audio(msg_id, sha=False)
                with sqlite3.connect(self.db) as conn:
                    conn.execute("UPDATE messages SET file_length=? WHERE id=?", (length, msg_id))
                transcribe.main()
                self.assertEqual(self.content(msg_id), "transcript")
                self.assertTrue(path.exists())
        self.post.assert_not_called()

    def test_invalid_cache_is_preserved_and_not_transcribed(self):
        for kind in ("hash", "size", "empty"):
            with self.subTest(kind=kind):
                path = self.audio(kind, sha=(kind == "hash"))
                bad = b"wrong audio!" if kind == "hash" else b"x" if kind == "size" else b""
                path.write_bytes(bad)
                self.response(200, {"success": True, "path": str(path)})
                transcribe.main()
                self.assertEqual(path.read_bytes(), bad)
                self.assertEqual(self.content(kind), "")
        self.tx.assert_not_called()

    def test_failed_redownload_preserves_invalid_cache_for_retry(self):
        path = self.audio()
        path.write_bytes(b"partial")
        self.response(401, {"message": "unauthorized"})
        transcribe.main()
        self.assertEqual(path.read_bytes(), b"partial")
        self.assertEqual(self.content(), "")
        self.tx.assert_not_called()

    def test_bridge_failure_never_marks_old_audio_unavailable(self):
        self.audio().unlink()
        failures = [
            (401, {"message": "unauthorized"}),
            (403, {"message": "forbidden"}),
            (404, {"message": "route missing"}),
            (410, {"message": "gone"}),
            (500, {"success": False, "message": "download failed: HTTP 403"}),
            (500, {"success": False, "reason": "other_error"}),
        ]
        for status, body in failures:
            with self.subTest(status=status, body=body):
                with sqlite3.connect(self.db) as conn:
                    conn.execute("UPDATE messages SET content=''")
                self.response(status, body)
                transcribe.main()
                self.assertEqual(self.content(), "")
        self.post.side_effect = transcribe.requests.ConnectionError("offline")
        transcribe.main()
        self.assertEqual(self.content(), "")
        self.tx.assert_not_called()

    def test_unavailable_requires_media_gone_and_expired_timestamp(self):
        now = datetime.now(timezone.utc)
        for expired in (False, True):
            for reason in (None, "other_error", "media_gone"):
                with self.subTest(expired=expired, reason=reason):
                    with sqlite3.connect(self.db) as conn:
                        conn.execute("DELETE FROM messages")
                    msg_id = f"audio-{expired}-{reason}"
                    age = transcribe.CDN_EXPIRY + timedelta(days=1) if expired else timedelta(days=1)
                    self.audio(msg_id, timestamp=(now - age).isoformat()).unlink()
                    self.response(500, {"success": False, "reason": reason})
                    transcribe.main()
                    expected = transcribe.SENTINEL_UNAVAILABLE if expired and reason == "media_gone" else ""
                    self.assertEqual(self.content(msg_id), expected)
        self.tx.assert_not_called()

    def test_expired_media_gone_is_not_downloaded_on_next_sweep(self):
        self.audio().unlink()
        self.response(500, {"success": False, "reason": "media_gone"})
        transcribe.main()
        transcribe.main()
        self.assertEqual(self.content(), transcribe.SENTINEL_UNAVAILABLE)
        self.post.assert_called_once()

    def test_valid_cache_wins_over_media_gone(self):
        path = self.audio()
        self.response(500, {"success": False, "reason": "media_gone"})
        transcribe.main()
        self.assertEqual(self.content(), "transcript")
        self.assertTrue(path.exists())
        self.post.assert_not_called()

    def test_invalid_json_response_is_retried(self):
        self.audio().unlink()
        self.post.side_effect = None
        self.post.return_value = Mock(status_code=502, text="bad gateway",
                                      json=Mock(side_effect=ValueError))
        transcribe.main()
        self.assertEqual(self.content(), "")

    def test_downloaded_audio_without_hash_still_checks_size(self):
        path = self.audio(sha=False)
        path.unlink()

        def download(*args, **kwargs):
            path.write_bytes(b"truncated")
            return Mock(status_code=200, json=Mock(return_value={"success": True, "path": str(path)}))

        self.post.side_effect = download
        transcribe.main()
        self.assertEqual(self.content(), "")
        self.tx.assert_not_called()

    def test_verified_download_is_transcribed(self):
        path = self.audio()
        path.unlink()

        def download(*args, **kwargs):
            path.write_bytes(b"cached audio")
            return Mock(status_code=200, json=Mock(return_value={"success": True, "path": str(path)}))

        self.post.side_effect = download
        transcribe.main()
        self.assertEqual(self.content(), "transcript")
        self.tx.assert_called_once_with(str(path), "audio")

    def test_recovery_rejects_corrupt_cached_audio(self):
        path = self.audio(content=transcribe.SENTINEL_UNAVAILABLE)
        path.write_bytes(b"wrong audio!")
        self.assertIsNone(recover_audios.recovered_file("chat", "audio"))
        self.assertEqual(path.read_bytes(), b"wrong audio!")

    def test_recovery_checks_size_when_hash_is_missing(self):
        path = self.audio(sha=False, content=transcribe.SENTINEL_UNAVAILABLE)
        self.assertEqual(recover_audios.recovered_file("chat", "audio"), str(path))
        path.write_bytes(b"x")
        self.assertIsNone(recover_audios.recovered_file("chat", "audio"))
        self.assertEqual(path.read_bytes(), b"x")

    def test_timeout_is_per_message_and_keeps_audio(self):
        paths = [self.audio("first", timestamp="2000-01-02T00:00:00Z"), self.audio("second")]
        self.tx.side_effect = [subprocess.TimeoutExpired("whisper", 600), "second transcript"]
        transcribe.main()
        with sqlite3.connect(self.db) as conn:
            contents = [r[0] for r in conn.execute("SELECT content FROM messages ORDER BY rowid")]
        self.assertEqual(contents, ["", "second transcript"])
        self.assertTrue(all(path.exists() for path in paths))
        self.assertEqual(self.tx.call_count, 2)


class TranscriptionTimeoutTest(unittest.TestCase):
    def test_subprocess_timeouts_and_cleanup(self):
        for stage, timeout in (("ffmpeg", 600), ("whisper", 600), ("whisper", 37)):
            with self.subTest(stage=stage, timeout=timeout), tempfile.TemporaryDirectory() as tmp:
                work = Path(tmp) / "work"
                work.mkdir()

                def run(cmd, **kwargs):
                    is_ffmpeg = cmd[0] == "ffmpeg"
                    self.assertEqual(kwargs.get("timeout"), 120 if is_ffmpeg else timeout)
                    if is_ffmpeg == (stage == "ffmpeg"):
                        raise subprocess.TimeoutExpired(cmd, kwargs["timeout"])

                with patch.object(transcribe.tempfile, "mkdtemp", return_value=str(work)), \
                     patch.object(transcribe, "TRANSCRIPTION_TIMEOUT", timeout, create=True), \
                     patch.object(transcribe.subprocess, "run", side_effect=run):
                    with self.assertRaises(subprocess.TimeoutExpired):
                        transcribe._transcribe_local("audio.ogg", "audio")
                self.assertFalse(work.exists())

    def test_whisper_timeout_env(self):
        probe = "import transcribe as t; print(t.TRANSCRIPTION_TIMEOUT)"
        for setting, expected in ((None, "600"), ("37", "37"), ("invalid", "600"), ("", "600")):
            with self.subTest(setting=setting):
                env = dict(os.environ)
                env.pop("TRANSCRIPTION_TIMEOUT", None)
                if setting is not None:
                    env["TRANSCRIPTION_TIMEOUT"] = setting
                result = subprocess.run([sys.executable, "-c", probe], env=env,
                                        cwd=Path(transcribe.__file__).parent,
                                        capture_output=True, text=True, check=True)
                self.assertEqual(result.stdout.strip(), expected)


if __name__ == "__main__":
    unittest.main()
