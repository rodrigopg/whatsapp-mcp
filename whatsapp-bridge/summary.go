package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
)

// One published summary per day, kept as a photograph: it never changes after
// it is built, so the owner can see later what was in front of them that morning.
const summarySchema = `
	CREATE TABLE IF NOT EXISTS summaries (
		day TEXT PRIMARY KEY,
		built_at TIMESTAMP NOT NULL,
		body TEXT NOT NULL
	);
`

const summaryMaxItems = 10

type SummaryRequest struct {
	AsOf string `json:"as_of"`
	All  bool   `json:"all"`
}

type SummaryItem struct {
	MatterID  int64   `json:"matter_id"`
	ChatJID   string  `json:"chat_jid"`
	Name      string  `json:"name"`
	Time      string  `json:"time"`
	Snippet   string  `json:"snippet"`
	Link      *string `json:"link"`
	Reason    string  `json:"reason"`
	Certainty string  `json:"certainty"`
	OwedBy    string  `json:"owed_by"`
	Origin    string  `json:"origin"`
	Deadline  *string `json:"deadline"`
	Group     string  `json:"group"`
}

type SummaryAnnouncement struct {
	MatterID int64  `json:"matter_id"`
	ChatJID  string `json:"chat_jid"`
	Name     string `json:"name"`
	Snippet  string `json:"snippet"`
	ClosedAt string `json:"closed_at"`
}

type APISummary struct {
	Day           string                `json:"day"`
	BuiltAt       string                `json:"built_at"`
	Items         []SummaryItem         `json:"items"`
	Announcements []SummaryAnnouncement `json:"announcements"`
	Total         int                   `json:"total"`
	More          int                   `json:"more"`
}

func chatNameFor(db *sql.DB, jid string) string {
	var name, full, push sql.NullString
	_ = db.QueryRow(`SELECT c.name, s.full_name, s.push_name FROM chats c LEFT JOIN senders s ON s.jid = c.jid WHERE c.jid = ?`, jid).Scan(&name, &full, &push)
	return displayName(name.String, full.String, push.String, jidUser(jid))
}

// morningSummary returns the owner's summary for the day of req.AsOf: what needs
// their action, favorites first, then who chased them or what they promised,
// then the rest, newest first inside a group. The first call of a day builds
// and stores it (and marks the closures it announces as told); later calls
// return the same photograph. Only the first summaryMaxItems come back unless
// All is set; More counts the rest, which are never dropped.
func morningSummary(db *sql.DB, req SummaryRequest, toPN func(string) string, now time.Time) (APISummary, error) {
	asOf, err := time.Parse(time.RFC3339, req.AsOf)
	if err != nil {
		return APISummary{}, invalid("as_of is required: an ISO-8601 instant with offset, e.g. 2026-10-12T07:00:00-03:00")
	}
	day := asOf.Format(matterDateLayout)
	var s APISummary
	var body string
	switch err := db.QueryRow(`SELECT body FROM summaries WHERE day = ?`, day).Scan(&body); {
	case err == nil:
		if err := json.Unmarshal([]byte(body), &s); err != nil {
			return APISummary{}, err
		}
	case errors.Is(err, sql.ErrNoRows):
		if s, err = buildSummary(db, asOf, day, toPN, now); err != nil {
			return APISummary{}, err
		}
	default:
		return APISummary{}, err
	}
	if !req.All && len(s.Items) > summaryMaxItems {
		s.More = len(s.Items) - summaryMaxItems
		s.Items = s.Items[:summaryMaxItems]
	}
	return s, nil
}

func buildSummary(db *sql.DB, asOf time.Time, day string, toPN func(string) string, now time.Time) (APISummary, error) {
	all, err := loadMatters(db, "")
	if err != nil {
		return APISummary{}, err
	}
	favs := map[string]bool{}
	rows, err := db.Query(`SELECT chat_jid FROM favorites WHERE status = 'favorite'`)
	if err != nil {
		return APISummary{}, err
	}
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			rows.Close()
			return APISummary{}, err
		}
		favs[jid] = true
	}
	rows.Close()

	type ranked struct {
		item SummaryItem
		at   time.Time
		rank int
	}
	var items []ranked
	s := APISummary{Day: day, BuiltAt: now.Format(time.RFC3339), Items: []SummaryItem{}, Announcements: []SummaryAnnouncement{}}
	var announced []int64
	for _, m := range all {
		if m.AnnouncePending && m.ClosedHow != nil && *m.ClosedHow == "confirmed" {
			closed := ""
			if m.ClosedAt != nil {
				closed = m.ClosedAt.Format(time.RFC3339)
			}
			s.Announcements = append(s.Announcements, SummaryAnnouncement{MatterID: m.ID, ChatJID: m.ChatJID, Name: chatNameFor(db, m.ChatJID), Snippet: m.Snippet, ClosedAt: closed})
			announced = append(announced, m.ID)
			continue
		}
		ok, reason := needsAttention(m, asOf)
		if !ok {
			continue
		}
		group, rank := "other", 2
		switch {
		case favs[m.ChatJID]:
			group, rank = "favorite", 0
		case m.Origin == "demand" || strings.HasPrefix(reason, "own_promise"):
			group, rank = "charged_or_promised", 1
		}
		pn := m.ChatJID
		if toPN != nil && strings.HasSuffix(pn, "@lid") {
			pn = toPN(pn)
		}
		items = append(items, ranked{SummaryItem{MatterID: m.ID, ChatJID: m.ChatJID, Name: chatNameFor(db, m.ChatJID), Time: m.CreatedAt.Format(time.RFC3339),
			Snippet: m.Snippet, Link: waLink(pn), Reason: reason, Certainty: m.Certainty, OwedBy: m.OwedBy, Origin: m.Origin, Deadline: m.Deadline, Group: group}, m.CreatedAt, rank})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].rank != items[j].rank {
			return items[i].rank < items[j].rank
		}
		return items[i].at.After(items[j].at)
	})
	for _, it := range items {
		s.Items = append(s.Items, it.item)
	}
	s.Total = len(s.Items)

	body, err := json.Marshal(s)
	if err != nil {
		return APISummary{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return APISummary{}, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO summaries (day, built_at, body) VALUES (?, ?, ?)`, day, now, string(body)); err != nil {
		return APISummary{}, err
	}
	for _, id := range announced {
		if _, err := tx.Exec(`UPDATE matters SET announce_pending = 0 WHERE id = ?`, id); err != nil {
			return APISummary{}, err
		}
		if _, err := tx.Exec(`INSERT INTO matter_events (matter_id, kind, at) VALUES (?, 'announced', ?)`, id, now); err != nil {
			return APISummary{}, err
		}
	}
	return s, tx.Commit()
}

func registerSummaryRoute(db *sql.DB, client *whatsmeow.Client) {
	http.HandleFunc("/api/summary", matterRoute(func(r SummaryRequest) (APISummary, error) {
		return morningSummary(db, r, lidToPN(client), time.Now())
	}))
}
