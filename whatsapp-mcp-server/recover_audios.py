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
                        engine_ready, SENTINEL_EMPTY, _bridge_auth_headers)

BRIDGE_LOG = os.environ.get("WHATSAPP_BRIDGE_LOG", "/tmp/wa-bridge.log")
SENTINEL_NOT_ON_PHONE = "[áudio indisponível: não está mais no telefone]"

# These must stay in sync with the stable log contract emitted by
# handleMediaRetry in whatsapp-bridge/main.go. Any "MEDIA RETRY <id>:" line that
# matches none of these goes to the `unclassified` bucket — never silently into
# no_response, which would tell the operator to retry a permanent failure.
RE_LINE = re.compile(r"MEDIA RETRY (\w+): (\w+)")
RES_SUCCESS = "SUCCESS"
RES_NOTONPHONE = "NOTONPHONE"
RES_ERROR = "ERROR"


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
                      headers=_bridge_auth_headers(),
                      timeout=30)
    if r.status_code == 401:
        log("HTTP 401 - check WHATSAPP_API_AUTH_TOKEN matches the bridge's API_AUTH_TOKEN")
    return r.status_code == 200 and r.json().get("success")


def scan_log_since(offset):
    """Read new bridge-log bytes since offset; classify MEDIA RETRY outcomes.
    Returns (recovered, not_on_phone, errored, unclassified, new_offset)."""
    recovered, not_on_phone, errored, unclassified = set(), set(), set(), set()
    try:
        with open(BRIDGE_LOG, encoding="utf-8", errors="ignore") as f:
            f.seek(offset)
            data = f.read()
            new_offset = f.tell()
    except OSError as e:
        log(f"WARN: cannot read bridge log {BRIDGE_LOG}: {e}")
        return recovered, not_on_phone, errored, unclassified, offset
    for line in data.splitlines():
        m = RE_LINE.search(line)
        if not m:
            continue
        msg_id, result = m.group(1), m.group(2)
        if result == RES_SUCCESS:
            recovered.add(msg_id)
        elif result == RES_NOTONPHONE:
            not_on_phone.add(msg_id)
        elif result == RES_ERROR:
            errored.add(msg_id)
        else:
            unclassified.add(msg_id)
    return recovered, not_on_phone, errored, unclassified, new_offset


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

    # Recovery transcribes the recovered files, so the engine must be usable.
    ok, reason = engine_ready()
    if not ok:
        log(f"Transcription engine not ready ({reason}); aborting recovery so "
            f"recovered audios aren't left untranscribed.")
        return
    # The whole flow is driven by scraping the bridge log; if it isn't there,
    # every outcome would be misclassified as no-response. Fail loudly instead.
    if not os.path.exists(BRIDGE_LOG):
        log(f"Bridge log not found at {BRIDGE_LOG}. Set WHATSAPP_BRIDGE_LOG or "
            f"start the bridge with stdout redirected there. Aborting.")
        return

    conn = sqlite3.connect(DB_PATH, timeout=10)
    rows = unavailable_audios(conn, args.limit)
    conn.close()
    # A retry answered after an earlier run ended left a verified file on disk; transcribe it
    # instead of asking the phone again.
    cached = [(m, c) for m, c in rows if recovered_file(c, m)]
    if cached:
        log(f"Already recovered on disk: {len(cached)}")
    rows = [r for r in rows if r not in cached]
    # The log names only the message ID, which can repeat across chats.
    attempted = {}
    for msg_id, chat_jid in rows:
        attempted.setdefault(msg_id, []).append(chat_jid)
    total = len(rows)
    log(f"Unavailable audios to attempt: {total}")

    log_offset = os.path.getsize(BRIDGE_LOG)
    recovered_ids, not_on_phone_ids, errored_ids, unclassified_ids = set(), set(), set(), set()
    request_failed_ids = set()

    for start in range(0, total, args.batch):
        batch = rows[start:start + args.batch]
        log(f"--- batch {start//args.batch + 1}: requesting {len(batch)} retries ---")
        for msg_id, chat_jid in batch:
            try:
                if not request_retry(msg_id, chat_jid):
                    request_failed_ids.add(msg_id)
            except requests.RequestException as e:
                request_failed_ids.add(msg_id)
                log(f"  retry request failed for {msg_id[:12]}: {e}")
            time.sleep(0.4)  # gentle pacing between receipts

        # Wait for the phone's async responses: stop when the log goes quiet.
        last_change = time.time()
        last_size = os.path.getsize(BRIDGE_LOG)
        while time.time() - last_change < args.quiet:
            time.sleep(2)
            try:
                size = os.path.getsize(BRIDGE_LOG)
                with open(BRIDGE_LOG, encoding="utf-8", errors="ignore") as f:
                    f.seek(max(0, size - 4000))
                    tail = f.read()
            except OSError as e:
                log(f"  WARN: bridge log read failed ({e}); ending wait")
                break
            if size != last_size:
                last_size = size
                last_change = time.time()
            if "stream end" in tail or "replaced" in tail.lower():
                log("  WARN: stream/replaced in log — backing off 30s")
                time.sleep(30)

        rec, nap, err, uncl, log_offset = scan_log_since(log_offset)
        recovered_ids |= rec
        not_on_phone_ids |= nap
        errored_ids |= err
        unclassified_ids |= uncl
        log(f"  batch: recovered={len(rec)} not_on_phone={len(nap)} "
            f"errored={len(err)} unclassified={len(uncl)}")

    # Transcribe everything that recovered, straight off disk. Guard per item so
    # one failure (or a stale log line whose ID isn't in this run) can't abort
    # the loop and lose already-recovered work.
    # A recovered ID outside this run's attempts is a log line from a prior run.
    to_transcribe = cached + [(m, c) for m in recovered_ids for c in attempted.get(m, [])]
    log(f"Transcribing {len(to_transcribe)} recovered audios...")
    transcribed = empty = missing = tx_failed = 0
    for msg_id, chat_jid in to_transcribe:
        try:
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
        except Exception as e:
            tx_failed += 1
            log(f"  {msg_id[:12]} transcription error: {e}")

    # Mark phone-confirmed-absent distinctly so a future pass can skip them.
    conn = sqlite3.connect(DB_PATH, timeout=10)
    conn.execute("PRAGMA busy_timeout=5000")
    for msg_id in not_on_phone_ids:
        chats = attempted.get(msg_id, [])
        # With the ID in several chats the answer may belong to any of them; retry next run.
        if len(chats) == 1 and not recovered_file(chats[0], msg_id):
            conn.execute("UPDATE messages SET content=? WHERE id=? AND chat_jid=?",
                         (SENTINEL_NOT_ON_PHONE, msg_id, chats[0]))
    conn.commit()
    conn.close()

    classified = (len(recovered_ids) | 0) + len(not_on_phone_ids) + len(errored_ids)
    no_response = total - classified - len(request_failed_ids) - len(unclassified_ids)
    log("=" * 50)
    log(f"RECOVERY DONE (of {total} attempted):")
    log(f"  recovered+transcribed  = {transcribed}")
    log(f"  recovered but empty    = {empty}")
    log(f"  recovered but file gone= {missing}")
    log(f"  transcription errored  = {tx_failed}")
    log(f"  not on phone           = {len(not_on_phone_ids)}")
    log(f"  bridge errored (see log)= {len(errored_ids)}")
    log(f"  unclassified (see log) = {len(unclassified_ids)}")
    log(f"  retry request failed   = {len(request_failed_ids)}")
    log(f"  no response (retry later)= {max(0, no_response)}")


if __name__ == "__main__":
    main()
