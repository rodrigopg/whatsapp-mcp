package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

type PollVoteRequest struct {
	ChatJID   string   `json:"chat_jid"`
	MessageID string   `json:"message_id"`
	Options   []string `json:"options"`
}

// validateVote checks a vote against the poll's own options (exact names) and its limit, and returns the
// trimmed choice. An empty choice is allowed: it retracts the voter's vote.
func validateVote(options []string, selectable int, chosen []string) ([]string, error) {
	known := map[string]bool{}
	for _, o := range options {
		known[o] = true
	}
	out, seen := []string{}, map[string]bool{}
	for _, c := range chosen {
		c = strings.TrimSpace(c)
		if !known[c] {
			return nil, invalid("%q is not an option of this poll (options: %s)", c, strings.Join(options, ", "))
		}
		if seen[c] {
			return nil, invalid("option %q chosen twice", c)
		}
		seen[c], out = true, append(out, c)
	}
	if selectable < 1 {
		selectable = 1
	}
	if len(out) > selectable {
		return nil, invalid("this poll allows at most %d option(s), got %d", selectable, len(out))
	}
	return out, nil
}

// handlePollVote returns the handler for POST /api/poll_vote: cast (or retract, with no options) the
// account's own vote in a poll this bridge has seen. The vote is also recorded locally, since own sends
// do not echo back.
func handlePollVote(client *whatsmeow.Client, store *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		var req PollVoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" || req.MessageID == "" {
			writeJSONError(w, http.StatusBadRequest, "Invalid request: chat_jid, message_id and options required")
			return
		}
		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Invalid chat_jid: %v", err))
			return
		}
		if client == nil || !client.IsConnected() || client.Store == nil || client.Store.ID == nil {
			writeJSONError(w, http.StatusServiceUnavailable, "WhatsApp client not connected")
			return
		}
		var optsJSON string
		var selectable int
		err = store.db.QueryRow(`SELECT options, selectable_count FROM polls WHERE id = ? AND chat_jid = ?`, req.MessageID, chatJID.String()).Scan(&optsJSON, &selectable)
		if errors.Is(err, sql.ErrNoRows) {
			writeJSONError(w, http.StatusBadRequest, "poll not found: this bridge has not seen that poll")
			return
		}
		var options []string
		if err != nil || json.Unmarshal([]byte(optsJSON), &options) != nil {
			writeJSONError(w, http.StatusInternalServerError, "could not read the poll")
			return
		}
		chosen, err := validateVote(options, selectable, req.Options)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		var sender sql.NullString
		var fromMe bool
		if err := store.db.QueryRow(`SELECT sender, is_from_me FROM messages WHERE id = ? AND chat_jid = ?`, req.MessageID, chatJID.String()).Scan(&sender, &fromMe); err != nil {
			writeJSONError(w, http.StatusBadRequest, "poll message not found in local history")
			return
		}
		creator := types.JID{User: sender.String, Server: types.DefaultUserServer}
		if fromMe {
			creator = client.Store.ID.ToNonAD()
		}
		info := types.MessageInfo{MessageSource: types.MessageSource{Chat: chatJID, Sender: creator, IsFromMe: fromMe, IsGroup: chatJID.Server == types.GroupServer}, ID: req.MessageID}
		ctx := context.Background()
		msg, err := client.BuildPollVote(ctx, &info, chosen)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("BuildPollVote error: %v", err))
			return
		}
		resp, err := client.SendMessage(ctx, chatJID, msg)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("SendMessage error: %v", err))
			return
		}
		sel, _ := json.Marshal(chosen)
		if _, err := store.db.Exec(`INSERT OR REPLACE INTO poll_votes (poll_id, chat_jid, voter, selected, timestamp) VALUES (?, ?, ?, ?, ?)`,
			req.MessageID, chatJID.String(), client.Store.ID.User, string(sel), resp.Timestamp); err != nil {
			fmt.Printf("Failed to record own poll vote: %v\n", err)
		}
		writeJSON(w, PollResponse{Success: true, Message: "Vote sent", MessageID: req.MessageID})
	}
}
