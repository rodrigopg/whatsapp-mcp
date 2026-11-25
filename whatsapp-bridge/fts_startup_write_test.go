//go:build sqlite_fts5 || fts5

package main

import (
	"testing"
	"time"
)

// The startup FTS backfill writes while the first messages arrive; a deferred
// transaction in StoreMessage got SQLITE_BUSY without waiting out busy_timeout.
func TestStoreMessageDuringStartupBackfill(t *testing.T) {
	for i := 0; i < 400; i++ {
		s := setupChatStore(t)
		at := time.Unix(100, 0)
		if err := s.StoreChat("111@s.whatsapp.net", "", at); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{"m1", "m2", "m3"} {
			if err := s.StoreMessage(id, "111@s.whatsapp.net", "111", "hello", at, false, "", "", "", nil, nil, nil, 0); err != nil {
				t.Fatalf("run %d, %s: %v", i, id, err)
			}
		}
		s.Close()
	}
}
