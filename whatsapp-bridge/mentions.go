package main

import (
	"database/sql"
	"strings"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
)

// Who each message @-mentions. In a group only a message that mentions the
// owner is something addressed to them, so only those are tracked as matters.
const mentionsSchema = `
	CREATE TABLE IF NOT EXISTS message_mentions (
		chat_jid TEXT NOT NULL,
		message_id TEXT NOT NULL,
		mentioned_user TEXT NOT NULL,
		PRIMARY KEY (chat_jid, message_id, mentioned_user)
	);
`

// mentionedJIDs returns the JIDs a message @-mentions (text and media captions).
func mentionedJIDs(m *waProto.Message) []string {
	switch {
	case m == nil:
		return nil
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetContextInfo().GetMentionedJID()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetContextInfo().GetMentionedJID()
	case m.GetVideoMessage() != nil:
		return m.GetVideoMessage().GetContextInfo().GetMentionedJID()
	case m.GetDocumentMessage() != nil:
		return m.GetDocumentMessage().GetContextInfo().GetMentionedJID()
	case m.GetAudioMessage() != nil:
		return m.GetAudioMessage().GetContextInfo().GetMentionedJID()
	}
	return nil
}

// storeMentions records the user part of each mentioned JID (a device suffix
// like "999:5" is dropped), once per message.
func storeMentions(db *sql.DB, chat, messageID string, jids []string) error {
	for _, jid := range jids {
		user, _, _ := strings.Cut(jidUser(jid), ":")
		if _, err := db.Exec(`INSERT OR IGNORE INTO message_mentions (chat_jid, message_id, mentioned_user) VALUES (?, ?, ?)`, chat, messageID, user); err != nil {
			return err
		}
	}
	return nil
}

func mentionsOwner(db *sql.DB, chat, messageID string, selfUsers []string) (bool, error) {
	if len(selfUsers) == 0 {
		return false, nil
	}
	args := []interface{}{chat, messageID}
	for _, u := range selfUsers {
		args = append(args, u)
	}
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM message_mentions WHERE chat_jid = ? AND message_id = ? AND mentioned_user IN (?`+strings.Repeat(",?", len(selfUsers)-1)+`)`, args...).Scan(&n)
	return n > 0, err
}
