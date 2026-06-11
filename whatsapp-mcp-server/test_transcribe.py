"""Stdlib unittest for the two pure functions whose silent regression is worst:
_is_expired (false-positive => permanent data loss) and _strip_accents (search
misses). Run: python3 -m unittest test_transcribe -v"""

import unittest
from datetime import datetime, timedelta, timezone

from transcribe import _is_expired, CDN_EXPIRY
from whatsapp import _strip_accents


def _iso(delta_days):
    dt = datetime.now(timezone.utc) - timedelta(days=delta_days)
    return dt.isoformat()


class IsExpiredTest(unittest.TestCase):
    def test_recent_is_not_expired(self):
        self.assertFalse(_is_expired(_iso(0)))
        self.assertFalse(_is_expired(_iso(CDN_EXPIRY.days - 1)))

    def test_old_is_expired(self):
        self.assertTrue(_is_expired(_iso(CDN_EXPIRY.days + 5)))

    def test_unknown_age_assumed_expired(self):
        # None / unparseable must NOT leave a row retried forever.
        self.assertTrue(_is_expired(None))
        self.assertTrue(_is_expired(""))
        self.assertTrue(_is_expired("not-a-date"))

    def test_naive_timestamp_handled(self):
        # go-sqlite3 stores an offset, but a naive string must not crash.
        naive = (datetime.now(timezone.utc) - timedelta(days=1)).replace(tzinfo=None)
        self.assertFalse(_is_expired(naive.isoformat()))


class StripAccentsTest(unittest.TestCase):
    def test_removes_diacritics_and_lowercases(self):
        self.assertEqual(_strip_accents("São Paulo"), "sao paulo")
        self.assertEqual(_strip_accents("ARRAIÁ"), "arraia")

    def test_none_passthrough(self):
        self.assertIsNone(_strip_accents(None))

    def test_unaccented_query_matches_accented_text(self):
        # The whole point: a no-accent query normalizes to the same string.
        self.assertEqual(_strip_accents("conciliacao"), _strip_accents("conciliação"))


if __name__ == "__main__":
    unittest.main()
