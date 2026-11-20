package main

import (
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func TestStatusPage(t *testing.T) {
	get := func(s statusInfo) string {
		w := httptest.NewRecorder()
		handleStatus(func() statusInfo { return s })(w, httptest.NewRequest("GET", "/status", nil))
		return w.Body.String()
	}
	if b := get(statusInfo{Connected: true, Account: maskAccount("5511900001234"), Messages: 3}); !strings.Contains(b, "Connected") || !strings.Contains(b, "•••1234") || strings.Contains(b, "55119") || strings.Contains(b, "/qr.png") {
		t.Errorf("connected page wrong: %s", b)
	}
	if b := get(statusInfo{HasQR: true}); !strings.Contains(b, "scan to pair") || !strings.Contains(b, "/qr.png") {
		t.Errorf("qr page wrong: %s", b)
	}
	if b := get(statusInfo{}); !strings.Contains(b, "Waiting for a QR") {
		t.Errorf("idle page wrong: %s", b)
	}
	w := httptest.NewRecorder()
	handleStatus(nil)(w, httptest.NewRequest("POST", "/status", nil))
	if w.Code != 405 {
		t.Errorf("POST = %d, want 405", w.Code)
	}
}

func TestLastMessage(t *testing.T) {
	db, _ := sql.Open("sqlite3", ":memory:")
	db.Exec(`CREATE TABLE messages (timestamp TIMESTAMP)`)
	if n, last := lastMessage(db); n != 0 || last != "" {
		t.Errorf("empty = %d %q", n, last)
	}
	db.Exec(`INSERT INTO messages VALUES (?), (?)`, "2026-10-21 08:35:23-03:00", "2026-10-21T12:00:00Z")
	if n, last := lastMessage(db); n != 2 || last != "21/10 09:00:00 BRT" {
		t.Errorf("got %d %q", n, last)
	}
}
