package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// Additive tables for open matters (pending requests and commitments found in
// conversations). A matter is one subject, not one message: events keep the
// history and the messages that motivated each step. The review cursor says
// up to where conversations have been reviewed.
const matterSchema = `
	CREATE TABLE IF NOT EXISTS matters (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		chat_jid TEXT NOT NULL,
		owed_by TEXT NOT NULL,
		origin TEXT NOT NULL,
		origin_message_id TEXT NOT NULL,
		snippet TEXT,
		created_at TIMESTAMP NOT NULL,
		deadline TEXT,
		certainty TEXT NOT NULL,
		valid_until TIMESTAMP NOT NULL,
		closed_how TEXT,
		closed_at TIMESTAMP,
		announce_pending INTEGER NOT NULL DEFAULT 0
	);
	CREATE INDEX IF NOT EXISTS idx_matters_chat ON matters(chat_jid);
	CREATE TABLE IF NOT EXISTS matter_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		matter_id INTEGER NOT NULL,
		kind TEXT NOT NULL,
		at TIMESTAMP NOT NULL,
		message_id TEXT,
		note TEXT
	);
	CREATE UNIQUE INDEX IF NOT EXISTS idx_matter_mention ON matter_events(matter_id, message_id) WHERE kind IN ('opened', 'mention', 'reopened');
	CREATE TABLE IF NOT EXISTS review_cursor (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		reviewed_up_to TIMESTAMP
	);
`

const matterDateLayout = "2006-01-02"

// Matter.OwedBy: "owner" | "other". Origin: "request" | "demand" | "promise" | "event".
// Certainty: "clear" | "uncertain". ClosedHow: nil | "confirmed" | "owner".
// Deadline is a plain date (YYYY-MM-DD), never a time.
type Matter struct {
	ID              int64
	ChatJID         string
	OwedBy          string
	Origin          string
	OriginMessageID string
	Snippet         string
	CreatedAt       time.Time
	Deadline        *string
	Certainty       string
	ValidUntil      time.Time
	ClosedHow       *string
	ClosedAt        *time.Time
	AnnouncePending bool
}

// matterValidity is when a matter opened (or reopened) at from expires.
func matterValidity(from time.Time) time.Time { return from.AddDate(0, 1, 0) }

// matterState is derived, never stored: a closed matter stays closed, an open
// one expires when its validity ends. Expiry needs no background job.
func matterState(m Matter, now time.Time) string {
	switch {
	case m.ClosedHow != nil && *m.ClosedHow == "owner":
		return "closed_owner"
	case m.ClosedHow != nil:
		return "closed_confirmed"
	case !now.Before(m.ValidUntil):
		return "expired"
	}
	return "open"
}

// needsAttention says whether an open matter asks for the owner's attention on
// the day of asOf, and why. The day (and the zone it is read in) comes from
// asOf: the bridge does not know the owner's timezone.
func needsAttention(m Matter, asOf time.Time) (bool, string) {
	if matterState(m, asOf) != "open" {
		return false, ""
	}
	today, created := asOf.Format(matterDateLayout), m.CreatedAt.In(asOf.Location()).Format(matterDateLayout)
	switch {
	case m.Deadline == nil || m.Origin == "request" || m.Origin == "demand":
		if today > created {
			return true, "next_day"
		}
	case m.OwedBy == "owner":
		if today == created {
			return true, "own_promise_created_today"
		}
		if today >= *m.Deadline {
			return true, "own_promise_deadline"
		}
	default:
		if today >= *m.Deadline {
			return true, "other_deadline"
		}
	}
	return false, ""
}

// ---- store ----------------------------------------------------------------------------------

type RecordMatterRequest struct {
	ChatJID   string  `json:"chat_jid"`
	MessageID string  `json:"message_id"`
	OwedBy    string  `json:"owed_by"`
	Origin    string  `json:"origin"`
	Deadline  *string `json:"deadline"`
	Certainty string  `json:"certainty"`
	MatterID  *int64  `json:"matter_id"`
}

type CloseMatterRequest struct {
	MatterID  int64  `json:"matter_id"`
	By        string `json:"by"`
	MessageID string `json:"message_id"`
}

type ReopenMatterRequest struct {
	MatterID  int64  `json:"matter_id"`
	MessageID string `json:"message_id"`
}

type ListMattersRequest struct {
	AsOf          string `json:"as_of"`
	State         string `json:"state"`
	ChatJID       string `json:"chat_jid"`
	AttentionOnly bool   `json:"attention_only"`
}

type ReviewNextRequest struct {
	Limit int `json:"limit"`
}

type ReviewMarkRequest struct {
	UpTo string `json:"up_to"`
}

type APIMatterEvent struct {
	Kind      string  `json:"kind"`
	At        string  `json:"at"`
	MessageID *string `json:"message_id"`
}

type APIMatter struct {
	ID              int64            `json:"id"`
	ChatJID         string           `json:"chat_jid"`
	OwedBy          string           `json:"owed_by"`
	Origin          string           `json:"origin"`
	OriginMessageID string           `json:"origin_message_id"`
	Snippet         string           `json:"snippet"`
	CreatedAt       string           `json:"created_at"`
	Deadline        *string          `json:"deadline"`
	Certainty       string           `json:"certainty"`
	State           string           `json:"state"`
	Reason          string           `json:"reason,omitempty"`
	ValidUntil      string           `json:"valid_until"`
	ClosedAt        *string          `json:"closed_at"`
	AnnouncePending bool             `json:"announce_pending"`
	Mentions        int              `json:"mentions"`
	Events          []APIMatterEvent `json:"events"`
}

type ReviewMessage struct {
	ID        string `json:"id"`
	ChatJID   string `json:"chat_jid"`
	ChatName  string `json:"chat_name"`
	Sender    string `json:"sender"`
	Content   string `json:"content"`
	IsFromMe  bool   `json:"is_from_me"`
	Timestamp string `json:"timestamp"`
	MediaType string `json:"media_type"`
}

type ReviewNextResponse struct {
	Messages    []ReviewMessage `json:"messages"`
	OpenMatters []APIMatter     `json:"open_matters"`
	NextCursor  string          `json:"next_cursor"`
}

func invalid(format string, args ...interface{}) error {
	return &errInvalidRequest{msg: fmt.Sprintf(format, args...)}
}

func oneOf(v string, set ...string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// checkTrackedChat: a matter lives in a one-to-one chat, or in a group when the
// cited message @-mentions the owner (anything else in a group is not for them).
func checkTrackedChat(db *sql.DB, jid, messageID string, selfUsers []string) error {
	if strings.HasSuffix(jid, "@g.us") {
		ok, err := mentionsOwner(db, jid, messageID, selfUsers)
		if err != nil {
			return err
		}
		if !ok {
			return invalid("message %s does not mention the owner: in groups only @mentions are tracked", messageID)
		}
		return nil
	}
	return checkIndividualChat(jid, selfUsers)
}

// checkIndividualChat: only one-to-one chats count, never the owner's own chat
// (a group needs the owner's @mention, see checkTrackedChat).
func checkIndividualChat(jid string, selfUsers []string) error {
	if !strings.HasSuffix(jid, "@s.whatsapp.net") && !strings.HasSuffix(jid, "@lid") {
		return invalid("chat %s is not an individual chat: groups, status and newsletters are not supported", jid)
	}
	for _, u := range selfUsers {
		if jidUser(jid) == u {
			return invalid("the owner's own chat is not supported")
		}
	}
	return nil
}

func loadMessage(db *sql.DB, chat, id string) (content string, ts time.Time, fromMe bool, err error) {
	var c, mt sql.NullString
	err = db.QueryRow(`SELECT content, timestamp, is_from_me, media_type FROM messages WHERE chat_jid = ? AND id = ?`, chat, id).Scan(&c, &ts, &fromMe, &mt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ts, false, invalid("message %s not found in chat %s", id, chat)
	}
	content = strings.Join(strings.Fields(c.String), " ")
	if content == "" && mt.String != "" {
		content = "[" + mt.String + "]"
	}
	return content, ts, fromMe, err
}

const matterCols = `id, chat_jid, owed_by, origin, origin_message_id, snippet, created_at, deadline, certainty, valid_until, closed_how, closed_at, announce_pending`

func scanMatter(row interface{ Scan(...interface{}) error }) (Matter, error) {
	var m Matter
	var snippet, deadline, closedHow sql.NullString
	var closedAt sql.NullTime
	if err := row.Scan(&m.ID, &m.ChatJID, &m.OwedBy, &m.Origin, &m.OriginMessageID, &snippet, &m.CreatedAt, &deadline, &m.Certainty,
		&m.ValidUntil, &closedHow, &closedAt, &m.AnnouncePending); err != nil {
		return m, err
	}
	m.Snippet, m.Deadline, m.ClosedHow = snippet.String, nullStringToPtr(deadline), nullStringToPtr(closedHow)
	if closedAt.Valid {
		m.ClosedAt = &closedAt.Time
	}
	return m, nil
}

func loadMatter(db *sql.DB, id int64) (Matter, error) {
	m, err := scanMatter(db.QueryRow(`SELECT `+matterCols+` FROM matters WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return m, invalid("matter %d not found", id)
	}
	return m, err
}

func apiMatter(db *sql.DB, m Matter, asOf time.Time) (APIMatter, error) {
	out := APIMatter{ID: m.ID, ChatJID: m.ChatJID, OwedBy: m.OwedBy, Origin: m.Origin, OriginMessageID: m.OriginMessageID,
		Snippet: m.Snippet, CreatedAt: m.CreatedAt.Format(time.RFC3339), Deadline: m.Deadline, Certainty: m.Certainty,
		State: matterState(m, asOf), ValidUntil: m.ValidUntil.Format(time.RFC3339), AnnouncePending: m.AnnouncePending, Events: []APIMatterEvent{}}
	if _, reason := needsAttention(m, asOf); reason != "" {
		out.Reason = reason
	}
	if m.ClosedAt != nil {
		s := m.ClosedAt.Format(time.RFC3339)
		out.ClosedAt = &s
	}
	rows, err := db.Query(`SELECT kind, at, message_id FROM matter_events WHERE matter_id = ? ORDER BY id`, m.ID)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var e APIMatterEvent
		var at time.Time
		var mid sql.NullString
		if err := rows.Scan(&e.Kind, &at, &mid); err != nil {
			return out, err
		}
		e.At, e.MessageID = at.Format(time.RFC3339), nullStringToPtr(mid)
		if e.Kind == "opened" || e.Kind == "mention" {
			out.Mentions++
		}
		out.Events = append(out.Events, e)
	}
	return out, rows.Err()
}

func addMatterEvent(db *sql.DB, id int64, kind string, at time.Time, messageID string) error {
	var mid interface{}
	if messageID != "" {
		mid = messageID
	}
	_, err := db.Exec(`INSERT INTO matter_events (matter_id, kind, at, message_id) VALUES (?, ?, ?, ?)`, id, kind, at, mid)
	return err
}

// recordMatter registers a matter the AI client recognized, citing the message
// that proves it. Recording the same message again returns the same matter.
func recordMatter(db *sql.DB, req RecordMatterRequest, selfUsers []string, now time.Time) (APIMatter, error) {
	if !oneOf(req.OwedBy, "owner", "other") || !oneOf(req.Origin, "request", "demand", "promise", "event") {
		return APIMatter{}, invalid("owed_by must be owner|other and origin request|demand|promise|event")
	}
	if req.Certainty == "" {
		req.Certainty = "clear"
	}
	if !oneOf(req.Certainty, "clear", "uncertain") {
		return APIMatter{}, invalid("certainty must be clear|uncertain")
	}
	if req.Deadline != nil {
		if _, err := time.Parse(matterDateLayout, *req.Deadline); err != nil {
			return APIMatter{}, invalid("deadline must be a date YYYY-MM-DD")
		}
	}
	if err := checkTrackedChat(db, req.ChatJID, req.MessageID, selfUsers); err != nil {
		return APIMatter{}, err
	}
	content, ts, _, err := loadMessage(db, req.ChatJID, req.MessageID)
	if err != nil {
		return APIMatter{}, err
	}
	if ts.Before(now.AddDate(0, -1, 0)) {
		return APIMatter{}, invalid("message %s is older than one month", req.MessageID)
	}

	var existing int64
	err = db.QueryRow(`SELECT e.matter_id FROM matter_events e JOIN matters m ON m.id = e.matter_id
		WHERE e.message_id = ? AND m.chat_jid = ? AND e.kind IN ('opened', 'mention', 'reopened') LIMIT 1`, req.MessageID, req.ChatJID).Scan(&existing)
	switch {
	case err == nil:
		if req.MatterID != nil && *req.MatterID != existing {
			return APIMatter{}, invalid("message %s is already recorded on matter %d", req.MessageID, existing)
		}
		m, err := loadMatter(db, existing)
		if err != nil {
			return APIMatter{}, err
		}
		return apiMatter(db, m, now)
	case !errors.Is(err, sql.ErrNoRows):
		return APIMatter{}, err
	}

	if req.MatterID != nil {
		m, err := loadMatter(db, *req.MatterID)
		if err != nil {
			return APIMatter{}, err
		}
		if m.ChatJID != req.ChatJID || matterState(m, now) != "open" {
			return APIMatter{}, invalid("matter %d is not an open matter of this chat", m.ID)
		}
		if err := addMatterEvent(db, m.ID, "mention", now, req.MessageID); err != nil {
			return APIMatter{}, err
		}
		return apiMatter(db, m, now)
	}

	snippet := []rune(content)
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	tx, err := db.Begin()
	if err != nil {
		return APIMatter{}, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO matters (chat_jid, owed_by, origin, origin_message_id, snippet, created_at, deadline, certainty, valid_until)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, req.ChatJID, req.OwedBy, req.Origin, req.MessageID, string(snippet), ts, req.Deadline, req.Certainty, matterValidity(ts))
	if err != nil {
		return APIMatter{}, err
	}
	id, _ := res.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO matter_events (matter_id, kind, at, message_id) VALUES (?, 'opened', ?, ?)`, id, now, req.MessageID); err != nil {
		return APIMatter{}, err
	}
	if err := tx.Commit(); err != nil {
		return APIMatter{}, err
	}
	m, err := loadMatter(db, id)
	if err != nil {
		return APIMatter{}, err
	}
	return apiMatter(db, m, now)
}

// closeMatter ends an open matter: "confirmed" needs a later message from the
// other person (the owner's own replies never close anything), "owner" is the
// owner's word and needs none.
func closeMatter(db *sql.DB, req CloseMatterRequest, now time.Time) (APIMatter, error) {
	m, err := loadMatter(db, req.MatterID)
	if err != nil {
		return APIMatter{}, err
	}
	if matterState(m, now) != "open" {
		return APIMatter{}, invalid("matter %d is not open", m.ID)
	}
	switch req.By {
	case "confirmed":
		_, ts, fromMe, err := loadMessage(db, m.ChatJID, req.MessageID)
		if err != nil {
			return APIMatter{}, err
		}
		if fromMe {
			return APIMatter{}, invalid("a message from the owner never closes a matter")
		}
		if !ts.After(m.CreatedAt) {
			return APIMatter{}, invalid("message %s is not newer than the matter", req.MessageID)
		}
	case "owner":
		req.MessageID = ""
	default:
		return APIMatter{}, invalid("by must be confirmed|owner")
	}
	if _, err := db.Exec(`UPDATE matters SET closed_how = ?, closed_at = ?, announce_pending = ? WHERE id = ?`, req.By, now, req.By == "confirmed", m.ID); err != nil {
		return APIMatter{}, err
	}
	if err := addMatterEvent(db, m.ID, "closed_"+req.By, now, req.MessageID); err != nil {
		return APIMatter{}, err
	}
	if m, err = loadMatter(db, m.ID); err != nil {
		return APIMatter{}, err
	}
	return apiMatter(db, m, now)
}

// reopenMatter brings a closed or expired matter back when the conversation
// returns to it: same matter, history kept, one more month from now.
func reopenMatter(db *sql.DB, req ReopenMatterRequest, now time.Time) (APIMatter, error) {
	m, err := loadMatter(db, req.MatterID)
	if err != nil {
		return APIMatter{}, err
	}
	if matterState(m, now) == "open" {
		return APIMatter{}, invalid("matter %d is already open", m.ID)
	}
	_, ts, _, err := loadMessage(db, m.ChatJID, req.MessageID)
	if err != nil {
		return APIMatter{}, err
	}
	var last time.Time
	err = db.QueryRow(`SELECT messages.timestamp FROM matter_events e JOIN messages ON messages.id = e.message_id AND messages.chat_jid = ?
		WHERE e.matter_id = ? ORDER BY julianday(messages.timestamp) DESC, messages.rowid DESC LIMIT 1`, m.ChatJID, m.ID).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return APIMatter{}, err
	}
	if !ts.After(last) {
		return APIMatter{}, invalid("message %s is not newer than what the matter already knows", req.MessageID)
	}
	if err := addMatterEvent(db, m.ID, "reopened", now, req.MessageID); err != nil {
		return APIMatter{}, err
	}
	if _, err := db.Exec(`UPDATE matters SET closed_how = NULL, closed_at = NULL, announce_pending = 0, valid_until = ? WHERE id = ?`, matterValidity(now), m.ID); err != nil {
		return APIMatter{}, err
	}
	if m, err = loadMatter(db, m.ID); err != nil {
		return APIMatter{}, err
	}
	return apiMatter(db, m, now)
}

// loadMatters returns every matter (of one chat when chat is set), oldest first.
func loadMatters(db *sql.DB, chat string) ([]Matter, error) {
	q, args := `SELECT `+matterCols+` FROM matters`, []interface{}{}
	if chat != "" {
		q, args = q+` WHERE chat_jid = ?`, append(args, chat)
	}
	rows, err := db.Query(q+` ORDER BY julianday(created_at), id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []Matter
	for rows.Next() {
		m, err := scanMatter(rows)
		if err != nil {
			return nil, err
		}
		all = append(all, m)
	}
	return all, rows.Err()
}

// listMatters returns matters with their derived state as of req.AsOf (an
// instant with offset: it also fixes the zone the "day" is read in).
func listMatters(db *sql.DB, req ListMattersRequest) ([]APIMatter, error) {
	asOf, err := time.Parse(time.RFC3339, req.AsOf)
	if err != nil {
		return nil, invalid("as_of is required: an ISO-8601 instant with offset, e.g. 2026-10-12T07:00:00-03:00")
	}
	all, err := loadMatters(db, req.ChatJID)
	if err != nil {
		return nil, err
	}
	out := []APIMatter{}
	for _, m := range all {
		state := matterState(m, asOf)
		if req.AttentionOnly {
			if ok, _ := needsAttention(m, asOf); !ok {
				continue
			}
		}
		if req.State != "" && state != req.State && !(req.State == "closed" && strings.HasPrefix(state, "closed_")) {
			continue
		}
		a, err := apiMatter(db, m, asOf)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, nil
}

func reviewCursor(db *sql.DB) (*time.Time, error) {
	var c sql.NullTime
	err := db.QueryRow(`SELECT reviewed_up_to FROM review_cursor WHERE id = 1`).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) || !c.Valid {
		return nil, nil
	}
	return &c.Time, err
}

// reviewNext returns the messages nobody has reviewed yet (after the cursor,
// at most one month old, individual chats, not the owner's own), oldest first,
// with the open matters of those chats. Group messages come only when they
// @-mention the owner. It never moves the cursor.
func reviewNext(db *sql.DB, req ReviewNextRequest, selfUsers []string, now time.Time) (ReviewNextResponse, error) {
	limit := req.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	since := now.AddDate(0, -1, 0)
	cursor, err := reviewCursor(db)
	if err != nil {
		return ReviewNextResponse{}, err
	}
	if cursor != nil && cursor.After(since) {
		since = *cursor
	}
	kinds, args := `m.chat_jid LIKE '%@s.whatsapp.net' OR m.chat_jid LIKE '%@lid'`, []interface{}{}
	if len(selfUsers) > 0 { // group messages only when they @-mention the owner
		kinds += ` OR (m.chat_jid LIKE '%@g.us' AND EXISTS (SELECT 1 FROM message_mentions mm WHERE mm.chat_jid = m.chat_jid AND mm.message_id = m.id AND mm.mentioned_user IN (?` + strings.Repeat(",?", len(selfUsers)-1) + `)))`
		for _, u := range selfUsers {
			args = append(args, u)
		}
	}
	where := `(` + kinds + `) AND julianday(m.timestamp) > julianday(?)`
	args = append(args, since.UTC().Format("2006-01-02 15:04:05.000000000"))
	for _, u := range selfUsers {
		where += ` AND substr(m.chat_jid, 1, instr(m.chat_jid, '@') - 1) != ?`
		args = append(args, u)
	}
	rows, err := db.Query(`SELECT m.id, m.chat_jid, c.name, m.sender, substr(m.content, 1, 2000), m.is_from_me, m.timestamp, m.media_type
		FROM messages m JOIN chats c ON c.jid = m.chat_jid WHERE `+where+` ORDER BY julianday(m.timestamp), m.rowid LIMIT ?`, append(args, limit)...)
	if err != nil {
		return ReviewNextResponse{}, err
	}
	defer rows.Close()
	resp := ReviewNextResponse{Messages: []ReviewMessage{}, OpenMatters: []APIMatter{}}
	if cursor != nil {
		resp.NextCursor = cursor.Format(time.RFC3339)
	}
	seen := map[string]bool{}
	for rows.Next() {
		var m ReviewMessage
		var name, content, mt sql.NullString
		var ts time.Time
		if err := rows.Scan(&m.ID, &m.ChatJID, &name, &m.Sender, &content, &m.IsFromMe, &ts, &mt); err != nil {
			return ReviewNextResponse{}, err
		}
		m.ChatName, m.Content, m.MediaType, m.Timestamp = name.String, content.String, mt.String, ts.Format(time.RFC3339)
		resp.Messages = append(resp.Messages, m)
		resp.NextCursor = m.Timestamp
		seen[m.ChatJID] = true
	}
	if err := rows.Err(); err != nil {
		return ReviewNextResponse{}, err
	}
	rows.Close()
	for jid := range seen {
		ms, err := listMatters(db, ListMattersRequest{AsOf: now.Format(time.RFC3339), State: "open", ChatJID: jid})
		if err != nil {
			return ReviewNextResponse{}, err
		}
		resp.OpenMatters = append(resp.OpenMatters, ms...)
	}
	return resp, nil
}

// reviewMark advances the cursor to upTo; it never goes back. Returns the cursor.
func reviewMark(db *sql.DB, req ReviewMarkRequest) (string, error) {
	up, err := time.Parse(time.RFC3339, req.UpTo)
	if err != nil {
		return "", invalid("up_to must be an ISO-8601 instant with offset")
	}
	cur, err := reviewCursor(db)
	if err != nil {
		return "", err
	}
	if cur != nil && !up.After(*cur) {
		return cur.Format(time.RFC3339), nil
	}
	_, err = db.Exec(`INSERT INTO review_cursor (id, reviewed_up_to) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET reviewed_up_to = excluded.reviewed_up_to`, up)
	return up.Format(time.RFC3339), err
}

// ---- REST -------------------------------------------------------------------------------------

// ownUsers are the owner's own phone and LID, whose chat is never tracked.
func ownUsers(client *whatsmeow.Client) []string {
	var self []string
	if client != nil && client.Store != nil {
		if client.Store.ID != nil {
			self = append(self, client.Store.ID.User)
		}
		if !client.Store.LID.IsEmpty() {
			self = append(self, client.Store.LID.User)
		}
	}
	return self
}

// lidToPN maps a LID jid to its phone jid through the whatsmeow store (as is when unknown).
func lidToPN(client *whatsmeow.Client) func(string) string {
	return func(jid string) string {
		parsed, err := types.ParseJID(jid)
		if err != nil {
			return jid
		}
		return resolveToPN(client, parsed).String()
	}
}

// waLink opens the chat with a phone jid; nil when there is no phone to open.
func waLink(pn string) *string {
	if !strings.HasSuffix(pn, "@s.whatsapp.net") {
		return nil
	}
	link := "https://wa.me/" + jidUser(pn)
	return &link
}

func matterRoute[Req, Resp any](fn func(Req) (Resp, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		var req Req
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		resp, err := fn(req)
		var bad *errInvalidRequest
		switch {
		case errors.As(err, &bad):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		case err != nil:
			writeJSONError(w, http.StatusInternalServerError, err.Error())
		default:
			writeJSON(w, resp)
		}
	}
}

func registerMatterRoutes(db *sql.DB, client *whatsmeow.Client) {
	http.HandleFunc("/api/matters/record", matterRoute(func(r RecordMatterRequest) (APIMatter, error) {
		return recordMatter(db, r, ownUsers(client), time.Now())
	}))
	http.HandleFunc("/api/matters/close", matterRoute(func(r CloseMatterRequest) (APIMatter, error) {
		return closeMatter(db, r, time.Now())
	}))
	http.HandleFunc("/api/matters/reopen", matterRoute(func(r ReopenMatterRequest) (APIMatter, error) {
		return reopenMatter(db, r, time.Now())
	}))
	http.HandleFunc("/api/matters/list", matterRoute(func(r ListMattersRequest) (map[string][]APIMatter, error) {
		ms, err := listMatters(db, r)
		return map[string][]APIMatter{"matters": ms}, err
	}))
	http.HandleFunc("/api/review/next", matterRoute(func(r ReviewNextRequest) (ReviewNextResponse, error) {
		return reviewNext(db, r, ownUsers(client), time.Now())
	}))
	http.HandleFunc("/api/review/mark", matterRoute(func(r ReviewMarkRequest) (map[string]string, error) {
		c, err := reviewMark(db, r)
		return map[string]string{"cursor": c}, err
	}))
}
