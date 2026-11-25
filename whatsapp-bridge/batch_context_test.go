package main

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"testing"
)

func TestBatchMessageContext(t *testing.T) {
	s := setupChatStore(t)
	db, err := openUnaccentMessagesDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	mapping, err := sql.Open("sqlite3", "store/whatsapp.db")
	if err != nil {
		t.Fatal(err)
	}
	defer mapping.Close()
	if _, err := mapping.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT, pn TEXT); INSERT INTO whatsmeow_lid_map VALUES ('222', '111')`); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []string{"alpha@lid", "beta@g.us", "empty@g.us"} {
		if err := s.StoreChat(chat, chat, ts("2026-10-01T09:00:00Z")); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []struct{ chat, id, sender, at string }{
		{"alpha@lid", "old", "other", "2026-10-01T10:00:00+03:00"},
		{"alpha@lid", "target", "111", "2026-10-01T08:00:00Z"},
		{"alpha@lid", "tie", "222@lid", "2026-10-01T05:00:00-03:00"},
		{"alpha@lid", "new", "other", "2026-10-01T06:00:00-03:00"},
		{"beta@g.us", "beta", "111", "2026-10-01T08:30:00Z"},
		{"beta@g.us", "follower", "other", "2026-10-01T10:00:00Z"},
	} {
		readMessage(t, s.db, m.chat, m.id, m.sender, m.at, false)
		if err := s.StoreReaction(m.chat, m.id, "reactor", "ok", 0, ts(m.at)); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, payload string
		want          []string
	}{
		{"overlap", `{"chat_jid":"alpha@lid","sender_phone_number":"+111","context_before":1,"context_after":1}`, []string{"target", "tie", "new", "old"}},
		{"multiple chats", `{"sender_phone_number":"111","context_before":1,"context_after":1}`, []string{"beta", "follower", "target", "tie", "new", "old"}},
		{"boundaries", `{"chat_jid":"alpha@lid","context_before":2,"context_after":2}`, []string{"tie", "target", "new", "old"}},
		{"filtered page", `{"chat_jid":"alpha@lid","after":"2026-10-01T07:30:00Z","before":"2026-10-01T09:00:00Z","query":"t","limit":1,"page":1,"context_before":1,"context_after":1}`, []string{"old", "target", "tie"}},
		{"empty", `{"chat_jid":"empty@g.us"}`, []string{}},
		{"no matches", `{"query":"absent"}`, []string{}},
		{"legacy defaults", `{"chat_jid":"alpha@lid","query":"target"}`, []string{"old", "target", "tie", "new"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req MessagesRequest
			if err := json.Unmarshal([]byte(tc.payload), &req); err != nil {
				t.Fatal(err)
			}
			req.IncludeContext = true
			got, err := listMessages(db, req)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(readIDs(got.Messages), tc.want) {
				t.Fatalf("messages = %v, want %v", readIDs(got.Messages), tc.want)
			}
			for _, m := range got.Messages {
				if len(m.Reactions) != 1 || m.Reactions[0].Emoji != "ok" {
					t.Errorf("missing/duplicated reactions: %+v", m)
				}
			}
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err := json.Unmarshal(data, &response); err != nil || response["context_included"] != true {
				t.Fatalf("batch support not confirmed: %s, %v", data, err)
			}
		})
	}
	for _, id := range []string{"old", "target", "tie", "new", "beta"} {
		t.Run("single parity "+id, func(t *testing.T) {
			single, found, err := getMessageContext(db, MessageContextRequest{MessageID: id, Before: 2, After: 2})
			if err != nil || !found {
				t.Fatalf("single context: found=%v err=%v", found, err)
			}
			got, err := listMessages(db, MessagesRequest{IncludeContext: true, Query: &id, Limit: 1, ContextBefore: 2, ContextAfter: 2})
			want := append([]APIMessage{}, single.Before...)
			want = append(want, single.Message)
			want = append(want, single.After...)
			if err != nil || !reflect.DeepEqual(got.Messages, want) {
				t.Fatalf("batch = %+v, %v; single = %+v", got.Messages, err, want)
			}
		})
	}
}

func TestBatchMessageContextSharedIDs(t *testing.T) {
	s := setupChatStore(t)
	for _, chat := range []string{"alpha@lid", "beta@g.us"} {
		if err := s.StoreChat(chat, chat, ts("2026-10-01T08:00:00Z")); err != nil {
			t.Fatal(err)
		}
		readMessage(t, s.db, chat, "shared", "111", "2026-10-01T08:00:00Z", false)
		if err := s.StoreReaction(chat, "shared", "reactor", chat, 0, ts("2026-10-01T08:00:00Z")); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listMessages(s.db, MessagesRequest{IncludeContext: true})
	if err != nil || len(got.Messages) != 2 {
		t.Fatalf("shared IDs: %+v, %v", got, err)
	}
	for i, chat := range []string{"beta@g.us", "alpha@lid"} {
		m := got.Messages[i]
		if m.ChatJID != chat || len(m.Reactions) != 1 || m.Reactions[0].Emoji != chat {
			t.Errorf("context crossed chats: %+v", m)
		}
	}
}
