//go:build sqlite_fts5 || fts5

package main

import "testing"

func TestFTSEachWordIsIndependentPrefix(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	seedChatWithLastMessage(t, s.db, chat, "Test", "привет мировой caféteria 12345 Ⅻ3456", "111", false, ts("2026-10-01T08:00:00Z"))
	for _, row := range []struct{ id, content string }{
		{"separated", "привет большой мировой caféteria 12345 Ⅻ3456"},
		{"reversed", "мировой привет caféteria 12345 Ⅻ3456"},
		{"other", "привет монстр caféteria 12345 Ⅻ3456"},
	} {
		readMessage(t, s.db, chat, row.id, "111", "2026-10-01T08:00:00Z", false)
		mustExec(t, s, `UPDATE messages SET content = ? WHERE id = ?`, row.content, row.id)
	}
	readMessage(t, s.db, chat, "private-use", "111", "2026-10-01T08:00:00Z", false)
	mustExec(t, s, `UPDATE messages SET content = ? WHERE id = ?`, "прив\uE000мир", "private-use")
	waitForFTS(t, s.db)
	privateUse := "прив\uE000мир"
	searchIDs(t, s.db, MessagesRequest{Query: &privateUse}, "private-use")
	for _, query := range []string{
		"прив мир", "прив-мир", "прив,мир", "прив—мир", "прив😀мир",
		"прив_мир", "прив\x00мир", `"прив","мир"`, "прив-мир cafe\u0301t-123-Ⅻ34",
	} {
		t.Run(query, func(t *testing.T) {
			searchIDs(t, s.db, MessagesRequest{Query: &query}, "reversed", "separated", "msg1")
		})
	}
	for _, query := range []string{"- , — 😀 _", "\x00", `"*():%`} {
		t.Run(query, func(t *testing.T) {
			searchIDs(t, s.db, MessagesRequest{Query: &query})
		})
	}
}
