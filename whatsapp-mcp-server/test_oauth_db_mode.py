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
        path = os.path.join(tmp.name, "o.db")
        self.assertEqual(self.open(path), 0o600, "a new database")
        os.chmod(path, 0o644)
        self.assertEqual(self.open(path), 0o600, "a database an older version created")


if __name__ == "__main__":
    unittest.main()
