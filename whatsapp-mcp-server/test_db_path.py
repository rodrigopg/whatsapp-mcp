import os
import sys
import tempfile
import unittest
from unittest.mock import patch
from pathlib import Path
from db_path import resolve_messages_db


class TestResolveMessagesDb(unittest.TestCase):
    """Test the resolution chain of the messages DB path."""

    def setUp(self):
        """Reset the module state before each test."""
        import db_path
        db_path._resolved_path = None

    def test_env_var_set(self):
        """Case 1: WHATSAPP_MESSAGES_DB env var is set → returned verbatim."""
        custom_path = "/custom/path/to/messages.db"
        with patch.dict(os.environ, {"WHATSAPP_MESSAGES_DB": custom_path}):
            result = resolve_messages_db()
            self.assertEqual(result, custom_path)

    def test_repo_relative_exists(self):
        """Case 2: Env unset + repo-relative path exists → repo-relative returned."""
        with tempfile.TemporaryDirectory() as tmpdir:
            fake_repo_file = os.path.join(tmpdir, "whatsapp-bridge", "store", "messages.db")
            os.makedirs(os.path.dirname(fake_repo_file), exist_ok=True)
            Path(fake_repo_file).touch()

            with patch.dict(os.environ, {"WHATSAPP_MESSAGES_DB": ""}, clear=False):
                with patch("os.path.dirname") as mock_dirname:
                    mock_dirname.return_value = os.path.join(tmpdir, "whatsapp-mcp-server")
                    with patch("os.path.exists") as mock_exists:
                        def exists_side_effect(path):
                            if path == fake_repo_file:
                                return True
                            return False
                        mock_exists.side_effect = exists_side_effect

                        result = resolve_messages_db()
                        # os.path.join, not a literal "/" path: the separator is
                        # "\" on Windows, so a hardcoded POSIX suffix fails there
                        # even though the resolved path is correct.
                        self.assertTrue(result.endswith(os.path.join("whatsapp-bridge", "store", "messages.db")))

    def test_home_path_exists(self):
        """Case 3: Env unset + relative missing + ~/.whatsapp-mcp/... exists → that path returned."""
        home_path = os.path.expanduser("~/.whatsapp-mcp/whatsapp-bridge/store/messages.db")
        with patch.dict(os.environ, {"WHATSAPP_MESSAGES_DB": ""}, clear=False):
            with patch("os.path.exists") as mock_exists:
                def exists_side_effect(path):
                    if path == home_path:
                        return True
                    return False
                mock_exists.side_effect = exists_side_effect

                result = resolve_messages_db()
                self.assertEqual(result, home_path)

    def test_fallback_to_repo_relative(self):
        """Case 4: Nothing exists → falls back to repo-relative path."""
        with patch.dict(os.environ, {"WHATSAPP_MESSAGES_DB": ""}, clear=False):
            with patch("os.path.exists", return_value=False):
                result = resolve_messages_db()
                self.assertTrue(result.endswith(os.path.join("whatsapp-bridge", "store", "messages.db")))

    def test_logs_once_to_stderr(self):
        """Test that the chosen path is logged once to stderr."""
        with tempfile.TemporaryDirectory() as tmpdir:
            fake_repo_file = os.path.join(tmpdir, "whatsapp-bridge", "store", "messages.db")
            os.makedirs(os.path.dirname(fake_repo_file), exist_ok=True)
            Path(fake_repo_file).touch()

            with patch.dict(os.environ, {"WHATSAPP_MESSAGES_DB": ""}, clear=False):
                with patch("os.path.dirname") as mock_dirname:
                    mock_dirname.return_value = os.path.join(tmpdir, "whatsapp-mcp-server")
                    with patch("os.path.exists") as mock_exists:
                        def exists_side_effect(path):
                            if path == fake_repo_file:
                                return True
                            return False
                        mock_exists.side_effect = exists_side_effect

                        with patch("sys.stderr", new_callable=__import__("io").StringIO) as mock_stderr:
                            result = resolve_messages_db()
                            stderr_output = mock_stderr.getvalue()
                            self.assertIn("whatsapp-mcp: using messages db at", stderr_output)


if __name__ == "__main__":
    unittest.main()
