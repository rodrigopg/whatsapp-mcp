package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A chat's media folder is one path component below store/, whatever the chat id says.
func TestChatMediaDir(t *testing.T) {
	for _, ok := range []string{"5562999990000@s.whatsapp.net", "120363000000000000@g.us", "123:45@lid", "status@broadcast"} {
		if dir, err := chatMediaDir(ok); err != nil || filepath.Dir(dir) != "store" {
			t.Errorf("chatMediaDir(%q) = %q, %v", ok, dir, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "../x", "../../outside@g.us", "a/b@g.us", `a\b@g.us`, "x..y@g.us"} {
		if dir, err := chatMediaDir(bad); err == nil {
			t.Errorf("chatMediaDir(%q) = %q, want a refusal", bad, dir)
		}
	}
}

// Downloading media of a row whose chat id is a path creates nothing outside the store.
func TestDownloadMediaRefusesAChatIDThatIsAPath(t *testing.T) {
	store := setupChatStore(t)
	chat := "../../outside-the-store@g.us"
	if _, err := store.db.Exec(`INSERT INTO chats (jid, name) VALUES (?, 'x')`, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) VALUES ('M1', ?, '111', '', ?, 0, 'image', 'a.jpg')`, chat, matterNow); err != nil {
		t.Fatal(err)
	}
	if ok, _, _, _, err := downloadMedia(nil, store, "M1", chat); ok || err == nil {
		t.Errorf("downloadMedia = %v, %v; want a refusal", ok, err)
	}
	if _, err := os.Stat(filepath.Join("store", chat)); err == nil {
		os.Remove(filepath.Join("store", chat))
		t.Error("a folder was created outside the store")
	}
	if _, err := safeMediaPath("store/x@g.us", "..hidden", "a.jpg"); err == nil {
		t.Error("a message id with a dot-dot must be refused")
	}
}
