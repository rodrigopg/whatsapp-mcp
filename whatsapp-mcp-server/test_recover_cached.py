import hashlib
import sqlite3
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import recover_audios
import transcribe


class RecoverCachedTest(unittest.TestCase):
    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.root = Path(tmp.name)
        self.db = self.root / "messages.db"
        self.log_file = self.root / "bridge.log"
        self.log_file.write_text("")
        with sqlite3.connect(self.db) as conn:
            conn.execute("CREATE TABLE messages (id TEXT, chat_jid TEXT, content TEXT, "
                         "media_type TEXT, file_sha256 BLOB, file_length INTEGER, timestamp TEXT)")
        for module in (transcribe, recover_audios):
            self.enterContext(patch.object(module, "DB_PATH", str(self.db)))
        self.enterContext(patch.object(recover_audios, "log"))
        self.enterContext(patch.object(recover_audios, "BRIDGE_LOG", str(self.log_file)))
        self.enterContext(patch.object(sys, "argv", ["recover_audios.py", "--quiet", "0"]))
        self.enterContext(patch.object(recover_audios, "engine_ready", return_value=(True, "test")))
        self.tx = self.enterContext(patch.object(recover_audios, "transcribe", return_value="transcript"))
        self.retry = self.enterContext(patch.object(recover_audios, "request_retry", return_value=True))
        self.enterContext(patch.object(recover_audios.time, "sleep"))

    def audio(self, msg_id, chat, data=None):
        content = transcribe.SENTINEL_UNAVAILABLE
        digest = hashlib.sha256(data).digest() if data else None
        with sqlite3.connect(self.db) as conn:
            conn.execute("INSERT INTO messages VALUES (?, ?, ?, 'audio', ?, ?, '2000-01-01T00:00:00Z')",
                         (msg_id, chat, content, digest, len(data) if data else 0))
        if data:
            path = self.root / chat / (msg_id + "_audio.ogg")
            path.parent.mkdir(exist_ok=True)
            path.write_bytes(data)

    def contents(self):
        with sqlite3.connect(self.db) as conn:
            return dict(((r[0], r[1]), r[2]) for r in conn.execute("SELECT id, chat_jid, content FROM messages"))

    def test_file_recovered_after_an_earlier_run_is_transcribed_without_a_retry(self):
        self.audio("late", "chat", b"recovered audio")
        recover_audios.main()
        self.retry.assert_not_called()
        self.assertEqual(self.contents()[("late", "chat")], "transcript")

    def test_not_on_phone_marks_only_the_attempted_chat(self):
        self.audio("same", "chat")
        with sqlite3.connect(self.db) as conn:
            conn.execute("INSERT INTO messages VALUES ('same', 'other', 'text', '', NULL, 0, '2000-01-01T00:00:00Z')")

        def answer(msg_id, chat_jid):
            with open(self.log_file, "a") as f:
                f.write(f"MEDIA RETRY {msg_id}: NOTONPHONE\n")
            return True
        self.retry.side_effect = answer
        recover_audios.main()
        self.assertEqual(self.contents(), {("same", "chat"): recover_audios.SENTINEL_NOT_ON_PHONE,
                                           ("same", "other"): "text"})

    def test_cached_copy_does_not_hide_the_same_id_in_another_chat(self):
        self.audio("same", "cached", b"recovered audio")
        self.audio("same", "other")
        recover_audios.main()
        self.retry.assert_called_once_with("same", "other")
        self.assertEqual(self.contents()[("same", "cached")], "transcript")

    def test_late_not_on_phone_leaves_a_cached_audio_transcribed(self):
        self.audio("cached", "chat", b"recovered audio")
        self.audio("asked", "chat")

        def answer(msg_id, chat_jid):
            with open(self.log_file, "a") as f:
                f.write("MEDIA RETRY cached: NOTONPHONE\n")
            return True
        self.retry.side_effect = answer
        recover_audios.main()
        self.assertEqual(self.contents()[("cached", "chat")], "transcript")

    def test_ambiguous_not_on_phone_keeps_the_recovered_twin(self):
        self.audio("same", "found")
        self.audio("same", "lost")

        def answer(msg_id, chat_jid):
            if chat_jid == "found":
                path = self.root / "found" / "same_audio.ogg"
                path.parent.mkdir(exist_ok=True)
                path.write_bytes(b"recovered audio")
                with sqlite3.connect(self.db) as conn:
                    conn.execute("UPDATE messages SET file_sha256=?, file_length=? WHERE chat_jid='found'",
                                 (hashlib.sha256(b"recovered audio").digest(), len(b"recovered audio")))
                line = "MEDIA RETRY same: SUCCESS recovered 15 bytes\n"
            else:
                line = "MEDIA RETRY same: NOTONPHONE\n"
            with open(self.log_file, "a") as f:
                f.write(line)
            return True
        self.retry.side_effect = answer
        recover_audios.main()
        contents = self.contents()
        self.assertEqual(contents[("same", "found")], "transcript")
        self.assertEqual(contents[("same", "lost")], transcribe.SENTINEL_UNAVAILABLE)


if __name__ == "__main__":
    unittest.main()
