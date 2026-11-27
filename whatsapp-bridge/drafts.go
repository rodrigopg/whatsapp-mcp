package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"go.mau.fi/whatsmeow"
)

// Drafts of chase messages: the AI writes, the owner decides. Nothing is ever
// sent except through approveDraft, which sends exactly the text it is given.
// State: written (pending) -> sending -> sent, or discarded.
const draftsSchema = `
	CREATE TABLE IF NOT EXISTS drafts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		matter_id INTEGER NOT NULL,
		chat_jid TEXT NOT NULL,
		text TEXT NOT NULL,
		state TEXT NOT NULL,
		written_at TIMESTAMP NOT NULL,
		decided_at TIMESTAMP,
		sent_text TEXT
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_draft_pending ON drafts(matter_id) WHERE state IN ('written', 'sending');
`

const maxDraftChars = 1000

type CreateDraftRequest struct {
	MatterID int64  `json:"matter_id"`
	Text     string `json:"text"`
}

type ApproveDraftRequest struct {
	DraftID int64  `json:"draft_id"`
	Text    string `json:"text"`
}

type DiscardDraftRequest struct {
	DraftID int64 `json:"draft_id"`
}

type ListDraftsRequest struct {
	State string `json:"state"`
}

type APIDraft struct {
	ID        int64   `json:"id"`
	MatterID  int64   `json:"matter_id"`
	ChatJID   string  `json:"chat_jid"`
	Name      string  `json:"name"`
	Snippet   string  `json:"snippet"`
	Text      string  `json:"text"`
	State     string  `json:"state"`
	WrittenAt string  `json:"written_at"`
	DecidedAt *string `json:"decided_at"`
	SentText  *string `json:"sent_text"`
}

const draftCols = `d.id, d.matter_id, d.chat_jid, d.text, d.state, d.written_at, d.decided_at, d.sent_text, m.snippet`

func scanDraft(db *sql.DB, row interface{ Scan(...interface{}) error }) (APIDraft, error) {
	var d APIDraft
	var written time.Time
	var decided sql.NullTime
	var sent, snippet sql.NullString
	if err := row.Scan(&d.ID, &d.MatterID, &d.ChatJID, &d.Text, &d.State, &written, &decided, &sent, &snippet); err != nil {
		return d, err
	}
	d.WrittenAt, d.SentText, d.Snippet, d.Name = written.Format(time.RFC3339), nullStringToPtr(sent), snippet.String, chatNameFor(db, d.ChatJID)
	if decided.Valid {
		s := decided.Time.Format(time.RFC3339)
		d.DecidedAt = &s
	}
	return d, nil
}

func loadDraft(db *sql.DB, id int64) (APIDraft, error) {
	d, err := scanDraft(db, db.QueryRow(`SELECT `+draftCols+` FROM drafts d JOIN matters m ON m.id = d.matter_id WHERE d.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, invalid("draft %d not found", id)
	}
	return d, err
}

func cleanDraftText(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > maxDraftChars {
		return "", invalid("text must be between 1 and %d characters", maxDraftChars)
	}
	return s, nil
}

// createDraft writes (or rewrites) the pending draft of a matter the other
// person owes. Writing sends nothing.
func createDraft(db *sql.DB, req CreateDraftRequest, now time.Time) (APIDraft, error) {
	text, err := cleanDraftText(req.Text)
	if err != nil {
		return APIDraft{}, err
	}
	m, err := loadMatter(db, req.MatterID)
	if err != nil {
		return APIDraft{}, err
	}
	if m.OwedBy != "other" || matterState(m, now) != "open" {
		return APIDraft{}, invalid("matter %d is not an open matter the other person owes: there is nothing to chase", m.ID)
	}
	res, err := db.Exec(`UPDATE drafts SET text = ?, written_at = ? WHERE matter_id = ? AND state = 'written'`, text, now, m.ID)
	if err != nil {
		return APIDraft{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := db.Exec(`INSERT INTO drafts (matter_id, chat_jid, text, state, written_at) VALUES (?, ?, ?, 'written', ?)`, m.ID, m.ChatJID, text, now); err != nil {
			return APIDraft{}, err
		}
	}
	d, err := scanDraft(db, db.QueryRow(`SELECT `+draftCols+` FROM drafts d JOIN matters m ON m.id = d.matter_id WHERE d.matter_id = ? AND d.state = 'written'`, m.ID))
	return d, err
}

func listDrafts(db *sql.DB, req ListDraftsRequest) ([]APIDraft, error) {
	q, args := `SELECT `+draftCols+` FROM drafts d JOIN matters m ON m.id = d.matter_id`, []interface{}{}
	if req.State != "" {
		q, args = q+` WHERE d.state = ?`, append(args, req.State)
	}
	rows, err := db.Query(q+` ORDER BY d.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIDraft{}
	for rows.Next() {
		d, err := scanDraft(db, rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// approveDraft is the only path that sends a chase. It sends exactly req.Text
// (the text the owner approved, edited or not) to the draft's chat, once: the
// draft is claimed before sending, so a double approval cannot send twice.
func approveDraft(db *sql.DB, req ApproveDraftRequest, send func(chatJID, text string) (bool, string), now time.Time) (APIDraft, error) {
	text, err := cleanDraftText(req.Text)
	if err != nil {
		return APIDraft{}, err
	}
	d, err := loadDraft(db, req.DraftID)
	if err != nil {
		return APIDraft{}, err
	}
	m, err := loadMatter(db, d.MatterID)
	if err != nil {
		return APIDraft{}, err
	}
	if matterState(m, now) != "open" {
		return APIDraft{}, invalid("matter %d is no longer open: nothing was sent", m.ID)
	}
	res, err := db.Exec(`UPDATE drafts SET state = 'sending' WHERE id = ? AND state = 'written'`, d.ID)
	if err != nil {
		return APIDraft{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return APIDraft{}, invalid("draft %d is %s, not pending: nothing was sent", d.ID, d.State)
	}
	if ok, msg := send(d.ChatJID, text); !ok {
		_, _ = db.Exec(`UPDATE drafts SET state = 'written' WHERE id = ?`, d.ID)
		return APIDraft{}, fmt.Errorf("send failed, the draft is still pending: %s", msg)
	}
	if _, err := db.Exec(`UPDATE drafts SET state = 'sent', decided_at = ?, sent_text = ? WHERE id = ?`, now, text, d.ID); err != nil {
		return APIDraft{}, err
	}
	return loadDraft(db, d.ID)
}

func discardDraft(db *sql.DB, req DiscardDraftRequest, now time.Time) (APIDraft, error) {
	res, err := db.Exec(`UPDATE drafts SET state = 'discarded', decided_at = ? WHERE id = ? AND state = 'written'`, now, req.DraftID)
	if err != nil {
		return APIDraft{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := loadDraft(db, req.DraftID); err != nil {
			return APIDraft{}, err
		}
		return APIDraft{}, invalid("draft %d is not pending", req.DraftID)
	}
	return loadDraft(db, req.DraftID)
}

func registerDraftRoutes(store *MessageStore, client *whatsmeow.Client) {
	db := store.db
	http.HandleFunc("/api/drafts/create", matterRoute(func(r CreateDraftRequest) (APIDraft, error) {
		return createDraft(db, r, time.Now())
	}))
	http.HandleFunc("/api/drafts/list", matterRoute(func(r ListDraftsRequest) (map[string][]APIDraft, error) {
		d, err := listDrafts(db, r)
		return map[string][]APIDraft{"drafts": d}, err
	}))
	http.HandleFunc("/api/drafts/approve", handleApproveDraft(store, client))
	http.HandleFunc("/api/drafts/discard", matterRoute(func(r DiscardDraftRequest) (APIDraft, error) {
		return discardDraft(db, r, time.Now())
	}))
}

func handleApproveDraft(store *MessageStore, client *whatsmeow.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		var req ApproveDraftRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		var sendErr error
		resp, err := approveDraft(store.db, req, func(chat, text string) (bool, string) {
			var message string
			message, sendErr = sendWhatsAppMessage(r.Context(), client, store, chat, text, "", "", nil)
			if sendErr != nil {
				return false, sendErr.Error()
			}
			return true, message
		}, time.Now())
		if sendErr != nil {
			err = fmt.Errorf("send failed, the draft is still pending: %w", sendErr)
		}
		if err != nil {
			if writeSendRateError(w, err) {
				return
			}
			status := http.StatusInternalServerError
			var bad *errInvalidRequest
			if errors.As(err, &bad) {
				status = http.StatusBadRequest
			}
			writeJSONError(w, status, err.Error())
			return
		}
		writeJSON(w, resp)
	}
}
