package main

import (
	"encoding/json"
	"net/http"
	"testing"
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
