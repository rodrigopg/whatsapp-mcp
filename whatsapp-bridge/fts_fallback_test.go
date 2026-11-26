package main

import (
	"reflect"
	"testing"
)

func TestMessageSearchLIKEFallback(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	seedChatWithLastMessage(t, s.db, chat, "Test", `CAFÉ São alpha OR "beta"`, "111", false, ts("2026-10-01T08:00:00Z"))
	readMessage(t, s.db, chat, "cafeteria", "111", "2026-10-01T08:00:00Z", false)
	db, err := openUnaccentMessagesDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"cafe", []string{"cafeteria", "msg1"}}, {"sao", []string{"msg1"}},
		{`alpha OR "beta"`, []string{"msg1"}}, {"%", []string{"cafeteria", "msg1"}},
	} {
		got, err := listMessagesWithSearch(db, MessagesRequest{Query: &tc.query}, false)
		if err != nil || !reflect.DeepEqual(readIDs(got.Messages), tc.want) {
			t.Fatalf("LIKE %q = %v, %v; want %v", tc.query, readIDs(got.Messages), err, tc.want)
		}
	}
	got, err := listMessagesWithSearch(db, MessagesRequest{Query: strp("cafe"), ChatJID: &chat,
		SenderPhoneNumber: strp("111"), After: strp("2026-10-01T07:59:00Z"),
		Before: strp("2026-10-01T08:01:00Z"), Limit: 1, Page: 1}, false)
	if err != nil || !reflect.DeepEqual(readIDs(got.Messages), []string{"msg1"}) {
		t.Fatalf("LIKE filters = %v, %v", readIDs(got.Messages), err)
	}
}
