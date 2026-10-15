"""Audio you sent yourself is only pending for the transcription sweep when
TRANSCRIBE_OUTGOING=1: a cloud engine would otherwise upload voice notes the
user never asked to transcribe.

Run: python -m unittest test_transcribe_outgoing -v"""

import sqlite3
import unittest
import unittest.mock

import transcribe


class PendingOutgoingTest(unittest.TestCase):
    def setUp(self):
        self.conn = sqlite3.connect(":memory:")
        self.addCleanup(self.conn.close)
        self.conn.execute("CREATE TABLE messages (id TEXT, chat_jid TEXT, content TEXT, "
                          "timestamp TEXT, media_type TEXT, file_sha256 BLOB, is_from_me BOOLEAN)")
        self.conn.executemany(
            "INSERT INTO messages VALUES (?, 'c@s.whatsapp.net', '', '2026-01-01', 'audio', NULL, ?)",
            [("IN", 0), ("OUT", 1)])

    def _pending_ids(self, flag):
        with unittest.mock.patch.object(transcribe, "TRANSCRIBE_OUTGOING", flag):
            return {r[0] for r in transcribe.pending_audios(self.conn)}

    def test_outgoing_audio_is_skipped_by_default(self):
        self.assertEqual(self._pending_ids(False), {"IN"})

    def test_outgoing_audio_is_pending_with_the_flag(self):
        self.assertEqual(self._pending_ids(True), {"IN", "OUT"})


if __name__ == "__main__":
    unittest.main()
