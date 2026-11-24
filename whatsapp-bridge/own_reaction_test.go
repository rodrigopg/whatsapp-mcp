package main

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// The bridge's own reactions are stored when sent: WhatsApp does not echo them back.
func TestOwnReactionStored(t *testing.T) {
	s := setupChatStore(t)
	seedActionMessage(t, s, "target", "222@s.whatsapp.net", "222", "hello", false)
	client := newFakeMessageActions()
	reactions := func() []APIReaction {
		t.Helper()
		messages := []APIMessage{{ID: "target", ChatJID: "222@s.whatsapp.net"}}
		if err := loadReactions(s.db, messages); err != nil {
			t.Fatal(err)
		}
		return messages[0].Reactions
	}
	for _, c := range []struct{ emoji, want string }{{"👍", "👍"}, {"❤️", "❤️"}, {"", ""}} {
		body, _ := json.Marshal(ReactRequest{ChatJID: "222@s.whatsapp.net", MessageID: "target", Emoji: c.emoji})
		if rec := doHandlerRequest(t, handleReact(client, s), http.MethodPost, body); rec.Code != http.StatusOK {
			t.Fatalf("%q: status %d: %s", c.emoji, rec.Code, rec.Body.String())
		}
		got := reactions()
		if c.want == "" {
			if len(got) != 0 {
				t.Errorf("removal left %+v", got)
			}
			continue
		}
		if len(got) != 1 || got[0].Sender != "111" || got[0].Emoji != c.want {
			t.Errorf("after %q: %+v", c.emoji, got)
		}
	}
}

// The same own reaction delivered later by history sync (from another of the account's devices)
// lands on the row written at send time, not on a second one.
func TestOwnReactionMatchesHistoryAuthor(t *testing.T) {
	s := setupChatStore(t)
	seedActionMessage(t, s, "target", "222@s.whatsapp.net", "222", "hello", false)
	body, _ := json.Marshal(ReactRequest{ChatJID: "222@s.whatsapp.net", MessageID: "target", Emoji: "👍"})
	if rec := doHandlerRequest(t, handleReact(newFakeMessageActions(), s), http.MethodPost, body); rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	own := types.JID{User: "111", Server: types.DefaultUserServer, Device: 2}
	if err := s.StoreReaction("222@s.whatsapp.net", "target", senderKey(nil, own.ToNonAD()), "👍", 1, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reactions WHERE message_id = 'target'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reaction rows = %d, %v; want 1", n, err)
	}
}
