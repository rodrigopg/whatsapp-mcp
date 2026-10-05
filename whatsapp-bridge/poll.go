package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

const (
	minPollOptions   = 2
	maxPollOptions   = 12
	maxPollQuestion  = 255
	maxPollOptionLen = 100
)

// Additive tables: only polls whose creation message passed through this
// bridge are tracked, since option names are needed to decode vote hashes.
const pollSchema = `
	CREATE TABLE IF NOT EXISTS polls (
		id TEXT,
		chat_jid TEXT,
		question TEXT,
		options TEXT,
		selectable_count INTEGER,
		PRIMARY KEY (id, chat_jid)
	);
	CREATE TABLE IF NOT EXISTS poll_votes (
		poll_id TEXT,
		chat_jid TEXT,
		voter TEXT,
		selected TEXT,
		timestamp TIMESTAMP,
		PRIMARY KEY (poll_id, chat_jid, voter)
	);
`

// validatePoll checks a poll request and returns the effective selectable
// count (0 defaults to 1) and the trimmed options.
func validatePoll(question string, options []string, selectable int) (int, []string, error) {
	question = strings.TrimSpace(question)
	if question == "" || utf8.RuneCountInString(question) > maxPollQuestion {
		return 0, nil, fmt.Errorf("question is required and must be at most %d characters", maxPollQuestion)
	}
	if len(options) < minPollOptions || len(options) > maxPollOptions {
		return 0, nil, fmt.Errorf("a poll needs %d to %d options", minPollOptions, maxPollOptions)
	}
	seen := map[string]bool{}
	clean := make([]string, len(options))
	for i, o := range options {
		o = strings.TrimSpace(o)
		if o == "" || utf8.RuneCountInString(o) > maxPollOptionLen {
			return 0, nil, fmt.Errorf("each option must be 1 to %d characters", maxPollOptionLen)
		}
		if seen[o] {
			return 0, nil, fmt.Errorf("duplicate option %q", o)
		}
		seen[o] = true
		clean[i] = o
	}
	if selectable == 0 {
		selectable = 1
	}
	if selectable < 1 || selectable > len(clean) {
		return 0, nil, fmt.Errorf("selectable_count must be between 1 and %d", len(clean))
	}
	return selectable, clean, nil
}

// pollCreation returns the poll creation payload of any WhatsApp version.
func pollCreation(msg *waE2E.Message) *waE2E.PollCreationMessage {
	for _, p := range []*waE2E.PollCreationMessage{msg.GetPollCreationMessage(), msg.GetPollCreationMessageV2(), msg.GetPollCreationMessageV3()} {
		if p != nil {
			return p
		}
	}
	return nil
}

func pollSearchText(question string) string { return "[Poll] " + question }

func storePoll(db *sql.DB, id, chatJID, question string, options []string, selectable int) error {
	opts, _ := json.Marshal(options)
	_, err := db.Exec(`INSERT OR IGNORE INTO polls (id, chat_jid, question, options, selectable_count) VALUES (?, ?, ?, ?, ?)`,
		id, chatJID, question, string(opts), selectable)
	return err
}

// optionsForHashes maps the SHA-256 option hashes of a vote back to names.
func optionsForHashes(options []string, hashes [][]byte) []string {
	byHash := map[string]string{}
	for _, o := range options {
		sum := sha256.Sum256([]byte(o))
		byHash[string(sum[:])] = o
	}
	selected := []string{}
	for _, h := range hashes {
		if name, ok := byHash[string(h)]; ok {
			selected = append(selected, name)
		}
	}
	return selected
}

// recordPollVote decrypts an incoming vote and stores it (latest vote per voter wins).
func recordPollVote(client *whatsmeow.Client, store *MessageStore, msg *events.Message, chatJID, voter string, logger waLog.Logger) {
	pollID := msg.Message.GetPollUpdateMessage().GetPollCreationMessageKey().GetID()
	var optsJSON string
	if err := store.db.QueryRow(`SELECT options FROM polls WHERE id = ? AND chat_jid = ?`, pollID, chatJID).Scan(&optsJSON); err != nil {
		return // poll not tracked locally
	}
	var options []string
	if json.Unmarshal([]byte(optsJSON), &options) != nil {
		return
	}
	vote, err := client.DecryptPollVote(context.Background(), msg)
	if err != nil {
		logger.Warnf("Failed to decrypt poll vote for %s: %v", pollID, err)
		return
	}
	selected, _ := json.Marshal(optionsForHashes(options, vote.GetSelectedOptions()))
	if _, err := store.db.Exec(`INSERT OR REPLACE INTO poll_votes (poll_id, chat_jid, voter, selected, timestamp) VALUES (?, ?, ?, ?, ?)`,
		pollID, chatJID, voter, string(selected), msg.Info.Timestamp); err != nil {
		logger.Warnf("Failed to store poll vote: %v", err)
	}
}

type PollRequest struct {
	ChatJID         string   `json:"chat_jid"`
	Question        string   `json:"question"`
	Options         []string `json:"options"`
	SelectableCount int      `json:"selectable_count"`
}

type PollResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	MessageID string `json:"message_id,omitempty"`
}

type PollVotesRequest struct {
	ChatJID   string `json:"chat_jid"`
	MessageID string `json:"message_id"`
}

type PollVote struct {
	Voter     string    `json:"voter"`
	Selected  []string  `json:"selected"`
	Timestamp time.Time `json:"timestamp"`
}

type PollVotesResponse struct {
	Success         bool           `json:"success"`
	Question        string         `json:"question"`
	Options         []string       `json:"options"`
	SelectableCount int            `json:"selectable_count"`
	Tally           map[string]int `json:"tally"`
	Votes           []PollVote     `json:"votes"`
}

var errPollNotFound = errors.New("poll not found in the local database")

func getPollVotes(db *sql.DB, chatJID, pollID string) (*PollVotesResponse, error) {
	resp := &PollVotesResponse{Success: true, Tally: map[string]int{}, Votes: []PollVote{}}
	var optsJSON string
	err := db.QueryRow(`SELECT question, options, selectable_count FROM polls WHERE id = ? AND chat_jid = ?`, pollID, chatJID).
		Scan(&resp.Question, &optsJSON, &resp.SelectableCount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errPollNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(optsJSON), &resp.Options); err != nil {
		return nil, err
	}
	for _, o := range resp.Options {
		resp.Tally[o] = 0
	}
	rows, err := db.Query(`SELECT voter, selected, timestamp FROM poll_votes WHERE poll_id = ? AND chat_jid = ? ORDER BY timestamp`, pollID, chatJID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var v PollVote
		var sel string
		if err := rows.Scan(&v.Voter, &sel, &v.Timestamp); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(sel), &v.Selected) != nil {
			continue
		}
		for _, o := range v.Selected {
			resp.Tally[o]++
		}
		resp.Votes = append(resp.Votes, v)
	}
	return resp, rows.Err()
}

// handlePoll returns the handler for POST /api/poll (create a poll).
func handlePoll(client *whatsmeow.Client, store *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		var req PollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" {
			writeJSONError(w, http.StatusBadRequest, "Invalid request: chat_jid, question and options required")
			return
		}
		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Invalid chat_jid: %v", err))
			return
		}
		selectable, options, err := validatePoll(req.Question, req.Options, req.SelectableCount)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		if client == nil || !client.IsConnected() {
			writeJSONError(w, http.StatusServiceUnavailable, "WhatsApp client not connected")
			return
		}
		question := strings.TrimSpace(req.Question)
		resp, err := client.SendMessage(context.Background(), chatJID, client.BuildPollCreation(question, options, selectable))
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("SendMessage error: %v", err))
			return
		}
		// Own sends don't echo back through handleMessage on single-device accounts.
		if client.Store != nil && client.Store.ID != nil {
			chat := chatJID.String()
			if err := store.EnsureChat(chat, resp.Timestamp); err == nil {
				_ = store.StoreMessage(resp.ID, chat, client.Store.ID.User, pollSearchText(question), resp.Timestamp, true, "", "", "", nil, nil, nil, 0)
				_ = store.TouchChatLastMessageTime(chat, resp.Timestamp)
			}
			if err := storePoll(store.db, resp.ID, chat, question, options, selectable); err != nil {
				fmt.Printf("Failed to persist poll: %v\n", err)
			}
		}
		writeJSON(w, PollResponse{Success: true, Message: "Poll sent", MessageID: resp.ID})
	}
}

// handlePollVotes returns the handler for POST /api/poll_votes (read votes).
func handlePollVotes(store *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		var req PollVotesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" || req.MessageID == "" {
			writeJSONError(w, http.StatusBadRequest, "Invalid request: chat_jid and message_id required")
			return
		}
		if _, err := types.ParseJID(req.ChatJID); err != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Invalid chat_jid: %v", err))
			return
		}
		resp, err := getPollVotes(store.db, req.ChatJID, req.MessageID)
		if errors.Is(err, errPollNotFound) {
			writeJSONError(w, http.StatusNotFound, err.Error())
			return
		}
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, resp)
	}
}
