package main

import (
	"database/sql"
	"strings"
	"time"
)

// Reactions can arrive before their target. Keep removals as tombstones so an older
// history delivery cannot resurrect them.
const reactionsSchema = `
	CREATE TABLE IF NOT EXISTS reactions (
		chat_jid TEXT NOT NULL,
		message_id TEXT NOT NULL,
		sender TEXT NOT NULL,
		emoji TEXT NOT NULL,
		timestamp TIMESTAMP NOT NULL,
		PRIMARY KEY (chat_jid, message_id, sender)
	);
`

type APIReaction struct {
	Sender    string    `json:"sender"`
	Emoji     string    `json:"emoji"`
	Timestamp time.Time `json:"timestamp"`
}

func (store *MessageStore) StoreReaction(chat, id, sender, emoji string, millis int64, at time.Time) error {
	if chat == "status@broadcast" || id == "" || sender == "" {
		return nil
	}
	if millis > 0 {
		at = time.UnixMilli(millis)
	}
	_, err := store.db.Exec(`INSERT INTO reactions (chat_jid, message_id, sender, emoji, timestamp) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(chat_jid, message_id, sender) DO UPDATE SET emoji = excluded.emoji, timestamp = excluded.timestamp
		WHERE julianday(excluded.timestamp) >= julianday(reactions.timestamp)`, chat, id, sender, emoji, at)
	return err
}

// loadReactions attaches live reactions to messages, in batches: list_messages has no upper
// limit and SQLite caps the number of bound parameters.
func loadReactions(db *sql.DB, messages []APIMessage) error {
	const batch = 500
	indices := make(map[[2]string]int, len(messages))
	for i, message := range messages {
		indices[[2]string{message.ChatJID, message.ID}] = i
	}
	for start := 0; start < len(messages); start += batch {
		part := messages[start:min(start+batch, len(messages))]
		args := make([]any, 0, len(part)*2)
		for _, message := range part {
			args = append(args, message.ChatJID, message.ID)
		}
		rows, err := db.Query(`SELECT chat_jid, message_id, sender, emoji, timestamp FROM reactions
			WHERE emoji != '' AND (chat_jid, message_id) IN (VALUES (?, ?)`+strings.Repeat(", (?, ?)", len(part)-1)+`) ORDER BY sender`, args...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var chat, id string
			var reaction APIReaction
			if err := rows.Scan(&chat, &id, &reaction.Sender, &reaction.Emoji, &reaction.Timestamp); err != nil {
				rows.Close()
				return err
			}
			i := indices[[2]string{chat, id}]
			messages[i].Reactions = append(messages[i].Reactions, reaction)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
	}
	return nil
}
