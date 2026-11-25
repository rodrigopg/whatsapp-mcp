import os
import stat
import tempfile
import unittest

from oauth import OwnerOAuth


@unittest.skipIf(os.name == "nt", "POSIX file modes")
class OAuthDatabaseMode(unittest.TestCase):
    def open(self, path):
        provider = OwnerOAuth(path, "p" * 32, "", "https://mcp.example.com")
        self.addCleanup(provider._db.close)
        return stat.S_IMODE(os.stat(path).st_mode)

    def test_the_login_database_is_readable_by_its_owner_only(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        old = os.umask(0o022)
        self.addCleanup(os.umask, old)
        self.assertEqual(self.open(os.path.join(tmp.name, "new.db")), 0o600, "a new database")
        older = os.path.join(tmp.name, "older.db")
        with open(older, "w"):  # created with the default mode, as an older version did
            pass
        self.assertNotEqual(stat.S_IMODE(os.stat(older).st_mode), 0o600)
        self.assertEqual(self.open(older), 0o600, "a database an older version created")


if __name__ == "__main__":
    unittest.main()
