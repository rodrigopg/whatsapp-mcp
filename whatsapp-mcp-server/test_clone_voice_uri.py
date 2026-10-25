import sqlite3
import tempfile
import unittest
from pathlib import Path

import clone_voice


class StorePathWithURICharactersTest(unittest.TestCase):
    def test_reads_the_named_database(self):
        with tempfile.TemporaryDirectory() as d:
            store = Path(d) / "a#b?c%41 d"
            store.mkdir()
            db = store / "messages.db"
            c = sqlite3.connect(db)
            c.execute("CREATE TABLE messages (id, chat_jid, content, filename, timestamp, is_from_me, media_type)")
            c.execute("INSERT INTO messages VALUES ('mine', 'a@s.whatsapp.net', 'ola', 'x.ogg', '2026-01-01', 1, 'audio')")
            c.commit()
            c.close()
            self.assertEqual([r[0] for r in clone_voice.rows(db, None, None)], ["mine"])


if __name__ == "__main__":
    unittest.main()
