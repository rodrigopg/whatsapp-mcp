#!/usr/bin/env python3
"""Build a voice-cloning / TTS fine-tuning dataset from the user's OWN WhatsApp voice notes.

Only messages with is_from_me=1 are read, so no other person's voice is ever
collected. Output (all local, nothing is uploaded):

    <out>/wavs/*.wav        24 kHz mono 16-bit
    <out>/train_raw.jsonl   {"audio", "text", "ref_audio"} per line (Qwen3-TTS format)
    <out>/metadata.csv      LJSpeech style: id|text
    <out>/report.txt

Run with: uv run --with imageio-ffmpeg python clone_voice.py --store ../whatsapp-bridge/store
"""
import argparse
import csv
import json
import os
import re
import sqlite3
import subprocess
import sys
import wave
from pathlib import Path

SAMPLE_RATE = 24000
PLACEHOLDER = re.compile(r"^\s*(\[.*\]|<.*>)?\s*$")


def usable_text(text, min_chars):
    return not PLACEHOLDER.match(text) and len(text) >= min_chars


def ffmpeg_exe():
    if os.environ.get("FFMPEG"):
        return os.environ["FFMPEG"]
    try:
        import imageio_ffmpeg

        return imageio_ffmpeg.get_ffmpeg_exe()
    except ImportError:
        return "ffmpeg"


def to_wav(ffmpeg, src, dst):
    r = subprocess.run(
        [ffmpeg, "-y", "-loglevel", "error", "-i", str(src), "-ac", "1", "-ar", str(SAMPLE_RATE),
         "-sample_fmt", "s16", str(dst)],
        capture_output=True,
    )
    return r.returncode == 0 and dst.exists()


def duration(path):
    with wave.open(str(path)) as w:
        return w.getnframes() / w.getframerate()


def rows(db, since, chat):
    con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
    q = ("SELECT id, chat_jid, content, filename, timestamp FROM messages "
         "WHERE is_from_me = 1 AND media_type = 'audio' AND COALESCE(content, '') != '' ")
    args = []
    if since:
        q += "AND timestamp >= ? "
        args.append(since)
    if chat:
        q += "AND chat_jid = ? "
        args.append(chat)
    return con.execute(q + "ORDER BY timestamp DESC", args).fetchall()


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--store", required=True, help="bridge store dir (has messages.db and one folder per chat)")
    ap.add_argument("--out", default="voice-dataset")
    ap.add_argument("--min-sec", type=float, default=2.0)
    ap.add_argument("--max-sec", type=float, default=20.0)
    ap.add_argument("--min-chars", type=int, default=8,
                    help="drop transcripts shorter than this; lower it for logographic scripts")
    ap.add_argument("--min-rate", type=float, default=6.0,
                    help="min transcript chars per second of audio; below = silence or a bad transcript")
    ap.add_argument("--max-rate", type=float, default=28.0,
                    help="max transcript chars per second; widen both for languages with denser or sparser scripts")
    ap.add_argument("--limit", type=int, default=0, help="stop after N accepted clips (0 = all)")
    ap.add_argument("--since", help="only messages on/after this date, e.g. 2025-01-01")
    ap.add_argument("--chat", help="only this chat JID")
    a = ap.parse_args()

    store, out = Path(a.store).expanduser(), Path(a.out).expanduser()
    (out / "wavs").mkdir(parents=True, exist_ok=True)
    ff = ffmpeg_exe()
    kept, skipped = [], {"no_file": 0, "bad_text": 0, "too_short": 0, "too_long": 0, "bad_rate": 0, "decode": 0}

    for mid, jid, text, fname, ts in rows(store / "messages.db", a.since, a.chat):
        if a.limit and len(kept) >= a.limit:
            break
        text = " ".join(text.split())
        src = store / jid.replace(":", "_") / f"{mid}_{fname}"
        if not fname or not src.is_file():
            skipped["no_file"] += 1
            continue
        if not usable_text(text, a.min_chars):
            skipped["bad_text"] += 1
            continue
        dst = out / "wavs" / f"{mid}.wav"
        if not to_wav(ff, src, dst):
            skipped["decode"] += 1
            continue
        d = duration(dst)
        # transcript/duration sanity: speech in alphabetic scripts runs ~10-22 chars/s; far off = bad transcript or silence
        if d < a.min_sec:
            skipped["too_short"] += 1
        elif d > a.max_sec:
            skipped["too_long"] += 1
        elif not a.min_rate <= len(text) / d <= a.max_rate:
            skipped["bad_rate"] += 1
        else:
            kept.append((mid, dst, text, d))
            continue
        dst.unlink()

    if not kept:
        sys.exit(f"no usable clips; skipped: {skipped}")

    # reference clip: 5-10 s, closest to 8 s (Qwen3-TTS uses one ref_audio for the whole set)
    ref = min(kept, key=lambda k: abs(k[3] - 8) if 5 <= k[3] <= 10 else 99)[1]
    with open(out / "train_raw.jsonl", "w", encoding="utf-8") as j, \
         open(out / "metadata.csv", "w", encoding="utf-8", newline="") as m:
        w = csv.writer(m, delimiter="|")
        for mid, dst, text, _ in kept:
            j.write(json.dumps({"audio": str(dst.resolve()), "text": text, "ref_audio": str(ref.resolve())},
                               ensure_ascii=False) + "\n")
            w.writerow([mid, text])
    hours = sum(k[3] for k in kept) / 3600
    report = f"clips={len(kept)} hours={hours:.2f} ref={ref.name} skipped={skipped}\n"
    (out / "report.txt").write_text(report)
    print(report, end="")


if __name__ == "__main__":
    main()
