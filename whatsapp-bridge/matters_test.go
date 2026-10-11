package main

import (
	"database/sql"
	"reflect"
	"testing"
	"time"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func strp(s string) *string { return &s }

// created 05/10 15:00 -03:00, so valid until 05/11 15:00 -03:00.
func newMatter(owedBy, origin string, deadline *string) Matter {
	created := ts("2026-10-05T15:00:00-03:00")
	return Matter{OwedBy: owedBy, Origin: origin, CreatedAt: created, Deadline: deadline,
		Certainty: "clear", ValidUntil: matterValidity(created)}
}

func TestMatterAttentionRules(t *testing.T) {
	day := func(d string) time.Time { return ts("2026-" + d + "T08:00:00-03:00") } // as_of: 08:00 of that day
	type step struct {
		asOf time.Time
		want string // "" = no attention, otherwise the reason
	}
	cases := map[string]struct {
		m     Matter
		steps []step
	}{
		"own promise with deadline: day it arose, deadline day and every day after": {
			newMatter("owner", "promise", strp("2026-10-09")),
			[]step{{day("10-05"), "own_promise_created_today"}, {day("10-06"), ""}, {day("10-08"), ""},
				{day("10-09"), "own_promise_deadline"}, {day("10-10"), "own_promise_deadline"}, {day("11-04"), "own_promise_deadline"}}},
		"other's promise with deadline: never before the deadline": {
			newMatter("other", "promise", strp("2026-10-09")),
			[]step{{day("10-05"), ""}, {day("10-08"), ""}, {day("10-09"), "other_deadline"}, {day("10-10"), "other_deadline"}}},
		"other's event with deadline behaves like a promise": {
			newMatter("other", "event", strp("2026-10-07")),
			[]step{{day("10-06"), ""}, {day("10-07"), "other_deadline"}}},
		"promise with no deadline: next day on, either side": {
			newMatter("owner", "promise", nil),
			[]step{{day("10-05"), ""}, {day("10-06"), "next_day"}, {day("10-20"), "next_day"}}},
		"other's promise with no deadline": {
			newMatter("other", "promise", nil),
			[]step{{day("10-05"), ""}, {day("10-06"), "next_day"}}},
		"request: next day on, even with a deadline in the text": {
			newMatter("owner", "request", strp("2026-10-09")),
			[]step{{day("10-05"), ""}, {day("10-06"), "next_day"}}},
		"demand received": {
			newMatter("owner", "demand", nil),
			[]step{{day("10-05"), ""}, {day("10-06"), "next_day"}}},
		"expired leaves the lists": {
			newMatter("owner", "request", nil),
			[]step{{ts("2026-11-05T14:59:00-03:00"), "next_day"}, {ts("2026-11-05T15:00:00-03:00"), ""}}},
	}
	for name, c := range cases {
		for _, s := range c.steps {
			got, reason := needsAttention(c.m, s.asOf)
			if got != (s.want != "") || reason != s.want {
				t.Errorf("%s @ %s: got (%v, %q), want %q", name, s.asOf.Format("01-02 15:04"), got, reason, s.want)
			}
		}
	}

	// Uncertain matters still appear (marked by the caller); closed ones never do.
	u := newMatter("owner", "request", nil)
	u.Certainty = "uncertain"
	if ok, _ := needsAttention(u, day("10-06")); !ok {
		t.Error("uncertain matter must appear in the attention list")
	}
	closed := newMatter("owner", "request", nil)
	closed.ClosedHow = strp("owner")
	if ok, _ := needsAttention(closed, day("10-06")); ok {
		t.Error("closed matter must not need attention")
	}
	// The day it arose is read in the caller's zone: 22:00 -03:00 of the 5th is already the 6th in UTC.
	late := newMatter("owner", "request", nil)
	late.CreatedAt = ts("2026-10-06T01:00:00Z")
	late.ValidUntil = matterValidity(late.CreatedAt)
	if ok, _ := needsAttention(late, ts("2026-10-06T08:00:00-03:00")); !ok {
		t.Error("created 22:00 of the 5th (-03:00): the 6th is the next day")
	}
}

func TestMatterStateAndValidity(t *testing.T) {
	m := newMatter("owner", "request", nil)
	for _, c := range []struct {
		now  string
		want string
	}{
		{"2026-11-05T14:59:59-03:00", "open"},
		{"2026-11-05T15:00:00-03:00", "expired"},
		{"2027-01-01T00:00:00-03:00", "expired"},
	} {
		if got := matterState(m, ts(c.now)); got != c.want {
			t.Errorf("state @ %s = %s, want %s", c.now, got, c.want)
		}
	}
	m.ClosedHow = strp("confirmed")
	if got := matterState(m, ts("2027-01-01T00:00:00-03:00")); got != "closed_confirmed" {
		t.Errorf("a closed matter never expires, got %s", got)
	}
	m.ClosedHow = strp("owner")
	if got := matterState(m, ts("2026-10-06T00:00:00-03:00")); got != "closed_owner" {
		t.Errorf("state = %s, want closed_owner", got)
	}
	// Reopening restarts the month from the reopening.
	reopened := ts("2026-12-01T10:00:00-03:00")
	if got := matterValidity(reopened); !got.Equal(ts("2027-01-01T10:00:00-03:00")) {
		t.Errorf("validity after reopen = %s", got)
	}
}

func TestMatterSchemaIsAdditive(t *testing.T) {
	store := setupChatStore(t)
	if _, err := store.db.Exec(`INSERT INTO matters (chat_jid, owed_by, origin, origin_message_id, snippet, created_at, certainty, valid_until)
		VALUES ('1@s.whatsapp.net', 'owner', 'request', 'm1', 'me manda', ?, 'clear', ?)`, time.Now(), time.Now()); err != nil {
		t.Fatalf("matters table: %v", err)
	}
	if _, err := store.db.Exec(`INSERT INTO matter_events (matter_id, kind, at, message_id) VALUES (1, 'opened', ?, 'm1')`, time.Now()); err != nil {
		t.Fatalf("matter_events table: %v", err)
	}
	if _, err := store.db.Exec(`INSERT INTO matter_events (matter_id, kind, at, message_id) VALUES (1, 'mention', ?, 'm1')`, time.Now()); err == nil {
		t.Error("the same message must not be recorded twice for one matter")
	}
	if _, err := store.db.Exec(`INSERT INTO review_cursor (id, reviewed_up_to) VALUES (1, ?)`, time.Now()); err != nil {
		t.Fatalf("review_cursor table: %v", err)
	}
}

// ---- store: record / close / reopen / list / review ----------------------------------------

var matterNow = ts("2026-10-12T09:00:00-03:00")

func seedMatterChat(t *testing.T) *sql.DB {
	t.Helper()
	db := setupChatStore(t).db
	at := func(d string) time.Time { return ts("2026-10-" + d + ":00-03:00") }
	// 111 asks (m1) and the owner promises (m2); 111 confirms (m3); an old message (m0); other chats.
	seedActiveMsg(t, db, "m1", "111@s.whatsapp.net", "me manda o contrato", "", false, at("10T10:00"))
	seedActiveMsg(t, db, "m2", "111@s.whatsapp.net", "mando ate sexta", "", true, at("10T10:05"))
	seedActiveMsg(t, db, "m3", "111@s.whatsapp.net", "recebi, obrigado", "", false, at("11T09:00"))
	seedActiveMsg(t, db, "m0", "111@s.whatsapp.net", "mensagem velha", "", false, ts("2026-08-01T10:00:00-03:00"))
	seedActiveMsg(t, db, "g1", "120363@g.us", "pedido no grupo", "", false, at("10T11:00"))
	seedActiveMsg(t, db, "s1", "999@s.whatsapp.net", "nota para mim", "", true, at("10T11:00"))
	seedActiveMsg(t, db, "x1", "222@s.whatsapp.net", "outra conversa", "", false, at("10T12:00"))
	return db
}

func rec(chat, msg, owedBy, origin string) RecordMatterRequest {
	return RecordMatterRequest{ChatJID: chat, MessageID: msg, OwedBy: owedBy, Origin: origin}
}

func mustRecord(t *testing.T, db *sql.DB, r RecordMatterRequest) APIMatter {
	t.Helper()
	m, err := recordMatter(db, r, []string{"999"}, matterNow)
	if err != nil {
		t.Fatalf("recordMatter(%+v): %v", r, err)
	}
	return m
}

func TestRecordMatter(t *testing.T) {
	db := seedMatterChat(t)
	dl := "2026-10-16"
	r := rec("111@s.whatsapp.net", "m2", "owner", "promise")
	r.Deadline = &dl
	m := mustRecord(t, db, r)
	if m.State != "open" || m.Snippet != "mando ate sexta" || m.Certainty != "clear" || m.Mentions != 1 || m.Deadline == nil || *m.Deadline != dl {
		t.Errorf("recorded matter = %+v", m)
	}
	if want := ts("2026-11-10T10:05:00-03:00"); m.ValidUntil != want.Format(time.RFC3339) {
		t.Errorf("valid_until = %s, want one month from the message (%s)", m.ValidUntil, want.Format(time.RFC3339))
	}
	// Same message again: same matter, no duplicate.
	again := mustRecord(t, db, r)
	if again.ID != m.ID {
		t.Errorf("recording the same message twice created matter %d, want %d", again.ID, m.ID)
	}
	// Another message about the same subject is a mention, not a new matter.
	mention := rec("111@s.whatsapp.net", "m3", "owner", "promise")
	mention.MatterID = &m.ID
	if got := mustRecord(t, db, mention); got.ID != m.ID || got.Mentions != 2 {
		t.Errorf("mention = id %d mentions %d, want id %d mentions 2", got.ID, got.Mentions, m.ID)
	}

	bad := map[string]RecordMatterRequest{
		"unknown message":  rec("111@s.whatsapp.net", "nope", "owner", "request"),
		"group":            rec("120363@g.us", "g1", "owner", "request"),
		"own chat":         rec("999@s.whatsapp.net", "s1", "owner", "request"),
		"older than month": rec("111@s.whatsapp.net", "m0", "owner", "request"),
		"bad owed_by":      rec("111@s.whatsapp.net", "m1", "me", "request"),
		"bad origin":       rec("111@s.whatsapp.net", "m1", "owner", "other"),
	}
	badDL := "sexta"
	withBadDL := rec("111@s.whatsapp.net", "m1", "owner", "promise")
	withBadDL.Deadline = &badDL
	bad["deadline not a date"] = withBadDL
	other := rec("222@s.whatsapp.net", "x1", "owner", "request")
	other.MatterID = &m.ID
	bad["matter of another chat"] = other
	for name, r := range bad {
		if _, err := recordMatter(db, r, []string{"999"}, matterNow); err == nil {
			t.Errorf("%s: accepted", name)
		} else if _, ok := err.(*errInvalidRequest); !ok {
			t.Errorf("%s: error %T, want *errInvalidRequest", name, err)
		}
	}
}

func TestCloseAndReopenMatter(t *testing.T) {
	db := seedMatterChat(t)
	m := mustRecord(t, db, rec("111@s.whatsapp.net", "m1", "owner", "request"))

	if _, err := closeMatter(db, CloseMatterRequest{MatterID: m.ID, By: "confirmed", MessageID: "m2"}, matterNow); err == nil {
		t.Error("closing by the owner's own message must be refused")
	}
	if _, err := closeMatter(db, CloseMatterRequest{MatterID: m.ID, By: "confirmed", MessageID: "x1"}, matterNow); err == nil {
		t.Error("closing by a message of another chat must be refused")
	}
	if _, err := closeMatter(db, CloseMatterRequest{MatterID: m.ID, By: "confirmed"}, matterNow); err == nil {
		t.Error("closing by confirmation needs a message")
	}
	closed, err := closeMatter(db, CloseMatterRequest{MatterID: m.ID, By: "confirmed", MessageID: "m3"}, matterNow)
	if err != nil || closed.State != "closed_confirmed" || !closed.AnnouncePending {
		t.Fatalf("close confirmed = %+v, %v", closed, err)
	}
	if _, err := closeMatter(db, CloseMatterRequest{MatterID: m.ID, By: "owner"}, matterNow); err == nil {
		t.Error("closing a matter that is not open must be refused")
	}

	// A message that is not newer than the matter cannot confirm it.
	late := mustRecord(t, db, rec("111@s.whatsapp.net", "m3", "other", "promise"))
	if _, err := closeMatter(db, CloseMatterRequest{MatterID: late.ID, By: "confirmed", MessageID: "m1"}, matterNow); err == nil {
		t.Error("a message older than the matter cannot confirm it")
	}
	byOwner, err := closeMatter(db, CloseMatterRequest{MatterID: late.ID, By: "owner"}, matterNow)
	if err != nil || byOwner.State != "closed_owner" || byOwner.AnnouncePending {
		t.Fatalf("close by owner = %+v, %v", byOwner, err)
	}

	// Reopen: same matter, history kept, validity restarts from the reopening.
	if _, err := reopenMatter(db, ReopenMatterRequest{MatterID: m.ID, MessageID: "m3"}, matterNow); err == nil {
		t.Error("reopening with the message that closed it must be refused")
	}
	reopened, err := reopenMatter(db, ReopenMatterRequest{MatterID: m.ID, MessageID: "x1"}, matterNow)
	if err == nil {
		t.Errorf("reopening by a message of another chat must be refused: %+v", reopened)
	}
	newer := ts("2026-10-12T08:00:00-03:00")
	seedActiveMsg(t, db, "m4", "111@s.whatsapp.net", "e aquele contrato?", "", false, newer)
	reopened, err = reopenMatter(db, ReopenMatterRequest{MatterID: m.ID, MessageID: "m4"}, matterNow)
	if err != nil || reopened.State != "open" || reopened.ID != m.ID || reopened.AnnouncePending {
		t.Fatalf("reopen = %+v, %v", reopened, err)
	}
	if reopened.ValidUntil != matterValidity(matterNow).Format(time.RFC3339) {
		t.Errorf("valid_until after reopen = %s", reopened.ValidUntil)
	}
	kinds := ""
	for _, e := range reopened.Events {
		kinds += e.Kind + " "
	}
	if kinds != "opened closed_confirmed reopened " {
		t.Errorf("history = %q", kinds)
	}
	if _, err := reopenMatter(db, ReopenMatterRequest{MatterID: m.ID, MessageID: "m4"}, matterNow); err == nil {
		t.Error("reopening an open matter must be refused")
	}
}

func TestListMatters(t *testing.T) {
	db := seedMatterChat(t)
	req := mustRecord(t, db, rec("111@s.whatsapp.net", "m1", "owner", "request")) // next day on
	dl := "2026-10-16"
	r := rec("111@s.whatsapp.net", "m2", "other", "promise")
	r.Deadline = &dl
	other := mustRecord(t, db, r) // only on the deadline
	closed := mustRecord(t, db, rec("222@s.whatsapp.net", "x1", "owner", "request"))
	if _, err := closeMatter(db, CloseMatterRequest{MatterID: closed.ID, By: "owner"}, matterNow); err != nil {
		t.Fatal(err)
	}
	ids := func(ms []APIMatter) []int64 {
		out := []int64{}
		for _, m := range ms {
			out = append(out, m.ID)
		}
		return out
	}
	asOf := "2026-10-12T07:00:00-03:00"
	for name, c := range map[string]struct {
		req  ListMattersRequest
		want []int64
	}{
		"attention today":   {ListMattersRequest{AsOf: asOf, AttentionOnly: true}, []int64{req.ID}},
		"attention on 16th": {ListMattersRequest{AsOf: "2026-10-16T07:00:00-03:00", AttentionOnly: true}, []int64{req.ID, other.ID}},
		"all":               {ListMattersRequest{AsOf: asOf}, []int64{req.ID, other.ID, closed.ID}},
		"closed only":       {ListMattersRequest{AsOf: asOf, State: "closed"}, []int64{closed.ID}},
		"one chat":          {ListMattersRequest{AsOf: asOf, ChatJID: "222@s.whatsapp.net"}, []int64{closed.ID}},
		"expired later":     {ListMattersRequest{AsOf: "2027-01-01T07:00:00-03:00", State: "expired"}, []int64{req.ID, other.ID}},
	} {
		got, err := listMatters(db, c.req)
		if err != nil || !reflect.DeepEqual(ids(got), c.want) {
			t.Errorf("%s: got %v (%v), want %v", name, ids(got), err, c.want)
		}
	}
	got, _ := listMatters(db, ListMattersRequest{AsOf: asOf, AttentionOnly: true})
	if got[0].Reason != "next_day" {
		t.Errorf("reason = %q, want next_day", got[0].Reason)
	}
	if _, err := listMatters(db, ListMattersRequest{AsOf: "hoje"}); err == nil {
		t.Error("as_of must be an ISO instant with offset")
	}
	if _, err := listMatters(db, ListMattersRequest{}); err == nil {
		t.Error("as_of is required")
	}
}

func TestReviewNextAndMark(t *testing.T) {
	db := seedMatterChat(t)
	mustRecord(t, db, rec("111@s.whatsapp.net", "m1", "owner", "request"))
	next, err := reviewNext(db, ReviewNextRequest{}, []string{"999"}, matterNow)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range next.Messages {
		got = append(got, m.ID)
	}
	// individual chats only, no own chat, no group, nothing older than a month, oldest first.
	if want := []string{"m1", "m2", "x1", "m3"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("unreviewed = %v, want %v", got, want)
	}
	if len(next.OpenMatters) != 1 || next.OpenMatters[0].OriginMessageID != "m1" {
		t.Errorf("open matters = %+v, want the one of chat 111", next.OpenMatters)
	}
	// Nothing moves the cursor but mark.
	if again, _ := reviewNext(db, ReviewNextRequest{}, []string{"999"}, matterNow); len(again.Messages) != 4 {
		t.Errorf("reviewNext moved the cursor by itself: %d messages", len(again.Messages))
	}
	if _, err := reviewMark(db, ReviewMarkRequest{UpTo: next.NextCursor}); err != nil {
		t.Fatal(err)
	}
	after, _ := reviewNext(db, ReviewNextRequest{}, []string{"999"}, matterNow)
	if len(after.Messages) != 0 {
		t.Errorf("after mark: %d messages, want 0", len(after.Messages))
	}
	// The cursor never goes back.
	if cur, _ := reviewMark(db, ReviewMarkRequest{UpTo: "2026-09-20T00:00:00-03:00"}); cur != next.NextCursor {
		t.Errorf("cursor went back to %s", cur)
	}
}

func TestReviewNextLimit(t *testing.T) {
	db := seedMatterChat(t)
	page, err := reviewNext(db, ReviewNextRequest{Limit: 2}, []string{"999"}, matterNow)
	if err != nil || len(page.Messages) != 2 || page.Messages[1].ID != "m2" {
		t.Fatalf("page 1 = %+v, %v", page, err)
	}
	if _, err := reviewMark(db, ReviewMarkRequest{UpTo: page.NextCursor}); err != nil {
		t.Fatal(err)
	}
	rest, _ := reviewNext(db, ReviewNextRequest{Limit: 2}, []string{"999"}, matterNow)
	if len(rest.Messages) != 2 || rest.Messages[0].ID != "x1" {
		t.Errorf("page 2 = %+v", rest.Messages)
	}
}
