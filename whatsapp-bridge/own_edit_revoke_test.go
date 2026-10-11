package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestApplyOwnProtocolAction(t *testing.T) {
	store := setupChatStore(t)
	ts := time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)
	chat := types.NewJID("123", types.DefaultUserServer)
	if err := store.StoreChat(chat.String(), "Alice", ts); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"e", "r"} {
		if err := store.StoreMessage(id, chat.String(), "me", "old", ts, true, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	get := func(id string) string {
		var s string
		if err := store.db.QueryRow("SELECT content FROM messages WHERE id=? AND chat_jid=?", id, chat.String()).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}

	applyOwnProtocolAction(nil, store, chat, "e", "new text")
	if got := get("e"); got != "new text" {
		t.Errorf("own edit: content = %q, want %q", got, "new text")
	}
	applyOwnProtocolAction(nil, store, chat, "r", revokedContent)
	if got := get("r"); got != revokedContent {
		t.Errorf("own revoke: content = %q, want %q", got, revokedContent)
	}
	t.Run("edit_empty_text_keeps_row", func(t *testing.T) {
		applyOwnProtocolAction(nil, store, chat, "e", "")
		applyOwnProtocolAction(nil, store, chat, "e", "   ")
		if got := get("e"); got != "new text" {
			t.Errorf("empty own edit: content = %q, want %q", got, "new text")
		}
	})
	// A nil store (handler built without one) must not panic.
	applyOwnProtocolAction(nil, nil, chat, "e", "x")
}
