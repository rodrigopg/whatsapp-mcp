import sqlite3
import tempfile
import unittest
from pathlib import Path

import clone_voice


class RowsTest(unittest.TestCase):
    def test_only_own_audio_with_text(self):
        with tempfile.TemporaryDirectory() as d:
            db = Path(d) / "messages.db"
            c = sqlite3.connect(db)
            c.execute("CREATE TABLE messages (id, chat_jid, content, filename, timestamp, is_from_me, media_type)")
            c.executemany("INSERT INTO messages VALUES (?,?,?,?,?,?,?)", [
                ("mine", "a@s.whatsapp.net", "ola tudo bem", "x.ogg", "2026-01-01", 1, "audio"),
                ("theirs", "a@s.whatsapp.net", "ola tudo bem", "y.ogg", "2026-01-01", 0, "audio"),
                ("notext", "a@s.whatsapp.net", "", "z.ogg", "2026-01-01", 1, "audio"),
                ("image", "a@s.whatsapp.net", "foto", "w.jpg", "2026-01-01", 1, "image"),
            ])
            c.commit()
            c.close()
            self.assertEqual([r[0] for r in clone_voice.rows(db, None, None)], ["mine"])

    def test_placeholder(self):
        self.assertTrue(clone_voice.PLACEHOLDER.match("[audio]"))
        self.assertFalse(clone_voice.PLACEHOLDER.match("ola tudo bem"))

    def test_min_chars_is_configurable(self):
        self.assertFalse(clone_voice.usable_text("你好，谢谢你", 8))
        self.assertTrue(clone_voice.usable_text("你好，谢谢你", 4))
        self.assertFalse(clone_voice.usable_text("[audio]", 1))


if __name__ == "__main__":
    unittest.main()
