import sqlite3
import tempfile
import unittest
from pathlib import Path

import clone_voice
import recover_audios
import transcribe


class ReadQueriesTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.db = Path(self.directory.name) / "messages.db"
        self.conn = sqlite3.connect(self.db)
        self.addCleanup(self.conn.close)
        self.conn.execute("CREATE TABLE messages (id, chat_jid, content, filename, timestamp, is_from_me, media_type, file_sha256, file_length)")
        for mid, at in [
            ("old", "2026-10-01 10:00:00+03:00"),
            ("target", "2026-10-01 08:00:00+00:00"),
            ("tie", "2026-10-01 05:00:00-03:00"),
            ("new", "2026-10-01 06:00:00-03:00"),
        ]:
            self.conn.execute("INSERT INTO messages VALUES (?, 'test', '', 'voice.ogg', ?, 1, 'audio', NULL, NULL)", (mid, at))
        self.conn.commit()

    def test_pending_audios(self):
        # Incoming audio: outgoing is skipped without TRANSCRIBE_OUTGOING.
        self.conn.execute("UPDATE messages SET is_from_me = 0")
        self.assertEqual([r[0] for r in transcribe.pending_audios(self.conn, 3)], ["new", "tie", "target"])

    def test_unavailable_audios(self):
        self.conn.execute("UPDATE messages SET content = '[áudio indisponível]'")
        self.assertEqual([r[0] for r in recover_audios.unavailable_audios(self.conn, 3)], ["new", "tie", "target"])

    def test_voice_samples(self):
        self.conn.execute("UPDATE messages SET content = 'test transcript'")
        self.conn.commit()
        self.assertEqual([r[0] for r in clone_voice.rows(self.db, None, None)], ["new", "tie", "target", "old"])

    def test_voice_samples_since(self):
        self.conn.execute("UPDATE messages SET content = 'test transcript'")
        self.conn.commit()
        self.assertEqual([r[0] for r in clone_voice.rows(self.db, "2026-10-01T11:00:00+03:00", "test")], ["new", "tie", "target"])
