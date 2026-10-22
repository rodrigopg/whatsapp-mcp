package main

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
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

var mentionToken = regexp.MustCompile(`@([0-9]+)\b`)

// withMentions makes a text message @-mention jids. Each must be a user JID
// (phone or LID) whose number appears in text as @<number>: WhatsApp shows that
// token as the person's name, and without it the person would be notified by a
// mention nobody reading the chat can see.
func withMentions(msg *waProto.Message, text string, jids []string) error {
	inText := make(map[string]bool)
	for _, m := range mentionToken.FindAllStringSubmatch(text, -1) {
		inText[m[1]] = true
	}
	var unique []string
	seen := make(map[string]bool)
	for _, raw := range jids {
		jid, err := types.ParseJID(raw)
		if err != nil || jid.ToNonAD().String() != raw || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
			return errors.New("mentions must be user JIDs, as returned by get_group_info or resolve_contact")
		}
		if !inText[jid.User] {
			return errors.New("each mentioned JID needs @<its number> in the message text")
		}
		if !seen[raw] {
			seen[raw] = true
			unique = append(unique, raw)
		}
	}
	if msg.GetExtendedTextMessage() == nil {
		msg.ExtendedTextMessage = &waProto.ExtendedTextMessage{Text: proto.String(text)}
		msg.Conversation = nil
	}
	if msg.ExtendedTextMessage.ContextInfo == nil {
		msg.ExtendedTextMessage.ContextInfo = &waProto.ContextInfo{}
	}
	msg.ExtendedTextMessage.ContextInfo.MentionedJID = unique
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
