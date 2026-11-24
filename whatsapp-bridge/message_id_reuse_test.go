package main

import (
	"testing"
	"time"
)

// A message that reuses the ID of a stored one may only refresh it when it has the same author.
func TestStoreMessageKeepsAnotherAuthorsRow(t *testing.T) {
	store := setupChatStore(t)
	grp, at := "120363@g.us", ts("2026-10-10T10:00:00-03:00")
	put := func(id, sender, content string, fromMe bool) {
		t.Helper()
		if err := store.StoreMessage(id, grp, sender, content, at.Add(time.Minute), fromMe, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	row := func(id string) (sender, content string, fromMe bool) {
		t.Helper()
		if err := store.db.QueryRow(`SELECT sender, content, is_from_me FROM messages WHERE id = ? AND chat_jid = ?`, id, grp).Scan(&sender, &content, &fromMe); err != nil {
			t.Fatal(err)
		}
		return
	}
	seedActiveMsg(t, store.db, "A", grp, "original", "", false, at)
	if _, err := store.db.Exec(`UPDATE messages SET sender = '111' WHERE id = 'A'`); err != nil {
		t.Fatal(err)
	}
	seedActiveMsg(t, store.db, "MINE", grp, "eu escrevi", "", true, at)

	put("A", "222", "forged", false)
	if s, c, _ := row("A"); s != "111" || c != "original" {
		t.Errorf("another member replaced the row: sender=%q content=%q", s, c)
	}
	put("MINE", "222", "forged", false)
	if _, c, mine := row("MINE"); c != "eu escrevi" || !mine {
		t.Errorf("a contact replaced the owner's message: content=%q is_from_me=%v", c, mine)
	}
	put("A", "111", "forged as the owner", true)
	if _, c, mine := row("A"); c != "original" || mine {
		t.Errorf("row flipped to the owner: content=%q is_from_me=%v", c, mine)
	}

	// The same author still refreshes the row (re-sync, redelivery), and an empty text keeps the stored one.
	put("A", "111", "", false)
	put("A", "111", "refreshed", false)
	if _, c, _ := row("A"); c != "refreshed" {
		t.Errorf("same author: content = %q, want refreshed", c)
	}
	put("MINE", "999", "eu de novo", true)
	if _, c, _ := row("MINE"); c != "eu de novo" {
		t.Errorf("owner's own redelivery: content = %q", c)
	}
	// A row stored with the bare LID user is the same author as the full LID JID; an unknown author gets filled in.
	put("L", "4242", "lid antigo", false)
	put("L", "4242@lid", "lid novo", false)
	if _, c, _ := row("L"); c != "lid novo" {
		t.Errorf("legacy LID sender: content = %q", c)
	}
	put("U", "", "sem autor", false)
	put("U", "111", "sem autor", false)
	if s, _, _ := row("U"); s != "111" {
		t.Errorf("unknown author not filled in: %q", s)
	}
}
