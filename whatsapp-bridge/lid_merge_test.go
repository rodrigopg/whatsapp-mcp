package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type mapLIDStore struct {
	store.LIDStore
	pn map[string]string // LID user -> phone number
}

func (m mapLIDStore) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	if pn, ok := m.pn[lid.User]; ok {
		return types.NewJID(pn, types.DefaultUserServer), nil
	}
	return types.JID{}, nil
}

func lidClient(pn map[string]string) *whatsmeow.Client {
	return &whatsmeow.Client{Store: &store.Device{LIDs: mapLIDStore{pn: pn}}}
}

const (
	testLID = "999@lid"
	testPN  = "222@s.whatsapp.net"
)

func mustExec(t *testing.T, s *MessageStore, q string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func seedLIDPair(t *testing.T, s *MessageStore) {
	t.Helper()
	if err := s.StoreChat(testPN, "Ana", ts("2026-10-20T10:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreChat(testLID, "", ts("2026-10-21T13:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	// The same audio under both keys, named differently; only the LID copy got transcribed.
	for _, c := range []struct{ chat, content, filename string }{{testPN, "", "pn.ogg"}, {testLID, "transcribed", "a.ogg"}} {
		mustExec(t, s, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_length)
			VALUES ('m1', ?, '222', ?, ?, 0, 'audio', ?, 'u', x'01', 42)`, c.chat, c.content, ts("2026-10-20T09:00:00Z"), c.filename)
	}
	if err := s.StoreMessage("m2", testLID, "222", "hello", ts("2026-10-21T10:00:00Z"), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `INSERT INTO message_mentions (chat_jid, message_id, mentioned_user) VALUES (?, 'm2', '111')`, testLID)
	mustExec(t, s, `INSERT INTO polls (id, chat_jid, question, options, selectable_count) VALUES ('p1', ?, 'q', '[]', 1)`, testLID)
	mustExec(t, s, `INSERT INTO poll_votes (poll_id, chat_jid, voter, selected, timestamp) VALUES ('p1', ?, '111', '[]', ?)`, testLID, time.Unix(100, 0))
	mustExec(t, s, `INSERT INTO favorites (chat_jid, status, source, decided_at) VALUES (?, 'rejected', 'user', ?), (?, 'accepted', 'whatsapp', ?)`,
		testPN, time.Unix(100, 0), testLID, time.Unix(100, 0))
	mustExec(t, s, `INSERT INTO matters (chat_jid, owed_by, origin, origin_message_id, created_at, certainty, valid_until)
		VALUES (?, 'me', 'message', 'm2', ?, 'high', ?)`, testLID, time.Unix(100, 0), time.Unix(200, 0))
	mustExec(t, s, `INSERT INTO drafts (matter_id, chat_jid, text, state, written_at) VALUES (1, ?, 'draft', 'written', ?)`, testLID, time.Unix(100, 0))
	if err := os.MkdirAll(filepath.Join("store", testLID), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("store", testLID, "m1_a.ogg"), []byte("audio"), 0644); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, s *MessageStore, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func TestLIDMergeKeepsTranscriptionAndMovesEverything(t *testing.T) {
	s := setupChatStore(t)
	seedLIDPair(t, s)

	migrateLIDChats(lidClient(map[string]string{"999": "222"}), s, waLog.Noop)

	for _, table := range []string{"messages", "message_mentions", "polls", "poll_votes", "favorites", "matters", "drafts"} {
		if n := countRows(t, s, "SELECT COUNT(*) FROM "+table+" WHERE chat_jid = ?", testLID); n != 0 {
			t.Errorf("%s still has %d LID row(s)", table, n)
		}
		if n := countRows(t, s, "SELECT COUNT(*) FROM "+table+" WHERE chat_jid = ?", testPN); n == 0 {
			t.Errorf("%s has nothing under the phone number", table)
		}
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM chats WHERE jid = ?", testLID); n != 0 {
		t.Error("LID chat row left behind")
	}
	var content string
	if err := s.db.QueryRow("SELECT content FROM messages WHERE id = 'm1' AND chat_jid = ?", testPN).Scan(&content); err != nil || content != "transcribed" {
		t.Errorf("m1 content = %q, %v; want the LID copy's transcription", content, err)
	}
	var status string
	if err := s.db.QueryRow("SELECT status FROM favorites WHERE chat_jid = ?", testPN).Scan(&status); err != nil || status != "rejected" {
		t.Errorf("favorite = %q, %v; want the phone number's own decision", status, err)
	}
	assertChat(t, s, testPN, "Ana", ts("2026-10-21T10:00:00Z"))
	if _, err := os.Stat(filepath.Join("store", testPN, "m1_pn.ogg")); err != nil {
		t.Errorf("cached audio not moved under the merged row's filename: %v", err)
	}
	if _, err := os.Stat(filepath.Join("store", testLID)); !os.IsNotExist(err) {
		t.Errorf("LID media directory left behind: %v", err)
	}
}

func TestLIDMergeRollsBackWholeChatOnFailure(t *testing.T) {
	s := setupChatStore(t)
	seedLIDPair(t, s)
	mustExec(t, s, `CREATE TRIGGER fail_drafts BEFORE UPDATE ON drafts BEGIN SELECT RAISE(FAIL, 'test failure'); END`)

	migrateLIDChats(lidClient(map[string]string{"999": "222"}), s, waLog.Noop)

	if n := countRows(t, s, "SELECT COUNT(*) FROM messages WHERE chat_jid = ?", testLID); n != 2 {
		t.Errorf("LID messages = %d, want both kept after the rollback", n)
	}
	if n := countRows(t, s, "SELECT COUNT(*) FROM messages WHERE chat_jid = ? AND content = 'transcribed'", testPN); n != 0 {
		t.Error("partial merge committed")
	}
	if _, err := os.Stat(filepath.Join("store", testLID, "m1_a.ogg")); err != nil {
		t.Errorf("media moved for a chat that was not merged: %v", err)
	}
}

func TestLIDMergeLeavesUnmappedChats(t *testing.T) {
	s := setupChatStore(t)
	seedLIDPair(t, s)

	migrateLIDChats(lidClient(nil), s, waLog.Noop)

	if n := countRows(t, s, "SELECT COUNT(*) FROM messages WHERE chat_jid = ?", testLID); n != 2 {
		t.Errorf("LID messages = %d, want 2", n)
	}
}

func TestLIDAuthorsRewrittenOnceMapped(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("123-456@g.us", "Group", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	for id, sender := range map[string]string{"a": "999@lid", "b": "999", "c": "555", "d": "777@lid"} {
		if err := s.StoreMessage(id, "123-456@g.us", sender, "x", time.Unix(100, 0), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}

	migrateLIDChats(lidClient(map[string]string{"999": "222"}), s, waLog.Noop)

	for id, want := range map[string]string{"a": "222", "b": "222", "c": "555", "d": "777@lid"} {
		var got string
		if err := s.db.QueryRow("SELECT sender FROM messages WHERE id = ?", id).Scan(&got); err != nil || got != want {
			t.Errorf("sender of %s = %q, %v; want %q", id, got, err, want)
		}
	}
}

func TestSenderKeyKeepsUnresolvedLID(t *testing.T) {
	client := lidClient(map[string]string{"999": "222"})
	for in, want := range map[types.JID]string{
		{User: "999", Server: types.HiddenUserServer, Device: 3}: "222",
		{User: "777", Server: types.HiddenUserServer, Device: 3}: "777@lid",
		types.NewJID("333", types.DefaultUserServer):             "333",
	} {
		if got := senderKey(client, in); got != want {
			t.Errorf("senderKey(%s) = %q, want %q", in, got, want)
		}
		back, err := parseSender(want)
		if err != nil || back.User != want[:3] {
			t.Errorf("parseSender(%q) = %v, %v", want, back, err)
		}
	}
	if jid := pollCreatorJID(&whatsmeow.Client{Store: &store.Device{}}, "777@lid", false); jid.String() != "777@lid" {
		t.Errorf("poll creator = %s, want the stored LID", jid)
	}
}

func assertChat(t *testing.T, s *MessageStore, jid, name string, last time.Time) {
	t.Helper()
	var gotName string
	var gotTime time.Time
	if err := s.db.QueryRow("SELECT name, last_message_time FROM chats WHERE jid = ?", jid).Scan(&gotName, &gotTime); err != nil {
		t.Fatal(err)
	}
	if gotName != name || !gotTime.Equal(last) {
		t.Errorf("chat %s = %q %v, want %q %v", jid, gotName, gotTime, name, last)
	}
}

func TestLIDMediaMovedOnNextStartAfterACrash(t *testing.T) {
	s := setupChatStore(t)
	seedLIDPair(t, s)
	if err := mergeLIDChat(s.db, testLID, testPN); err != nil { // rows merged, then the process died
		t.Fatal(err)
	}

	migrateLIDChats(lidClient(map[string]string{"999": "222"}), s, waLog.Noop)

	if _, err := os.Stat(filepath.Join("store", testPN, "m1_pn.ogg")); err != nil {
		t.Errorf("leftover LID media not moved: %v", err)
	}
}

func pollVote(t *testing.T, s *MessageStore, chat, voter string) (string, time.Time) {
	t.Helper()
	var selected string
	var at time.Time
	if err := s.db.QueryRow(`SELECT selected, timestamp FROM poll_votes WHERE poll_id = 'p1' AND chat_jid = ? AND voter = ?`, chat, voter).Scan(&selected, &at); err != nil {
		t.Fatalf("vote of %s in %s: %v", voter, chat, err)
	}
	return selected, at
}

func TestLIDMergeKeepsNewerPollVote(t *testing.T) {
	for name, tc := range map[string]struct {
		pnAt, lidAt int64
		want        string
	}{
		"LID copy newer": {pnAt: 100, lidAt: 200, want: `["lid"]`},
		"PN copy newer":  {pnAt: 300, lidAt: 200, want: `["pn"]`},
	} {
		t.Run(name, func(t *testing.T) {
			s := setupChatStore(t)
			seedLIDPair(t, s)
			mustExec(t, s, `DELETE FROM poll_votes`)
			mustExec(t, s, `INSERT INTO poll_votes (poll_id, chat_jid, voter, selected, timestamp) VALUES
				('p1', ?, '111', '["pn"]', ?), ('p1', ?, '111', '["lid"]', ?)`, testPN, time.Unix(tc.pnAt, 0), testLID, time.Unix(tc.lidAt, 0))

			migrateLIDChats(lidClient(map[string]string{"999": "222"}), s, waLog.Noop)

			if got, _ := pollVote(t, s, testPN, "111"); got != tc.want {
				t.Errorf("vote = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestLIDVotersRewrittenNewerVoteWins(t *testing.T) {
	s := setupChatStore(t)
	const group = "123-456@g.us"
	mustExec(t, s, `INSERT INTO poll_votes (poll_id, chat_jid, voter, selected, timestamp) VALUES
		('p1', ?, '222', '["old"]', ?), ('p1', ?, '999@lid', '["new"]', ?), ('p2', ?, '999', '["only"]', ?), ('p2', ?, '222', '["newer"]', ?)`,
		group, time.Unix(100, 0), group, time.Unix(200, 0), group, time.Unix(100, 0), group, time.Unix(300, 0))

	migrateLIDChats(lidClient(map[string]string{"999": "222"}), s, waLog.Noop)

	if n := countRows(t, s, `SELECT COUNT(*) FROM poll_votes WHERE voter != '222'`); n != 0 {
		t.Errorf("%d vote(s) still under a LID", n)
	}
	if got, _ := pollVote(t, s, group, "222"); got != `["new"]` {
		t.Errorf("p1 vote = %s, want the newer LID vote", got)
	}
	var p2 string
	if err := s.db.QueryRow(`SELECT selected FROM poll_votes WHERE poll_id = 'p2'`).Scan(&p2); err != nil || p2 != `["newer"]` {
		t.Errorf("p2 vote = %s, %v; want the newer phone-number vote", p2, err)
	}
}

func TestChatDeleteTargetAddressesLIDAuthor(t *testing.T) {
	s := setupChatStore(t)
	const group = "123-456@g.us"
	if err := s.StoreChat(group, "Team", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("g1", group, "777@lid", "x", time.Unix(100, 0), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	_, _, key, err := chatDeleteTarget(s.db, group, "Team")
	if err != nil || key.GetParticipant() != "777@lid" {
		t.Fatalf("participant = %q, %v; want 777@lid", key.GetParticipant(), err)
	}
}

func TestApplyEditMatchesLegacyBareLIDAuthor(t *testing.T) {
	s := setupChatStore(t)
	const group = "123-456@g.us"
	if err := s.StoreChat(group, "Team", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("e1", group, "777", "before", time.Unix(100, 0), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ sender, want string }{{"778@lid", "before"}, {"777@lid", "after"}} {
		if err := s.ApplyEdit("e1", group, "after", false, tc.sender); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := s.db.QueryRow(`SELECT content FROM messages WHERE id = 'e1'`).Scan(&got); err != nil || got != tc.want {
			t.Errorf("edit by %s: content = %q, %v; want %q", tc.sender, got, err, tc.want)
		}
	}
}

func TestLIDMediaMoveUsesExactPlanForPrefixIDs(t *testing.T) {
	s := setupChatStore(t)
	seedLIDPair(t, s)
	mustExec(t, s, `INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename)
		VALUES ('m1_x', ?, '222', '', ?, 0, 'audio', 'old.ogg')`, testLID, ts("2026-10-20T09:00:00Z"))
	if err := os.WriteFile(filepath.Join("store", testLID, "m1_x_old.ogg"), []byte("other"), 0644); err != nil {
		t.Fatal(err)
	}

	migrateLIDChats(lidClient(map[string]string{"999": "222"}), s, waLog.Noop)

	for name, want := range map[string]string{"m1_pn.ogg": "audio", "m1_x_old.ogg": "other"} {
		if got, err := os.ReadFile(filepath.Join("store", testPN, name)); err != nil || string(got) != want {
			t.Errorf("%s = %q, %v; want %q", name, got, err, want)
		}
	}
}

func TestLIDMediaKeptWhenPlanUnreadable(t *testing.T) {
	s := setupChatStore(t)
	seedLIDPair(t, s)
	if err := mergeLIDChat(s.db, testLID, testPN); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `ALTER TABLE lid_media_moves RENAME TO lid_media_moves_gone`)

	if err := moveChatMedia(s.db, testLID, testPN); err == nil {
		t.Fatal("moved media without its plan")
	}
	if _, err := os.Stat(filepath.Join("store", testLID, "m1_a.ogg")); err != nil {
		t.Errorf("LID media touched without a plan: %v", err)
	}
}

func TestLegacyLIDVoterAdoptedBeforeNumberIsKnown(t *testing.T) {
	s := setupChatStore(t)
	const group, other = "123-456@g.us", "789-000@g.us"
	mustExec(t, s, `INSERT INTO poll_votes (poll_id, chat_jid, voter, selected, timestamp) VALUES
		('p1', ?, '777', '["old"]', ?), ('p1', ?, '777@lid', '["new"]', ?)`, group, time.Unix(100, 0), group, time.Unix(200, 0))
	for _, c := range []string{group, other} {
		if err := s.StoreChat(c, "Group", time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	for id, m := range map[string]struct{ chat, sender string }{"a": {group, "777"}, "b": {other, "777"}} {
		if err := s.StoreMessage(id, m.chat, m.sender, "x", time.Unix(100, 0), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}

	migrateLIDChats(lidClient(nil), s, waLog.Noop)

	if n := countRows(t, s, `SELECT COUNT(*) FROM poll_votes`); n != 1 {
		t.Fatalf("%d votes, want the voter counted once", n)
	}
	if got, _ := pollVote(t, s, group, "777@lid"); got != `["new"]` {
		t.Errorf("vote = %s, want the newer one", got)
	}
	for id, want := range map[string]string{"a": "777@lid", "b": "777"} {
		var got string
		if err := s.db.QueryRow(`SELECT sender FROM messages WHERE id = ?`, id).Scan(&got); err != nil || got != want {
			t.Errorf("sender of %s = %q, %v; want %q (only chats where the LID appears)", id, got, err, want)
		}
	}
}

func TestLiveLIDVoteMergesWithLegacyVote(t *testing.T) {
	const group = "123-456@g.us"
	for name, tc := range map[string]struct {
		legacyAt, liveAt int64
		want             string
	}{
		"live vote newer":   {legacyAt: 100, liveAt: 200, want: `["live"]`},
		"live vote delayed": {legacyAt: 300, liveAt: 200, want: `["old"]`},
	} {
		t.Run(name, func(t *testing.T) {
			s := setupChatStore(t)
			mustExec(t, s, `INSERT INTO poll_votes (poll_id, chat_jid, voter, selected, timestamp) VALUES ('p1', ?, '777', '["old"]', ?)`, group, time.Unix(tc.legacyAt, 0))
			if err := storePollVote(s.db, "p1", group, "777@lid", `["live"]`, time.Unix(tc.liveAt, 0)); err != nil {
				t.Fatal(err)
			}
			if n := countRows(t, s, `SELECT COUNT(*) FROM poll_votes`); n != 1 {
				t.Fatalf("%d votes, want the voter counted once", n)
			}
			if got, _ := pollVote(t, s, group, "777@lid"); got != tc.want {
				t.Errorf("vote = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestPollVoteKeptWhenStoringFails(t *testing.T) {
	s := setupChatStore(t)
	const group = "123-456@g.us"
	mustExec(t, s, `INSERT INTO poll_votes (poll_id, chat_jid, voter, selected, timestamp) VALUES ('p1', ?, '777', '["old"]', ?)`, group, time.Unix(100, 0))
	mustExec(t, s, `CREATE TRIGGER fail_vote BEFORE INSERT ON poll_votes BEGIN SELECT RAISE(FAIL, 'test failure'); END`)
	if err := storePollVote(s.db, "p1", group, "777@lid", `["live"]`, time.Unix(200, 0)); err == nil {
		t.Fatal("failure not reported")
	}
	if got, _ := pollVote(t, s, group, "777"); got != `["old"]` {
		t.Errorf("stored vote = %s after a failed write, want it untouched", got)
	}
}
