package main

import (
	"database/sql"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"
)

// The owner's circle of favorites: contacts that come first in the morning
// summary. Source "whatsapp" mirrors the app's own Favorites list; "owner" is a
// suggestion the owner accepted. status "rejected" remembers a refusal so the
// contact is not suggested again.
const favoritesSchema = `
	CREATE TABLE IF NOT EXISTS favorites (
		chat_jid TEXT PRIMARY KEY,
		status TEXT NOT NULL,
		source TEXT NOT NULL,
		decided_at TIMESTAMP NOT NULL
	);
`

// A suggestion needs a real conversation: at least this many messages each way
// in the last month, so someone who only writes to the owner is never proposed.
const favoriteMinEachWay = 3

type SuggestFavoritesRequest struct {
	Limit int `json:"limit"`
}

type DecideFavoriteRequest struct {
	ChatJID  string `json:"chat_jid"`
	Decision string `json:"decision"`
}

type FavoriteSuggestion struct {
	JID      string `json:"jid"`
	Name     string `json:"name"`
	Incoming int    `json:"incoming"`
	Outgoing int    `json:"outgoing"`
}

type APIFavorite struct {
	JID    string `json:"jid"`
	Name   string `json:"name"`
	Source string `json:"source"`
	Status string `json:"status,omitempty"`
}

// displayName: the chat name unless it is empty or just digits, then the saved
// or push name, then fallback (the phone).
func displayName(chatName, fullName, pushName, fallback string) string {
	if strings.TrimSpace(chatName) != "" && !isDigits(chatName) {
		return chatName
	}
	for _, n := range []string{fullName, pushName} {
		if strings.TrimSpace(n) != "" {
			return n
		}
	}
	return fallback
}

func suggestFavorites(db *sql.DB, req SuggestFavoritesRequest, selfUsers []string, now time.Time) ([]FavoriteSuggestion, error) {
	limit := req.Limit
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	where := `(m.chat_jid LIKE '%@s.whatsapp.net' OR m.chat_jid LIKE '%@lid') AND julianday(m.timestamp) > julianday(?)
		AND NOT EXISTS (SELECT 1 FROM favorites f WHERE f.chat_jid = m.chat_jid)`
	args := []interface{}{now.AddDate(0, -1, 0).UTC().Format("2006-01-02 15:04:05.000000000")}
	for _, u := range selfUsers {
		where += ` AND substr(m.chat_jid, 1, instr(m.chat_jid, '@') - 1) != ?`
		args = append(args, u)
	}
	rows, err := db.Query(`SELECT m.chat_jid, SUM(m.is_from_me = 0), SUM(m.is_from_me = 1), c.name, s.full_name, s.push_name
		FROM messages m JOIN chats c ON c.jid = m.chat_jid LEFT JOIN senders s ON s.jid = m.chat_jid
		WHERE `+where+` GROUP BY m.chat_jid HAVING SUM(m.is_from_me = 0) >= ? AND SUM(m.is_from_me = 1) >= ?
		ORDER BY COUNT(*) DESC, m.chat_jid LIMIT ?`, append(args, favoriteMinEachWay, favoriteMinEachWay, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FavoriteSuggestion{}
	for rows.Next() {
		var f FavoriteSuggestion
		var name, full, push sql.NullString
		if err := rows.Scan(&f.JID, &f.Incoming, &f.Outgoing, &name, &full, &push); err != nil {
			return nil, err
		}
		f.Name = displayName(name.String, full.String, push.String, jidUser(f.JID))
		out = append(out, f)
	}
	return out, rows.Err()
}

func listFavorites(db *sql.DB) ([]APIFavorite, error) {
	rows, err := db.Query(`SELECT f.chat_jid, f.source, c.name, s.full_name, s.push_name FROM favorites f
		LEFT JOIN chats c ON c.jid = f.chat_jid LEFT JOIN senders s ON s.jid = f.chat_jid
		WHERE f.status = 'favorite' ORDER BY julianday(f.decided_at), f.chat_jid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []APIFavorite{}
	for rows.Next() {
		var f APIFavorite
		var name, full, push sql.NullString
		if err := rows.Scan(&f.JID, &f.Source, &name, &full, &push); err != nil {
			return nil, err
		}
		f.Name = displayName(name.String, full.String, push.String, jidUser(f.JID))
		out = append(out, f)
	}
	return out, rows.Err()
}

// decideFavorite records the owner's decision: accept, reject (never suggest
// again) or remove (forget the decision).
func decideFavorite(db *sql.DB, req DecideFavoriteRequest, selfUsers []string, now time.Time) (APIFavorite, error) {
	status := map[string]string{"accept": "favorite", "reject": "rejected"}[req.Decision]
	if status == "" && req.Decision != "remove" {
		return APIFavorite{}, invalid("decision must be accept|reject|remove")
	}
	if err := checkIndividualChat(req.ChatJID, selfUsers); err != nil {
		return APIFavorite{}, err
	}
	var err error
	if status == "" {
		_, err = db.Exec(`DELETE FROM favorites WHERE chat_jid = ?`, req.ChatJID)
	} else {
		_, err = db.Exec(`INSERT INTO favorites (chat_jid, status, source, decided_at) VALUES (?, ?, 'owner', ?)
			ON CONFLICT(chat_jid) DO UPDATE SET status = excluded.status, source = 'owner', decided_at = excluded.decided_at`, req.ChatJID, status, now)
	}
	return APIFavorite{JID: req.ChatJID, Source: "owner", Status: status}, err
}

// mirrorWhatsAppFavorites replaces what came from the app's Favorites list with
// its current content. What the owner decided here stays, except that a contact
// the owner rejected here but favorited in the app becomes a favorite.
func mirrorWhatsAppFavorites(db *sql.DB, jids []string, now time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM favorites WHERE source = 'whatsapp'`); err != nil {
		return err
	}
	for _, jid := range jids {
		if _, err := tx.Exec(`INSERT INTO favorites (chat_jid, status, source, decided_at) VALUES (?, 'favorite', 'whatsapp', ?)
			ON CONFLICT(chat_jid) DO UPDATE SET status = 'favorite', source = 'whatsapp', decided_at = excluded.decided_at
			WHERE NOT (favorites.status = 'favorite' AND favorites.source = 'owner')`, jid, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// favoritesFromAppState reads the app's Favorites list out of an app-state
// event; ok is false for any other kind of event.
func favoritesFromAppState(evt *events.AppState) ([]string, bool) {
	if len(evt.Index) == 0 || evt.Index[0] != "favorites" {
		return nil, false
	}
	jids := []string{}
	for _, f := range evt.SyncActionValue.GetFavoritesAction().GetFavorites() {
		id := f.GetID()
		if id == "" {
			continue
		}
		if !strings.Contains(id, "@") {
			id += "@s.whatsapp.net"
		}
		jids = append(jids, id)
	}
	return jids, true
}

func registerFavoriteRoutes(db *sql.DB, client *whatsmeow.Client) {
	http.HandleFunc("/api/favorites/suggest", matterRoute(func(r SuggestFavoritesRequest) (map[string][]FavoriteSuggestion, error) {
		s, err := suggestFavorites(db, r, ownUsers(client), time.Now())
		return map[string][]FavoriteSuggestion{"suggestions": s}, err
	}))
	http.HandleFunc("/api/favorites/list", matterRoute(func(struct{}) (map[string][]APIFavorite, error) {
		f, err := listFavorites(db)
		return map[string][]APIFavorite{"favorites": f}, err
	}))
	http.HandleFunc("/api/favorites/decide", matterRoute(func(r DecideFavoriteRequest) (APIFavorite, error) {
		return decideFavorite(db, r, ownUsers(client), time.Now())
	}))
}
