"""Recover + transcribe audios whose CDN media expired (403), via media retry.

For audios marked '[áudio indisponível...]', this asks the phone to re-upload
the blob (whatsmeow SendMediaRetryReceipt). The bridge's handleMediaRetry
downloads the fresh copy and writes it to store/<chat>/<msgID>_<filename>.
This script then transcribes those recovered files straight off disk (no
re-download — that would hit the still-dead URL) and writes messages.content.

Throttled in small batches with a quiet-period wait, because a flood of retry
receipts is exactly what gets an account rate-limited. Driven off the set of
messages that actually recovered, so non-recovered rows are left untouched.

Run:  python3 recover_audios.py [--limit N] [--batch 15]
"""

import argparse
import glob
import os
import re
import sqlite3
import time

import requests

from transcribe import (DB_PATH, API_BASE, transcribe, write_content,
                        SENTINEL_EMPTY)

BRIDGE_LOG = "/tmp/wa-bridge.log"
SENTINEL_NOT_ON_PHONE = "[áudio indisponível: não está mais no telefone]"

RE_SUCCESS = re.compile(r"MEDIA RETRY (\w+): recovered \d+ bytes")
RE_NOT_AVAIL = re.compile(r"MEDIA RETRY (\w+): result=.*not available")
RE_DECRYPT_FAIL = re.compile(r"MEDIA RETRY (\w+): FAILED")


def log(msg):
    print(f"[{time.strftime('%H:%M:%S')}] {msg}", flush=True)


def unavailable_audios(conn, limit):
    sql = ("SELECT id, chat_jid FROM messages "
           "WHERE media_type='audio' AND content LIKE '[áudio indisponível%' "
           "ORDER BY timestamp DESC")
    if limit:
        sql += f" LIMIT {int(limit)}"
    return conn.execute(sql).fetchall()


def request_retry(message_id, chat_jid):
    r = requests.post(f"{API_BASE}/mediaretry",
                      json={"message_id": message_id, "chat_jid": chat_jid},
                      timeout=30)
    return r.status_code == 200 and r.json().get("success")


def scan_log_since(offset):
    """Read new bridge-log bytes since offset; classify MEDIA RETRY outcomes."""
    recovered, not_on_phone, failed = set(), set(), set()
    with open(BRIDGE_LOG, encoding="utf-8", errors="ignore") as f:
        f.seek(offset)
        data = f.read()
        new_offset = f.tell()
    for line in data.splitlines():
        if m := RE_SUCCESS.search(line):
            recovered.add(m.group(1))
        elif m := RE_NOT_AVAIL.search(line):
            not_on_phone.add(m.group(1))
        elif m := RE_DECRYPT_FAIL.search(line):
            failed.add(m.group(1))
    return recovered, not_on_phone, failed, new_offset


def recovered_file(chat_jid, msg_id):
    chat_dir = os.path.join(os.path.dirname(DB_PATH), chat_jid.replace(":", "_"))
    hits = glob.glob(os.path.join(chat_dir, f"{msg_id}_*"))
    return hits[0] if hits else None


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--limit", type=int, default=None)
    ap.add_argument("--batch", type=int, default=15)
    ap.add_argument("--quiet", type=int, default=12, help="seconds of log silence = batch done")
    args = ap.parse_args()

    conn = sqlite3.connect(DB_PATH, timeout=10)
    rows = unavailable_audios(conn, args.limit)
    conn.close()
    total = len(rows)
    log(f"Unavailable audios to attempt: {total}")

    log_offset = os.path.getsize(BRIDGE_LOG) if os.path.exists(BRIDGE_LOG) else 0
    recovered_ids, not_on_phone_ids, failed_ids = set(), set(), set()
    id_to_chat = {r[0]: r[1] for r in rows}

    for start in range(0, total, args.batch):
        batch = rows[start:start + args.batch]
        log(f"--- batch {start//args.batch + 1}: requesting {len(batch)} retries ---")
        for msg_id, chat_jid in batch:
            if not request_retry(msg_id, chat_jid):
                failed_ids.add(msg_id)
            time.sleep(0.4)  # gentle pacing between receipts

        # Wait for the phone's async responses: stop when the log goes quiet.
        last_change = time.time()
        last_size = os.path.getsize(BRIDGE_LOG)
        while time.time() - last_change < args.quiet:
            time.sleep(2)
            size = os.path.getsize(BRIDGE_LOG)
            if size != last_size:
                last_size = size
                last_change = time.time()
            # backoff guard
            with open(BRIDGE_LOG, encoding="utf-8", errors="ignore") as f:
                f.seek(max(0, size - 4000))
                tail = f.read()
            if "stream end" in tail or "replaced" in tail.lower():
                log("WARN: stream/replaced in log — backing off 30s")
                time.sleep(30)

        rec, nap, fail, log_offset = scan_log_since(log_offset)
        recovered_ids |= rec
        not_on_phone_ids |= nap
        failed_ids |= fail
        log(f"  batch result: recovered={len(rec)} not_on_phone={len(nap)} failed={len(fail)}")

    # Transcribe everything that recovered, straight off disk.
    log(f"Transcribing {len(recovered_ids)} recovered audios...")
    transcribed = empty = missing = 0
    for msg_id in recovered_ids:
        chat_jid = id_to_chat[msg_id]
        path = recovered_file(chat_jid, msg_id)
        if not path:
            missing += 1
            continue
        text = transcribe(path, msg_id)
        if text:
            write_content(msg_id, chat_jid, text)
            transcribed += 1
            log(f"  {msg_id[:12]} ok: {text[:50]}...")
        else:
            write_content(msg_id, chat_jid, SENTINEL_EMPTY)
            empty += 1

    # Mark phone-confirmed-absent distinctly so a future pass can skip them.
    conn = sqlite3.connect(DB_PATH, timeout=10)
    conn.execute("PRAGMA busy_timeout=5000")
    for msg_id in not_on_phone_ids:
        conn.execute("UPDATE messages SET content=? WHERE id=?",
                     (SENTINEL_NOT_ON_PHONE, msg_id))
    conn.commit()
    conn.close()

    no_response = total - len(recovered_ids) - len(not_on_phone_ids) - len(failed_ids)
    log("=" * 50)
    log(f"RECOVERY DONE (of {total} attempted):")
    log(f"  recovered+transcribed = {transcribed}")
    log(f"  recovered but empty    = {empty}")
    log(f"  recovered but file gone= {missing}")
    log(f"  not on phone (code 2)  = {len(not_on_phone_ids)}")
    log(f"  request failed         = {len(failed_ids)}")
    log(f"  no response (offline/throttled, retry later) = {no_response}")


if __name__ == "__main__":
    main()
