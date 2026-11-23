package main

import "database/sql"

func removeStatusBroadcast(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM drafts WHERE chat_jid = ?1 OR matter_id IN (SELECT id FROM matters WHERE chat_jid = ?1)`,
		`DELETE FROM matter_events WHERE matter_id IN (SELECT id FROM matters WHERE chat_jid = ?1)`,
		`DELETE FROM matters WHERE chat_jid = ?1`,
		`DELETE FROM favorites WHERE chat_jid = ?1`,
		`DELETE FROM poll_votes WHERE chat_jid = ?1`,
		`DELETE FROM polls WHERE chat_jid = ?1`,
		`DELETE FROM message_mentions WHERE chat_jid = ?1`,
		`DELETE FROM reactions WHERE chat_jid = ?1`,
		`DELETE FROM lid_media_moves WHERE lid = ?1 OR pn = ?1`,
		`DELETE FROM messages WHERE chat_jid = ?1`,
		`DELETE FROM chats WHERE jid = ?1`,
		`DELETE FROM senders WHERE jid = ?1`,
	} {
		if _, err := tx.Exec(q, "status@broadcast"); err != nil {
			return err
		}
	}
	return tx.Commit()
}
