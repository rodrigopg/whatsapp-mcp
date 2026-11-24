import os
import tempfile
import unittest
from unittest.mock import patch

import audio


class ConvertedAudioLocation(unittest.TestCase):
    """The bridge sends files only from a few folders by default (media_path.go). The audio this
    server converts or synthesizes, then sends, must land in one of them: the temp folder, under a
    name that is not hidden."""

    def test_converted_audio_lands_in_the_temp_folder(self):
        with patch("audio.convert_to_opus_ogg"):
            path = audio.convert_to_opus_ogg_temp("input.wav")
        self.addCleanup(os.remove, path)
        self.assertEqual(os.path.dirname(path), tempfile.gettempdir())
        self.assertFalse(os.path.basename(path).startswith("."))
        self.assertTrue(path.endswith(".ogg"))


if __name__ == "__main__":
    unittest.main()
