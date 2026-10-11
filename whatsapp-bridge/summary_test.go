package main

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"
)

// seedSummary: favorite Ana (request), Bia (demand), Caio (own promise due today),
// Dora (request), Eli (other's promise, no deadline). Messages are older than the as_of day.
func seedSummary(t *testing.T) *sql.DB {
	t.Helper()
	db := setupChatStore(t).db
	day := func(d, h string) time.Time { return ts("2026-10-" + d + "T" + h + ":00:00-03:00") }
	for i, c := range []struct{ jid, name, msg, kind string }{
		{"1@s.whatsapp.net", "Ana", "ana1", "request"},
		{"2@s.whatsapp.net", "Bia", "bia1", "demand"},
		{"3@s.whatsapp.net", "Caio", "caio1", "promise"},
		{"4@s.whatsapp.net", "Dora", "dora1", "request"},
		{"5@s.whatsapp.net", "Eli", "eli1", "promise"},
	} {
		if _, err := db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`, c.jid, c.name, day("10", "08")); err != nil {
			t.Fatal(err)
		}
		// later chats are more recent: Ana 09h ... Eli 13h on the 10th
		seedActiveMsg(t, db, c.msg, c.jid, "texto de "+c.name, "", c.kind == "promise" && c.jid == "3@s.whatsapp.net", day("10", fmt.Sprintf("%02d", 9+i)))
	}
	return db
}

func recordFor(t *testing.T, db *sql.DB, chat, msg, owedBy, origin string, deadline *string) APIMatter {
	t.Helper()
	r := rec(chat, msg, owedBy, origin)
	r.Deadline = deadline
	return mustRecord(t, db, r)
}

func summaryNames(s APISummary) []string {
	out := []string{}
	for _, i := range s.Items {
		out = append(out, i.Name)
	}
	return out
}

func TestMorningSummaryOrderLimitAndSnapshot(t *testing.T) {
	db := seedSummary(t)
	today := "2026-10-12"
	recordFor(t, db, "1@s.whatsapp.net", "ana1", "owner", "request", nil)
	recordFor(t, db, "2@s.whatsapp.net", "bia1", "owner", "demand", nil)
	recordFor(t, db, "3@s.whatsapp.net", "caio1", "owner", "promise", &today)
	recordFor(t, db, "4@s.whatsapp.net", "dora1", "owner", "request", nil)
	recordFor(t, db, "5@s.whatsapp.net", "eli1", "other", "promise", nil)
	if _, err := decideFavorite(db, DecideFavoriteRequest{ChatJID: "1@s.whatsapp.net", Decision: "accept"}, nil, matterNow); err != nil {
		t.Fatal(err)
	}
	asOf := "2026-10-12T07:00:00-03:00"
	s, err := morningSummary(db, SummaryRequest{AsOf: asOf}, nil, matterNow)
	if err != nil {
		t.Fatal(err)
	}
	// favorites first; then who chased the owner / what the owner promised; then the rest. Most recent first inside a group.
	if want := []string{"Ana", "Caio", "Bia", "Eli", "Dora"}; !reflect.DeepEqual(summaryNames(s), want) {
		t.Fatalf("order = %v, want %v", summaryNames(s), want)
	}
	groups := map[string]string{}
	for _, i := range s.Items {
		groups[i.Name] = i.Group
	}
	if !reflect.DeepEqual(groups, map[string]string{"Ana": "favorite", "Bia": "charged_or_promised", "Caio": "charged_or_promised", "Dora": "other", "Eli": "other"}) {
		t.Errorf("groups = %v", groups)
	}
	if s.Items[0].Link == nil || *s.Items[0].Link != "https://wa.me/1" || s.Items[0].Snippet == "" || s.Items[0].Reason != "next_day" || s.Items[0].Time == "" {
		t.Errorf("first item = %+v", s.Items[0])
	}
	if s.Total != 5 || s.More != 0 || s.Day != "2026-10-12" {
		t.Errorf("summary = total %d more %d day %s", s.Total, s.More, s.Day)
	}

	// A snapshot: a new matter later the same day does not change it; another day builds its own.
	seedActiveMsg(t, db, "late1", "4@s.whatsapp.net", "mais um", "", false, ts("2026-10-11T09:00:00-03:00"))
	recordFor(t, db, "4@s.whatsapp.net", "late1", "owner", "request", nil)
	again, err := morningSummary(db, SummaryRequest{AsOf: "2026-10-12T15:00:00-03:00"}, nil, matterNow)
	if err != nil || !reflect.DeepEqual(again, s) {
		t.Errorf("same day must return the same snapshot: %+v vs %+v (%v)", again, s, err)
	}
	next, _ := morningSummary(db, SummaryRequest{AsOf: "2026-10-13T07:00:00-03:00"}, nil, matterNow)
	if next.Day != "2026-10-13" || next.Total == 0 {
		t.Errorf("next day = %+v", next)
	}
}

func TestMorningSummaryLimitAndAll(t *testing.T) {
	db := setupChatStore(t).db
	for i := 0; i < 12; i++ {
		jid := string(rune('a'+i)) + "@s.whatsapp.net"
		id := "m" + string(rune('a'+i))
		if _, err := db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`, jid, "P"+string(rune('a'+i)), matterNow); err != nil {
			t.Fatal(err)
		}
		seedActiveMsg(t, db, id, jid, "oi", "", false, ts("2026-10-10T09:00:00-03:00").Add(time.Duration(i)*time.Minute))
		recordFor(t, db, jid, id, "owner", "request", nil)
	}
	s, err := morningSummary(db, SummaryRequest{AsOf: "2026-10-12T07:00:00-03:00"}, nil, matterNow)
	if err != nil || len(s.Items) != 10 || s.More != 2 || s.Total != 12 {
		t.Fatalf("limit: %d items, more %d, total %d (%v)", len(s.Items), s.More, s.Total, err)
	}
	all, _ := morningSummary(db, SummaryRequest{AsOf: "2026-10-12T07:00:00-03:00", All: true}, nil, matterNow)
	if len(all.Items) != 12 || all.More != 0 {
		t.Errorf("all: %d items, more %d", len(all.Items), all.More)
	}
	if all.Items[0].Name != "Pl" {
		t.Errorf("most recent first, got %s", all.Items[0].Name)
	}
}

func TestMorningSummaryAnnouncesClosuresOnce(t *testing.T) {
	db := seedSummary(t)
	m := recordFor(t, db, "2@s.whatsapp.net", "bia1", "owner", "request", nil)
	seedActiveMsg(t, db, "bia2", "2@s.whatsapp.net", "feito, obrigado", "", false, ts("2026-10-11T09:00:00-03:00"))
	if _, err := closeMatter(db, CloseMatterRequest{MatterID: m.ID, By: "confirmed", MessageID: "bia2"}, matterNow); err != nil {
		t.Fatal(err)
	}
	s, err := morningSummary(db, SummaryRequest{AsOf: "2026-10-12T07:00:00-03:00"}, nil, matterNow)
	if err != nil || len(s.Announcements) != 1 || s.Announcements[0].MatterID != m.ID || s.Announcements[0].Name != "Bia" {
		t.Fatalf("announcements = %+v, %v", s.Announcements, err)
	}
	if len(s.Items) != 0 {
		t.Errorf("a closed matter is not an item: %v", summaryNames(s))
	}
	got, _ := listMatters(db, ListMattersRequest{AsOf: "2026-10-12T07:00:00-03:00", State: "closed"})
	if len(got) != 1 || got[0].AnnouncePending {
		t.Errorf("after the summary the closure must no longer be pending: %+v", got)
	}
	// Announced once: the next day does not repeat it.
	next, _ := morningSummary(db, SummaryRequest{AsOf: "2026-10-13T07:00:00-03:00"}, nil, matterNow)
	if len(next.Announcements) != 0 {
		t.Errorf("announced once only, got %+v", next.Announcements)
	}
}

func TestMorningSummaryEmptyDayIsStoredAndBadAsOfRefused(t *testing.T) {
	db := setupChatStore(t).db
	s, err := morningSummary(db, SummaryRequest{AsOf: "2026-10-12T07:00:00-03:00"}, nil, matterNow)
	if err != nil || s.Items == nil || s.Announcements == nil || s.Total != 0 {
		t.Fatalf("empty summary = %+v, %v", s, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM summaries WHERE day = '2026-10-12'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("empty day not stored: %d, %v", n, err)
	}
	if _, err := morningSummary(db, SummaryRequest{AsOf: "hoje"}, nil, matterNow); err == nil {
		t.Error("as_of must be an instant with offset")
	}
}
