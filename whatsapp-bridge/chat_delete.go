package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// DeleteChatRequest deletes a chat from WhatsApp (the phone's chat list). It
// is deliberately hard to aim wrong: the caller must also say what the chat's
// name starts with, and the bridge refuses unless that matches, so a mistyped
// JID can never delete someone else's conversation. Built for the live e2e
// gate, which creates throwaway "e2e-..." groups.
type DeleteChatRequest struct {
	ChatJID          string `json:"chat_jid"`
	ExpectNamePrefix string `json:"expect_name_prefix"`
	ForgetLocal      bool   `json:"forget_local"`
}

// chatDeleteTarget checks the name guard and returns what WhatsApp needs to
// delete the chat: its last message (time and key), zero values when none is stored.
func chatDeleteTarget(db *sql.DB, chat, prefix string) (string, time.Time, *waCommon.MessageKey, error) {
	if strings.TrimSpace(prefix) == "" {
		return "", time.Time{}, nil, invalid("expect_name_prefix is required")
	}
	var name sql.NullString
	if err := db.QueryRow(`SELECT name FROM chats WHERE jid = ?`, chat).Scan(&name); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", time.Time{}, nil, invalid("chat %s not found", chat)
		}
		return "", time.Time{}, nil, err
	}
	if !strings.HasPrefix(name.String, prefix) {
		return "", time.Time{}, nil, invalid("chat %s is named %q, which does not start with %q: nothing deleted", chat, name.String, prefix)
	}
	var id, sender sql.NullString
	var ts time.Time
	var fromMe bool
	err := db.QueryRow(`SELECT id, timestamp, is_from_me, sender FROM messages WHERE chat_jid = ? ORDER BY julianday(timestamp) DESC, rowid DESC LIMIT 1`, chat).Scan(&id, &ts, &fromMe, &sender)
	if errors.Is(err, sql.ErrNoRows) {
		return name.String, time.Time{}, nil, nil
	}
	if err != nil {
		return "", time.Time{}, nil, err
	}
	key := &waCommon.MessageKey{RemoteJID: proto.String(chat), FromMe: proto.Bool(fromMe), ID: proto.String(id.String)}
	if !fromMe && strings.HasSuffix(chat, "@g.us") && sender.String != "" {
		if participant, err := parseSender(sender.String); err == nil {
			key.Participant = proto.String(participant.String())
		}
	}
	return name.String, ts, key, nil
}

// forgetChatLocally removes a chat's rows from the local history (messages,
// mentions, the chat itself). Matters that cite it are left alone.
func forgetChatLocally(db *sql.DB, chat string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM read_receipts WHERE chat_jid = ?`, `DELETE FROM reactions WHERE chat_jid = ?`, `DELETE FROM message_mentions WHERE chat_jid = ?`, `DELETE FROM messages WHERE chat_jid = ?`, `DELETE FROM chats WHERE jid = ?`} {
		if _, err := tx.Exec(q, chat); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// handleDeleteChat returns the handler for POST /api/chat/delete.
func handleDeleteChat(client *whatsmeow.Client, store *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		var req DeleteChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		if req.ChatJID == "" || strings.TrimSpace(req.ExpectNamePrefix) == "" {
			writeJSONError(w, http.StatusBadRequest, "chat_jid and expect_name_prefix are required")
			return
		}
		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Invalid chat_jid: %v", err))
			return
		}
		if client == nil || !client.IsConnected() {
			writeJSONError(w, http.StatusServiceUnavailable, "WhatsApp client not connected")
			return
		}
		_, lastTS, key, err := chatDeleteTarget(store.db, chatJID.String(), req.ExpectNamePrefix)
		if err != nil {
			var bad *errInvalidRequest
			if errors.As(err, &bad) {
				writeJSONError(w, http.StatusBadRequest, err.Error())
			} else {
				writeJSONError(w, http.StatusInternalServerError, err.Error())
			}
			return
		}
		if err := safeSendAppState(client, context.Background(), appstate.BuildDeleteChat(chatJID, lastTS, key, true)); err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("AppState error: %v", err))
			return
		}
		if req.ForgetLocal {
			if err := forgetChatLocally(store.db, chatJID.String()); err != nil {
				writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("deleted on WhatsApp, local cleanup failed: %v", err))
				return
			}
		}
		writeJSON(w, MarkChatResponse{Success: true, Message: fmt.Sprintf("Chat %s deleted", req.ChatJID)})
	}
}
