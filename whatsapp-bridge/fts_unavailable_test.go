//go:build !sqlite_fts5 && !fts5

package main

import (
	"os"
	"reflect"
	"testing"
)

func TestFTSUnavailable(t *testing.T) {
	var s *MessageStore
	if dir := os.Getenv("WA_FTS_TEST_DIR"); dir != "" {
		t.Chdir(dir)
		var err error
		s, err = NewMessageStore()
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
	} else {
		s = setupChatStore(t)
		seedChatWithLastMessage(t, s.db, "111@s.whatsapp.net", "Test", "obsolete", "111", false, ts("2026-10-01T08:00:00Z"))
	}
	if ready, err := messageFTSReady(s.db); err != nil || ready {
		t.Fatalf("untagged FTS ready = %v, %v", ready, err)
	}
	mustExec(t, s, `UPDATE messages SET content = 'transcribed café' WHERE id = 'msg1'`)
	readMessage(t, s.db, "111@s.whatsapp.net", "late", "111", "2026-10-01T08:00:00Z", false)
	mustExec(t, s, `UPDATE messages SET content = 'cafeteria' WHERE id = 'late'`)
	readMessage(t, s.db, "111@s.whatsapp.net", "deleted", "111", "2026-10-01T08:00:00Z", false)
	mustExec(t, s, `DELETE FROM messages WHERE id = 'deleted'`)
	db, err := openUnaccentMessagesDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	got, err := listMessages(db, MessagesRequest{Query: strp("cafe")})
	if err != nil || !reflect.DeepEqual(readIDs(got.Messages), []string{"late", "msg1"}) {
		t.Fatalf("untagged LIKE = %v, %v", readIDs(got.Messages), err)
	}
}
