package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChatDeleteTarget(t *testing.T) {
	db := setupChatStore(t).db
	grp := "120363@g.us"
	if _, err := db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'e2e-ab12 renamed', ?)`, grp, matterNow); err != nil {
		t.Fatal(err)
	}
	seedActiveMsg(t, db, "old", grp, "oi", "", false, ts("2026-10-10T10:00:00-03:00"))
	if _, err := db.Exec(`UPDATE messages SET sender = '5562911112222' WHERE id = 'old'`); err != nil {
		t.Fatal(err)
	}
	seedActiveMsg(t, db, "last", grp, "tchau", "", true, ts("2026-10-10T11:00:00-03:00"))

	name, lastTS, key, err := chatDeleteTarget(db, grp, "e2e-")
	if err != nil || name != "e2e-ab12 renamed" {
		t.Fatalf("target = %q, %v", name, err)
	}
	if !lastTS.Equal(ts("2026-10-10T11:00:00-03:00")) || key == nil || key.GetID() != "last" || !key.GetFromMe() || key.GetRemoteJID() != grp {
		t.Errorf("last message = %v %+v", lastTS, key)
	}
	// A wrong prefix, an empty prefix and an unknown chat are all refused: nothing is deleted by a typo.
	for name, c := range map[string][2]string{"wrong prefix": {grp, "Familia"}, "empty prefix": {grp, ""}, "unknown chat": {"999@g.us", "e2e-"}} {
		if _, _, _, err := chatDeleteTarget(db, c[0], c[1]); err == nil {
			t.Errorf("%s accepted", name)
		} else if _, ok := err.(*errInvalidRequest); !ok {
			t.Errorf("%s: error %T", name, err)
		}
	}
	// A chat with no stored message still has a target (zero time, no key).
	if _, err := db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES ('555@g.us', 'e2e-empty', ?)`, matterNow); err != nil {
		t.Fatal(err)
	}
	if _, ts0, k0, err := chatDeleteTarget(db, "555@g.us", "e2e-"); err != nil || !ts0.IsZero() || k0 != nil {
		t.Errorf("empty chat: %v %v %v", ts0, k0, err)
	}
}

func TestForgetChatLocally(t *testing.T) {
	db := setupChatStore(t).db
	grp, other := "120363@g.us", "111@s.whatsapp.net"
	seedActiveMsg(t, db, "g1", grp, "x", "", false, time.Now())
	seedActiveMsg(t, db, "o1", other, "y", "", false, time.Now())
	if err := storeMentions(db, grp, "g1", []string{"999@s.whatsapp.net"}); err != nil {
		t.Fatal(err)
	}
	if err := forgetChatLocally(db, grp); err != nil {
		t.Fatal(err)
	}
	count := func(q string, args ...interface{}) (n int) {
		_ = db.QueryRow(q, args...).Scan(&n)
		return
	}
	if count(`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, grp)+count(`SELECT COUNT(*) FROM chats WHERE jid = ?`, grp)+count(`SELECT COUNT(*) FROM message_mentions WHERE chat_jid = ?`, grp) != 0 {
		t.Error("the group's local rows must be gone")
	}
	if count(`SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, other) != 1 {
		t.Error("another chat must be untouched")
	}
}

func TestHandleDeleteChatValidation(t *testing.T) {
	h := handleDeleteChat(nil, nil)
	for name, c := range map[string]struct {
		method, body string
		want         int
	}{
		"get":           {http.MethodGet, "", http.StatusMethodNotAllowed},
		"bad json":      {http.MethodPost, "{", http.StatusBadRequest},
		"no jid":        {http.MethodPost, `{"expect_name_prefix":"e2e-"}`, http.StatusBadRequest},
		"no prefix":     {http.MethodPost, `{"chat_jid":"1@g.us"}`, http.StatusBadRequest},
		"invalid jid":   {http.MethodPost, `{"chat_jid":"x:y:z@g.us","expect_name_prefix":"e2e-"}`, http.StatusBadRequest},
		"not connected": {http.MethodPost, `{"chat_jid":"1@g.us","expect_name_prefix":"e2e-"}`, http.StatusServiceUnavailable},
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(c.method, "/api/chat/delete", strings.NewReader(c.body)))
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, c.want, rec.Body.String())
		}
	}
}
