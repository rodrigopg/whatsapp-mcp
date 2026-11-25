package main

import (
	"fmt"
	"testing"
)

// A message with a huge or repetitive mention list costs one bounded write.
func TestStoreMentionsDedupesAndCaps(t *testing.T) {
	db := seedMatterChat(t)
	var jids []string
	for i := 0; i < maxStoredMentions+500; i++ {
		jids = append(jids, fmt.Sprintf("%d@s.whatsapp.net", 5500000000+i), "999@s.whatsapp.net", "999:7@s.whatsapp.net")
	}
	if err := storeMentions(db, "120363@g.us", "big", jids); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM message_mentions WHERE message_id = 'big'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != maxStoredMentions {
		t.Errorf("stored %d mentions, want the cap %d", n, maxStoredMentions)
	}
	if mine, err := mentionsOwner(db, "120363@g.us", "big", []string{"999"}); err != nil || !mine {
		t.Errorf("owner mention lost: %v, %v", mine, err)
	}
}
