---
description: Build a voice-cloning / TTS fine-tuning dataset (24 kHz wavs + transcripts) from the user's OWN WhatsApp voice notes
---

Turn the user's sent voice notes into a dataset for voice cloning or TTS fine-tuning (e.g. Qwen3-TTS). Everything stays on this machine; nothing is uploaded.

## Hard rules

- Only the user's own voice: the script reads `is_from_me = 1` audio only. Never add audios received from other people — that is someone else's voice and needs their consent.
- The dataset holds private conversation text (names, health, money). Tell the user not to publish it or push it to a public repo/Hub.

## 1. Pick the account and the store

If more than one `whatsapp*` account exists, ask which one holds the user's voice notes (default: the one with more sent audios). The store is the bridge's `store/` dir (has `messages.db` and one folder per chat), e.g. `~/whatsapp-mcp/whatsapp-bridge/store` on the VPS or `whatsapp-bridge/store` in a local checkout.

## 2. Build

```bash
cd whatsapp-mcp-server
uv run --with imageio-ffmpeg python clone_voice.py --store <store> --out ~/voice-dataset --max-sec 25
```

Options: `--since 2025-01-01` (recent voice = better match to current voice), `--chat <jid>`, `--limit N`, `--min-sec`, `--min-rate`/`--max-rate` (transcript chars per second, default 6–28) and `--min-chars` (default 8); lower or widen them for languages whose script is much denser or sparser. `imageio-ffmpeg` brings a static ffmpeg; set `FFMPEG=/path` to use another.

The script keeps clips of 2-25 s whose transcript already exists (the bridge's transcription sweep) and whose characters-per-second is plausible, converts them to 24 kHz mono 16-bit wav, and writes:

- `wavs/*.wav`
- `train_raw.jsonl` — `{"audio", "text", "ref_audio"}` per line (the Qwen3-TTS fine-tuning input; `ref_audio` is one 5-10 s clip shared by all rows)
- `metadata.csv` — LJSpeech style `id|text`
- `report.txt` — clip count, hours, skipped counts

Audios not yet downloaded to disk are skipped (`no_file`); download more with `download_media` and re-run if the count is too low.

## 3. Report and hand off

Tell the user: clips, hours, the output path, and the skipped breakdown. Rough guide: ~1 h of clean speech is enough for a clone; 3 h+ is plenty for fine-tuning.

Next step for Qwen3-TTS: run its `finetuning/` data-prep on `train_raw.jsonl` (adds audio codes), then the SFT script — follow that README. Spot-check 5 clips by ear first: transcripts come from automatic transcription, so a few will be wrong; bad text hurts more than fewer clips.
