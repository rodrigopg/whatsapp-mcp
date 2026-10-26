package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

type historySyncClient interface {
	IsConnected() bool
	BuildHistorySyncRequest(*types.MessageInfo, int) *waE2E.Message
	SendPeerMessage(context.Context, *waE2E.Message) (whatsmeow.SendResponse, error)
}

func handleChatBackfill(client historySyncClient, store *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		var req struct {
			ChatJID string `json:"chat_jid"`
			Count   int    `json:"count"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		jid, err := types.ParseJID(req.ChatJID)
		if err != nil || jid.User == "" || jid.Device != 0 || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer && jid.Server != types.GroupServer) {
			writeJSONError(w, http.StatusBadRequest, "chat_jid must be a user or group JID")
			return
		}
		if req.Count < 1 || req.Count > 50 {
			writeJSONError(w, http.StatusBadRequest, "count must be between 1 and 50")
			return
		}
		if client == nil || !client.IsConnected() {
			writeJSONError(w, http.StatusServiceUnavailable, "WhatsApp client not connected")
			return
		}
		oldest := types.MessageInfo{MessageSource: types.MessageSource{Chat: jid}}
		err = store.db.QueryRowContext(r.Context(), `SELECT id, is_from_me, timestamp FROM messages WHERE chat_jid = ? ORDER BY julianday(timestamp), rowid LIMIT 1`, jid.String()).Scan(&oldest.ID, &oldest.IsFromMe, &oldest.Timestamp)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusNotFound, "No stored messages for this chat; cannot request earlier history")
			return
		}
		if err != nil {
			fmt.Printf("Chat backfill: failed to read oldest stored message: %v\n", err)
			writeJSONError(w, http.StatusInternalServerError, "Failed to read oldest stored message")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 75*time.Second)
		defer cancel()
		if err := requestHistorySync(ctx, client, &oldest, req.Count); err != nil {
			fmt.Printf("Chat backfill: failed to request chat history from phone: %v\n", err)
			writeJSONError(w, http.StatusInternalServerError, "Failed to request chat history from phone")
			return
		}
		writeJSON(w, MarkChatResponse{Success: true, Message: "History requested from phone; messages arrive asynchronously. Use list_messages to check for earlier history"})
	}
}
