"""Stdlib unittest for the two pure functions whose silent regression is worst:
_is_expired (false-positive => permanent data loss) and _strip_accents (search
misses). Run: python3 -m unittest test_transcribe -v"""

import unittest
import unittest.mock
from datetime import datetime, timedelta, timezone

import transcribe
from transcribe import _is_expired, _retry_after_seconds, CDN_EXPIRY
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


class RetryAfterSecondsTest(unittest.TestCase):
    def test_respects_header(self):
        self.assertEqual(_retry_after_seconds("7", attempt=0), 7.0)

    def test_missing_header_falls_back_to_backoff(self):
        self.assertEqual(_retry_after_seconds(None, attempt=0), 1.0)
        self.assertEqual(_retry_after_seconds(None, attempt=3), 8.0)

    def test_malformed_header_falls_back_to_backoff(self):
        # A non-numeric Retry-After must not crash the retry loop.
        self.assertEqual(_retry_after_seconds("not-a-number", attempt=2), 4.0)

    def test_non_finite_header_falls_back_to_backoff(self):
        # float() accepts these without raising ValueError — inf/nan would
        # otherwise reach time.sleep and crash (OverflowError/ValueError).
        self.assertEqual(_retry_after_seconds("inf", attempt=1), 2.0)
        self.assertEqual(_retry_after_seconds("-inf", attempt=1), 2.0)
        self.assertEqual(_retry_after_seconds("nan", attempt=1), 2.0)

    def test_negative_header_is_clamped(self):
        self.assertEqual(_retry_after_seconds("-5", attempt=0), 0.0)

    def test_huge_header_is_clamped(self):
        # A legitimate but huge Retry-After (e.g. daily quota reset) shouldn't
        # stall a backfill for its full duration unnoticed.
        self.assertEqual(_retry_after_seconds("3600", attempt=0), 60.0)


class StripAccentsTest(unittest.TestCase):
    def test_removes_diacritics_and_lowercases(self):
        self.assertEqual(_strip_accents("São Paulo"), "sao paulo")
        self.assertEqual(_strip_accents("ARRAIÁ"), "arraia")

    def test_none_passthrough(self):
        self.assertIsNone(_strip_accents(None))

    def test_unaccented_query_matches_accented_text(self):
        # The whole point: a no-accent query normalizes to the same string.
        self.assertEqual(_strip_accents("conciliacao"), _strip_accents("conciliação"))


class EngineReadyTest(unittest.TestCase):
    def test_local_unconfigured_is_not_ready(self):
        with unittest.mock.patch.multiple(
            transcribe, TRANSCRIPTION_ENGINE="local", WHISPER_CLI="", WHISPER_MODEL=""
        ):
            ok, reason = transcribe.engine_ready()
        self.assertFalse(ok)
        self.assertIn("WHISPER_CLI", reason)


if __name__ == "__main__":
    unittest.main()
