package main

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// No foreign key: an action can arrive in a chunk before its original message.
const messageEditsSchema = `
CREATE TABLE IF NOT EXISTS message_edits (
	chat_jid TEXT NOT NULL,
	message_id TEXT NOT NULL,
	is_from_me BOOLEAN NOT NULL,
	sender TEXT NOT NULL,
	content TEXT NOT NULL,
	timestamp TIMESTAMP NOT NULL,
	PRIMARY KEY (chat_jid, message_id, is_from_me, sender)
);
`

type messageSecretReader struct {
	store.MsgSecretStore
	err error
}

func (s *messageSecretReader) GetMessageSecret(ctx context.Context, chat, sender types.JID, id types.MessageID) ([]byte, types.JID, error) {
	secret, sender, err := s.MsgSecretStore.GetMessageSecret(ctx, chat, sender, id)
	s.err = err
	return secret, sender, err
}

func decryptIncomingMessage(client *whatsmeow.Client, msg *events.Message, logger waLog.Logger) (*events.Message, error) {
	encrypted := msg.Message.GetSecretEncryptedMessage()
	if encrypted == nil && msg.Message.GetEncReactionMessage() == nil {
		return msg, nil
	}
	// Event and poll edits decrypt to a whole event or poll; stored as is they would duplicate it.
	if encrypted != nil && encrypted.SecretEncType != nil && encrypted.GetSecretEncType() != waE2E.SecretEncryptedMessage_MESSAGE_EDIT {
		logger.Infof("Skipping unsupported encrypted %s", encrypted.GetSecretEncType())
		return nil, nil
	}
	var body *waProto.Message
	var err error
	if client == nil || client.Store == nil || client.Store.MsgSecrets == nil || (encrypted != nil && encrypted.SecretEncType == nil) {
		logger.Warnf("Cannot decrypt encrypted message; skipping")
		return nil, nil
	}
	// whatsmeow wraps store and crypto errors alike; observe reads without mutating the shared client.
	secrets := &messageSecretReader{MsgSecretStore: client.Store.MsgSecrets}
	decryptor := &whatsmeow.Client{Store: &store.Device{MsgSecrets: secrets}}
	if encrypted != nil {
		body, err = decryptor.DecryptSecretEncryptedMessage(context.Background(), msg)
		if err == nil {
			body = unwrapHistoryMessage(body)
			if body.GetProtocolMessage() == nil {
				body = &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
					Type: waProto.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: encrypted.GetTargetMessageKey(), EditedMessage: body,
				}}
			}
		}
	} else {
		var reaction *waProto.ReactionMessage
		reaction, err = decryptor.DecryptReaction(context.Background(), msg)
		if err == nil {
			reaction.Key = msg.Message.GetEncReactionMessage().GetTargetMessageKey()
		}
		body = &waProto.Message{ReactionMessage: reaction}
	}
	if err != nil {
		if secrets.err != nil {
			logger.Warnf("Cannot read message secret; retrying: %v", err)
			return nil, err
		}
		logger.Warnf("Cannot decrypt encrypted message; skipping: %v", err)
		return nil, nil
	}
	decoded := *msg
	decoded.Message = body
	return &decoded, nil
}

func protocolTimestamp(m *waProto.Message, fallback time.Time) time.Time {
	if millis := m.GetProtocolMessage().GetTimestampMS(); millis > 0 {
		return time.UnixMilli(millis)
	}
	return fallback
}

const messageEditConflictSQL = `
	ON CONFLICT(chat_jid, message_id, is_from_me, sender) DO UPDATE SET content = excluded.content, timestamp = excluded.timestamp
	WHERE message_edits.content != '` + revokedContent + `' AND (excluded.content = '` + revokedContent + `'
		OR julianday(excluded.timestamp) >= julianday(message_edits.timestamp))`

func (store *MessageStore) ApplyEdit(id, chatJID, content string, fromMe bool, sender string, timestamps ...time.Time) error {
	if id == "" || chatJID == "status@broadcast" || strings.TrimSpace(content) == "" || (!fromMe && sender == "") {
		return nil
	}
	if fromMe {
		sender = ""
	}
	at := time.Now()
	if len(timestamps) > 0 {
		at = timestamps[0]
	}
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT INTO message_edits (chat_jid, message_id, is_from_me, sender, content, timestamp) VALUES (?, ?, ?, ?, ?, ?)`+messageEditConflictSQL,
		chatJID, id, fromMe, sender, content, at)
	if err != nil {
		return err
	}
	if err = applyRecordedEdit(tx, id, chatJID); err != nil {
		return err
	}
	return tx.Commit()
}

// Legacy rows can store a bare LID. Actions stay separated by author so a forged action
// received before the original cannot hide its author's legitimate action.
const recordedEditSQL = `UPDATE messages SET content = COALESCE((
	SELECT e.content FROM message_edits e
	WHERE e.chat_jid = messages.chat_jid AND e.message_id = messages.id AND e.is_from_me = messages.is_from_me
		AND (e.is_from_me = 1 OR messages.sender = e.sender OR
			(e.sender LIKE '%@lid' AND messages.sender = substr(e.sender, 1, length(e.sender) - 4)))
		AND (e.content = ?1 OR COALESCE(messages.media_type, '') != 'audio')
	ORDER BY (e.content = ?1) DESC, julianday(e.timestamp) DESC LIMIT 1
), content) WHERE COALESCE(content, '') != ?1`

func applyRecordedEdit(tx *sql.Tx, id, chat string) error {
	_, err := tx.Exec(recordedEditSQL+" AND id = ?2 AND chat_jid = ?3", revokedContent, id, chat)
	return err
}
