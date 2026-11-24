package main

import (
	"testing"
	"time"
)

// The review cursor never passes the present: a future instant would hide every later message.
func TestReviewMarkClampsToNow(t *testing.T) {
	db := seedMatterChat(t)
	cur, err := reviewMark(db, ReviewMarkRequest{UpTo: "2099-01-01T00:00:00-03:00"}, matterNow)
	if err != nil {
		t.Fatal(err)
	}
	if got := ts(cur); got.After(matterNow) {
		t.Fatalf("cursor = %s, after now (%s)", cur, matterNow)
	}
	later := matterNow.AddDate(0, 0, 1)
	seedActiveMsg(t, db, "new1", "111@s.whatsapp.net", "mensagem nova", "", false, later.Add(-time.Second))
	next, err := reviewNext(db, ReviewNextRequest{}, []string{"999"}, later)
	if err != nil || len(next.Messages) != 1 || next.Messages[0].ID != "new1" {
		t.Fatalf("after a future mark: %+v, %v", next.Messages, err)
	}
}

// A cursor already stored in the future (older versions accepted it) is brought back to the present
// on the next read, so the messages that arrive from then on are reviewed again.
func TestReviewCursorStoredInTheFutureHeals(t *testing.T) {
	db := seedMatterChat(t)
	if _, err := db.Exec(`INSERT INTO review_cursor (id, reviewed_up_to) VALUES (1, ?)`, ts("2099-01-01T00:00:00-03:00")); err != nil {
		t.Fatal(err)
	}
	later := matterNow.AddDate(0, 0, 1)
	if next, err := reviewNext(db, ReviewNextRequest{}, []string{"999"}, later); err != nil || len(next.Messages) != 0 {
		t.Fatalf("first read: %+v, %v", next.Messages, err)
	}
	seedActiveMsg(t, db, "new1", "111@s.whatsapp.net", "mensagem nova", "", false, later.Add(time.Minute))
	next, err := reviewNext(db, ReviewNextRequest{}, []string{"999"}, later.Add(time.Hour))
	if err != nil || len(next.Messages) != 1 || next.Messages[0].ID != "new1" {
		t.Fatalf("a message after the repair is not reviewed: %+v, %v", next.Messages, err)
	}
	if cur, err := reviewMark(db, ReviewMarkRequest{UpTo: next.NextCursor}, later.Add(time.Hour)); err != nil || !ts(cur).Equal(later.Add(time.Minute)) {
		t.Fatalf("mark after the repair = %s, %v", cur, err)
	}
}
