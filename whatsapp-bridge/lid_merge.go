package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// lidMediaSchema holds the renames planned for a merged chat's cached media, written in the
// merge's own transaction: the files move afterwards, possibly on a later start, and by then the
// LID rows that name them are gone.
const lidMediaSchema = `
	CREATE TABLE IF NOT EXISTS lid_media_moves (
		lid TEXT,
		pn TEXT,
		src TEXT,
		dst TEXT,
		PRIMARY KEY (lid, src)
	);
`

// senderKey is what messages.sender stores for an author: the phone number when whatsmeow knows
// the LID's number, otherwise the whole LID JID. A bare LID user would later read as a phone
// number (a media retry would address <lid>@s.whatsapp.net).
func senderKey(client *whatsmeow.Client, jid types.JID) string {
	pn := resolveToPN(client, jid)
	if pn.Server == types.HiddenUserServer {
		return pn.ToNonAD().String()
	}
	return pn.User
}

// parseSender turns a stored messages.sender back into a JID: a full JID as stored, a bare user as
// a phone number.
func parseSender(sender string) (types.JID, error) {
	if strings.Contains(sender, "@") {
		return types.ParseJID(sender)
	}
	return types.JID{User: sender, Server: types.DefaultUserServer}, nil
}

// migrateLIDChats merges chats stored under a LID JID into the phone-number chat, and rewrites
// authors stored as LIDs, once whatsmeow knows the mapping. Each chat merges in its own
// transaction: any failure rolls that chat back whole and leaves it for the next start.
func migrateLIDChats(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	if client == nil || store == nil || store.db == nil {
		return
	}
	lidChats, err := queryStrings(store.db, "SELECT jid FROM chats WHERE jid LIKE '%@"+types.HiddenUserServer+"'")
	if err != nil {
		logger.Warnf("LID migration: failed to list LID chats: %v", err)
		return
	}
	merged, unmapped, failed := 0, 0, 0
	for _, lid := range lidChats {
		lidJID, err := types.ParseJID(lid)
		pn := resolveToPN(client, lidJID)
		if err != nil || pn.Server != types.DefaultUserServer {
			unmapped++
			continue
		}
		if err := mergeLIDChat(store.db, lid, pn.String()); err != nil {
			failed++
			logger.Warnf("LID migration: %s left as is: %v", lid, err)
			continue
		}
		merged++
	}
	// Media moves after the chat's rows; whatever a crash left in between (planned moves, a LID
	// directory whose chat is gone) is finished here on the next start.
	moves := map[string]string{}
	rows, err := store.db.Query(`SELECT DISTINCT lid, pn FROM lid_media_moves`)
	if err != nil {
		logger.Warnf("LID migration: failed to read planned media moves: %v", err)
		rows = nil
	}
	for rows != nil && rows.Next() {
		var lid, pn string
		if rows.Scan(&lid, &pn) == nil {
			moves[lid] = pn
		}
	}
	if rows != nil {
		rows.Close()
	}
	lidDirs, _ := filepath.Glob(filepath.Join("store", "*@"+types.HiddenUserServer))
	for _, dir := range lidDirs {
		lid := filepath.Base(dir)
		lidJID, err := types.ParseJID(lid)
		pn := resolveToPN(client, lidJID)
		if _, planned := moves[lid]; planned || err != nil || pn.Server != types.DefaultUserServer || chatExists(store.db, lid) {
			continue
		}
		moves[lid] = pn.String()
	}
	for lid, pn := range moves {
		if err := moveChatMedia(store.db, lid, pn); err != nil {
			logger.Warnf("LID migration: media of %s not fully moved: %v", lid, err)
		}
	}
	senders, err := migrateLIDSenders(client, store.db)
	if err != nil {
		logger.Warnf("LID migration: failed to rewrite LID authors: %v", err)
	}
	if merged+unmapped+failed+senders > 0 {
		logger.Infof("LID migration: %d chat(s) merged, %d without a known number yet, %d failed; %d LID author(s) rewritten",
			merged, unmapped, failed, senders)
	}
}

// mergeLIDChat moves everything stored under the LID chat to the phone-number chat. A message
// present under both keeps the phone-number row and fills its empty fields from the LID copy, so
// a transcription that only the LID copy has survives.
func mergeLIDChat(db *sql.DB, lid, pn string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	steps := []string{
		// The merged row keeps the phone-number copy's filename when it has one (below).
		`INSERT OR REPLACE INTO lid_media_moves (lid, pn, src, dst)
			SELECT ?1, ?2, l.id || '_' || l.filename, l.id || '_' || COALESCE(NULLIF(p.filename, ''), l.filename)
			FROM messages l LEFT JOIN messages p ON p.chat_jid = ?2 AND p.id = l.id
			WHERE l.chat_jid = ?1 AND COALESCE(l.filename, '') != ''`,
		`INSERT INTO chats (jid, name, last_message_time)
			SELECT ?2, name, last_message_time FROM chats WHERE jid = ?1
			ON CONFLICT(jid) DO UPDATE SET
				name = COALESCE(NULLIF(chats.name, ''), excluded.name),
				last_message_time = CASE
					WHEN julianday(chats.last_message_time) IS NULL
						OR julianday(excluded.last_message_time) > julianday(chats.last_message_time)
					THEN excluded.last_message_time ELSE chats.last_message_time END`,
		`UPDATE messages SET
				sender = COALESCE(NULLIF(messages.sender, ''), l.sender),
				content = COALESCE(NULLIF(messages.content, ''), l.content),
				media_type = COALESCE(NULLIF(messages.media_type, ''), l.media_type),
				filename = COALESCE(NULLIF(messages.filename, ''), l.filename),
				url = COALESCE(NULLIF(messages.url, ''), l.url),
				media_key = COALESCE(NULLIF(messages.media_key, x''), l.media_key),
				file_sha256 = COALESCE(NULLIF(messages.file_sha256, x''), l.file_sha256),
				file_enc_sha256 = COALESCE(NULLIF(messages.file_enc_sha256, x''), l.file_enc_sha256),
				file_length = COALESCE(NULLIF(messages.file_length, 0), l.file_length)
			FROM (SELECT * FROM messages WHERE chat_jid = ?1) AS l
			WHERE messages.chat_jid = ?2 AND messages.id = l.id`,
		`UPDATE OR IGNORE messages SET chat_jid = ?2 WHERE chat_jid = ?1`,
		`DELETE FROM messages WHERE chat_jid = ?1`,
		`UPDATE OR IGNORE message_mentions SET chat_jid = ?2 WHERE chat_jid = ?1`,
		`DELETE FROM message_mentions WHERE chat_jid = ?1`,
		`UPDATE OR IGNORE polls SET chat_jid = ?2 WHERE chat_jid = ?1`,
		`DELETE FROM polls WHERE chat_jid = ?1`,
		// A voter's newer vote wins, whichever key it was stored under.
		`DELETE FROM poll_votes AS p WHERE chat_jid = ?2 AND EXISTS (SELECT 1 FROM poll_votes l
				WHERE l.chat_jid = ?1 AND l.poll_id = p.poll_id AND l.voter = p.voter
				AND julianday(l.timestamp) > julianday(p.timestamp))`,
		`UPDATE OR IGNORE poll_votes SET chat_jid = ?2 WHERE chat_jid = ?1`,
		`DELETE FROM poll_votes WHERE chat_jid = ?1`,
		`UPDATE OR IGNORE favorites SET chat_jid = ?2 WHERE chat_jid = ?1`, // a decision on the number wins
		`DELETE FROM favorites WHERE chat_jid = ?1`,
		`UPDATE matters SET chat_jid = ?2 WHERE chat_jid = ?1`,
		`UPDATE drafts SET chat_jid = ?2 WHERE chat_jid = ?1`,
		`DELETE FROM chats WHERE jid = ?1`,
	}
	for _, q := range steps {
		if _, err := tx.Exec(q, lid, pn); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// moveChatMedia moves downloaded media from the LID chat's directory to the phone-number one, so
// the cache (often the only copy once the CDN expired) stays where downloadMedia looks: under
// <id>_<filename> of the merged row, as planned by mergeLIDChat. Unplanned files keep their name.
func moveChatMedia(db *sql.DB, lid, pn string) error {
	planned := map[string]string{}
	rows, err := db.Query(`SELECT src, dst FROM lid_media_moves WHERE lid = ?`, lid)
	if err != nil {
		return err
	}
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			rows.Close()
			return err
		}
		planned[src] = dst
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	from := filepath.Join("store", strings.ReplaceAll(lid, ":", "_"))
	to := filepath.Join("store", strings.ReplaceAll(pn, ":", "_"))
	entries, err := os.ReadDir(from)
	if os.IsNotExist(err) {
		_, err = db.Exec(`DELETE FROM lid_media_moves WHERE lid = ?`, lid)
		return err
	} else if err != nil {
		return err
	}
	if err := os.MkdirAll(to, 0755); err != nil {
		return err
	}
	var kept int
	for _, e := range entries {
		name := e.Name()
		if d, ok := planned[name]; ok && !strings.ContainsAny(d, `/\`) && d != ".." {
			name = d
		}
		dst := filepath.Join(to, name)
		if _, err := os.Lstat(dst); err == nil {
			kept++
			continue
		}
		if err := os.Rename(filepath.Join(from, e.Name()), dst); err != nil {
			return err
		}
	}
	if kept > 0 {
		return fmt.Errorf("%d file(s) already present under %s, left in %s", kept, to, from)
	}
	if err := os.Remove(from); err != nil {
		return err
	}
	_, err = db.Exec(`DELETE FROM lid_media_moves WHERE lid = ?`, lid)
	return err
}

func chatExists(db *sql.DB, jid string) bool {
	var n int
	return db.QueryRow(`SELECT COUNT(*) FROM chats WHERE jid = ?`, jid).Scan(&n) == nil && n > 0
}

// migrateLIDSenders rewrites authors and poll voters stored as a LID (whole JID, or a bare user
// from before senderKey) to the phone number once the mapping is known.
func migrateLIDSenders(client *whatsmeow.Client, db *sql.DB) (int, error) {
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return 0, nil
	}
	lidLike := `LIKE '%@` + types.HiddenUserServer + `'`
	senders, err := queryStrings(db, `SELECT sender FROM messages WHERE sender != '' AND (sender `+lidLike+` OR sender NOT LIKE '%@%')
		UNION SELECT voter FROM poll_votes WHERE voter != '' AND (voter `+lidLike+` OR voter NOT LIKE '%@%')`)
	if err != nil {
		return 0, err
	}
	rewritten := 0
	for _, s := range senders {
		lid := types.JID{User: strings.TrimSuffix(s, "@"+types.HiddenUserServer), Server: types.HiddenUserServer}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		pn, err := client.Store.LIDs.GetPNForLID(ctx, lid)
		cancel()
		if err != nil || pn.IsEmpty() {
			if s == lid.String() {
				if err := adoptLegacyLIDAuthor(db, lid.User, s); err != nil {
					return rewritten, err
				}
			}
			continue
		}
		if err := rewriteLIDAuthor(db, s, pn.User); err != nil {
			return rewritten, err
		}
		rewritten++
	}
	return rewritten, nil
}

// adoptLegacyLIDAuthor gives rows stored before senderKey (a bare LID user) the LID key, in the
// chats where that LID already appears, so one person is not counted twice while the phone
// number is still unknown.
func adoptLegacyLIDAuthor(db *sql.DB, bare, lid string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	inChats := `chat_jid IN (SELECT chat_jid FROM messages WHERE sender = ?2 UNION SELECT chat_jid FROM poll_votes WHERE voter = ?2)`
	for _, q := range []string{
		`UPDATE messages SET sender = ?2 WHERE sender = ?1 AND ` + inChats,
		`DELETE FROM poll_votes AS o WHERE voter = ?1 AND EXISTS (SELECT 1 FROM poll_votes n
			WHERE n.poll_id = o.poll_id AND n.chat_jid = o.chat_jid AND n.voter = ?2
			AND julianday(n.timestamp) >= julianday(o.timestamp))`,
		`UPDATE OR REPLACE poll_votes SET voter = ?2 WHERE voter = ?1 AND ` + inChats,
	} {
		if _, err := tx.Exec(q, bare, lid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// storePollVote keeps one vote per voter on a poll, the newest by its own timestamp, so a
// delayed older vote never replaces a newer one. A vote stored before senderKey under the bare LID
// user belongs to the same voter as the LID key: the two merge first, in the same transaction.
func storePollVote(db *sql.DB, pollID, chatJID, voter, selected string, at time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if legacy := strings.TrimSuffix(voter, "@"+types.HiddenUserServer); legacy != voter {
		for _, q := range []string{
			`DELETE FROM poll_votes AS o WHERE poll_id = ?1 AND chat_jid = ?2 AND voter = ?3 AND EXISTS (
				SELECT 1 FROM poll_votes n WHERE n.poll_id = ?1 AND n.chat_jid = ?2 AND n.voter = ?4
				AND julianday(n.timestamp) >= julianday(o.timestamp))`,
			`UPDATE OR REPLACE poll_votes SET voter = ?4 WHERE poll_id = ?1 AND chat_jid = ?2 AND voter = ?3`,
		} {
			if _, err := tx.Exec(q, pollID, chatJID, legacy, voter); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO poll_votes (poll_id, chat_jid, voter, selected, timestamp) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(poll_id, chat_jid, voter) DO UPDATE SET selected = excluded.selected, timestamp = excluded.timestamp
		WHERE julianday(excluded.timestamp) >= julianday(poll_votes.timestamp)`,
		pollID, chatJID, voter, selected, at); err != nil {
		return err
	}
	return tx.Commit()
}

// rewriteLIDAuthor moves one author to the phone number; of two votes on one poll, the newer stays.
func rewriteLIDAuthor(db *sql.DB, from, to string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`UPDATE messages SET sender = ?2 WHERE sender = ?1`,
		`DELETE FROM poll_votes AS o WHERE voter = ?1 AND EXISTS (SELECT 1 FROM poll_votes n
			WHERE n.poll_id = o.poll_id AND n.chat_jid = o.chat_jid AND n.voter = ?2
			AND julianday(n.timestamp) >= julianday(o.timestamp))`,
		`UPDATE OR REPLACE poll_votes SET voter = ?2 WHERE voter = ?1`,
	} {
		if _, err := tx.Exec(q, from, to); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func queryStrings(db *sql.DB, query string, args ...any) ([]string, error) {
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
