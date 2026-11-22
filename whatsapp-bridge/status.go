package main

import (
	"database/sql"
	"time"
)

var (
	brt       = time.FixedZone("BRT", -3*3600)
	startedAt = time.Now()
)

// maskAccount keeps the last 4 digits: /status is open without the token, like /qr.
func maskAccount(user string) string {
	if len(user) <= 4 {
		return user
	}
	return "•••" + user[len(user)-4:]
}

// statusInfo is what /status shows. Built per request from live state, never cached.
type statusInfo struct {
	Connected bool
	Account   string // last digits of the paired account (maskAccount), empty when not paired
	HasQR     bool   // waiting for a scan
	Uptime    string
	Messages  int
	LastMsg   string // newest stored message, BRT; a stale value with Connected=true means the DB stopped recording

	LastMsgAt   time.Time     // zero when unknown
	Paired      bool          // a session exists in the store
	Pairing     bool          // paired in this run and history is still arriving
	Held        time.Duration // WhatsApp asked to wait this long (ban, second client)
	Stopped     bool          // needs a person (outdated client, logout)
	LoggedOutAt time.Time     // WhatsApp removed this device at this time; zero otherwise
}

// lastMessage returns the newest stored message time in BRT ("" when the table is empty or unparsable).
func lastMessage(db *sql.DB) (n int, last string) {
	n, last, _ = lastMessageAt(db)
	return
}

func lastMessageAt(db *sql.DB) (n int, last string, at time.Time) {
	var raw sql.NullString
	if db.QueryRow(`SELECT COUNT(*), MAX(timestamp) FROM messages`).Scan(&n, &raw) != nil || !raw.Valid {
		return n, "", at
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.Parse(layout, raw.String); err == nil {
			return n, t.In(brt).Format("02/01 15:04:05 BRT"), t
		}
	}
	return n, raw.String, at
}
