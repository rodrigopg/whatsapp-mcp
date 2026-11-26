//go:build sqlite_fts5 || fts5

package main

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func waitForFTS(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var ready int
		if err := db.QueryRow(`SELECT ready FROM message_fts_state WHERE id = 1`).Scan(&ready); err != nil {
			t.Fatal(err)
		}
		if ready == 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("FTS backfill did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func searchIDs(t *testing.T, db *sql.DB, req MessagesRequest, want ...string) {
	t.Helper()
	got, err := listMessages(db, req)
	if err != nil || !reflect.DeepEqual(readIDs(got.Messages), append([]string{}, want...)) {
		t.Fatalf("search = %v, %v; want %v", readIDs(got.Messages), err, want)
	}
}

func TestFTSSearch(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	seedChatWithLastMessage(t, s.db, chat, "Test", "CAFÉ São ộ Привет", "111", false, ts("2026-10-01T08:00:00Z"))
	readMessage(t, s.db, chat, "cafeteria", "111", "2026-10-01T08:00:00Z", false)
	for _, row := range []struct{ id, content string }{
		{"literal", `alpha OR "beta"`}, {"other", "alpha"}, {"syntax", "NEAR alpha beta 2"}, {"prefix", "alphabet"},
	} {
		readMessage(t, s.db, chat, row.id, "111", "2026-10-01T08:00:00Z", false)
		mustExec(t, s, `UPDATE messages SET content = ? WHERE id = ?`, row.content, row.id)
	}
	waitForFTS(t, s.db)
	for _, tc := range []struct {
		query string
		want  []string
	}{
		{"cafe", []string{"cafeteria", "msg1"}}, {"sao o", []string{"msg1"}}, {"привет", []string{"msg1"}}, {"прив", []string{"msg1"}},
		{`alpha OR "beta"`, []string{"literal"}}, {`NEAR(alpha beta, 2)`, []string{"syntax"}},
		{`alpha*`, []string{"prefix", "syntax", "other", "literal"}}, {`content:alpha`, nil},
		{`"`, nil}, {`%_*():`, nil}, {" \t", nil}, {"alpha\x00OR beta", []string{"literal"}},
	} {
		t.Run(tc.query, func(t *testing.T) {
			searchIDs(t, s.db, MessagesRequest{Query: &tc.query}, tc.want...)
		})
	}
}

func TestFTSFiltersAndPagination(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	seedChatWithLastMessage(t, s.db, chat, "Test", "café", "111", false, ts("2026-10-01T08:00:00Z"))
	for _, id := range []string{"second", "third"} {
		readMessage(t, s.db, chat, id, "111", "2026-10-01T05:00:00-03:00", false)
		mustExec(t, s, `UPDATE messages SET content = 'café' WHERE id = ?`, id)
	}
	seedChatWithLastMessage(t, s.db, "222@s.whatsapp.net", "Other", "café", "111", false, ts("2026-10-01T08:00:00Z"))
	readMessage(t, s.db, chat, "wrong-sender", "222", "2026-10-01T08:00:00Z", false)
	mustExec(t, s, `UPDATE messages SET content = 'café' WHERE id = 'wrong-sender'`)
	waitForFTS(t, s.db)
	req := MessagesRequest{Query: strp("cafe"), ChatJID: &chat, SenderPhoneNumber: strp("+111"),
		After: strp("2026-10-01T07:59:00Z"), Before: strp("2026-10-01T08:01:00Z"), Limit: 1, Page: 1}
	searchIDs(t, s.db, req, "second")
	req.After = strp("2026-10-01T08:00:00Z")
	searchIDs(t, s.db, req)
}

func TestFTSTranscriptionMergeAndDelete(t *testing.T) {
	s := setupChatStore(t)
	seedLIDPair(t, s)
	waitForFTS(t, s.db)
	searchIDs(t, s.db, MessagesRequest{Query: strp("transcribed")}, "m1")
	if err := mergeLIDChat(s.db, testLID, testPN); err != nil {
		t.Fatal(err)
	}
	searchIDs(t, s.db, MessagesRequest{Query: strp("transcribed"), ChatJID: strp(testPN)}, "m1")
	mustExec(t, s, `UPDATE messages SET content = 'new transcript café' WHERE id = 'm1'`)
	searchIDs(t, s.db, MessagesRequest{Query: strp("transcribed")})
	searchIDs(t, s.db, MessagesRequest{Query: strp("transcript cafe")}, "m1")
	if err := s.StoreMessage("m1", testPN, "222", "", ts("2026-10-01T08:00:00Z"), false, "audio", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	searchIDs(t, s.db, MessagesRequest{Query: strp("transcript cafe")}, "m1")
	if err := forgetChatLocally(s.db, testPN); err != nil {
		t.Fatal(err)
	}
	searchIDs(t, s.db, MessagesRequest{Query: strp("transcript cafe")})
	// External-content SELECTs read messages; integrity-check also checks for orphan postings.
	mustExec(t, s, `INSERT INTO messages_fts(messages_fts, rank) VALUES ('integrity-check', 1)`)
}

func TestFTSBackfillBatchesAndResume(t *testing.T) {
	s := setupChatStore(t)
	s.ftsCancel()
	<-s.ftsDone
	mustExec(t, s, `DROP TRIGGER messages_fts_insert;
		DROP TRIGGER messages_fts_update;
		DROP TRIGGER messages_fts_delete;
		DROP TABLE messages_fts;
		DROP TABLE message_fts_state;`)
	mustExec(t, s, `INSERT INTO chats(jid, name) VALUES ('111@s.whatsapp.net', 'Test')`)
	mustExec(t, s, `WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x < ?)
		INSERT INTO messages(id, chat_jid, sender, content, timestamp, is_from_me)
		SELECT 'm' || x, '111@s.whatsapp.net', '111', 'café old', '2026-10-01 08:00:00+00:00', 0 FROM n`, messageFTSBatchSize+3)
	if available, err := initMessageFTS(s.db); err != nil || !available {
		t.Fatalf("init FTS = %v, %v", available, err)
	}
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if done, err := backfillMessageFTS(ctx, s.db); err != nil || done {
			t.Fatalf("batch %d = %v, %v", i, done, err)
		}
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'cafe'`); n != messageFTSBatchSize {
		t.Fatalf("first batch indexed %d rows", n)
	}
	db, err := openUnaccentMessagesDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	searchIDs(t, db, MessagesRequest{Query: strp("cafe"), Limit: 1}, "m503")
	// Both sides of the watermark can change while the old LIKE path serves complete results.
	mustExec(t, s, `UPDATE messages SET content = 'changed' WHERE id IN ('m1', 'm501')`)
	mustExec(t, s, `DELETE FROM messages WHERE id IN ('m2', 'm502')`)
	readMessage(t, s.db, "111@s.whatsapp.net", "late", "111", "2026-10-01T08:00:00Z", false)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitForFTS(t, s.db)
	searchIDs(t, s.db, MessagesRequest{Query: strp("changed")}, "m501", "m1")
	searchIDs(t, s.db, MessagesRequest{Query: strp("late")}, "late")
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages_fts WHERE messages_fts MATCH 'cafe'`); n != messageFTSBatchSize-1 {
		t.Fatalf("resumed index has %d cafe rows", n)
	}
	mustExec(t, s, `INSERT INTO messages_fts(messages_fts, rank) VALUES ('integrity-check', 1)`)
}

func TestFTSUntaggedBinaryCompatibility(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "untagged.test")
	cmd := exec.Command("go", "test", "-c", "-tags=", "-o", binary, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile untagged tests: %v\n%s", err, out)
	}
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	seedChatWithLastMessage(t, s.db, chat, "Test", "obsolete", "111", false, ts("2026-10-01T08:00:00Z"))
	waitForFTS(t, s.db)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	cmd = exec.Command(binary, "-test.run=^TestFTSUnavailable$", "-test.v")
	cmd.Env = append(os.Environ(), "WA_FTS_TEST_DIR="+dir)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "--- PASS: TestFTSUnavailable") {
		t.Fatalf("untagged database reopen: %v\n%s", err, out)
	}
	s, err = NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	waitForFTS(t, s.db)
	searchIDs(t, s.db, MessagesRequest{Query: strp("obsolete")})
	searchIDs(t, s.db, MessagesRequest{Query: strp("transcribed cafe")}, "msg1")
	searchIDs(t, s.db, MessagesRequest{Query: strp("cafeteria")}, "late")
	mustExec(t, s, `INSERT INTO messages_fts(messages_fts, rank) VALUES ('integrity-check', 1)`)
}

func BenchmarkFTSMigration(b *testing.B) {
	for i := 0; i < b.N; i++ {
		db, err := sql.Open("sqlite3", filepath.Join(b.TempDir(), "messages.db"))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { db.Close() })
		_, err = db.Exec(`CREATE TABLE messages (id TEXT, chat_jid TEXT, content TEXT, PRIMARY KEY(id, chat_jid));
			WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x < 100000)
			INSERT INTO messages SELECT x, '111@s.whatsapp.net',
				'Hello café São Привет. Meeting notes and audio transcription. Please review the schedule for tomorrow, confirm the document and reply with your changes. Message number ' || x FROM n`)
		if err != nil {
			b.Fatal(err)
		}
		start := time.Now()
		if available, err := initMessageFTS(db); err != nil || !available {
			b.Fatalf("init = %v, %v", available, err)
		}
		b.ReportMetric(float64(time.Since(start).Microseconds()), "schema-us")
		start = time.Now()
		var maxBatch time.Duration
		batches := 0
		for {
			batchStart := time.Now()
			done, err := backfillMessageFTS(context.Background(), db)
			elapsed := time.Since(batchStart)
			if err != nil {
				b.Fatal(err)
			}
			if elapsed > maxBatch {
				maxBatch = elapsed
			}
			batches++
			if done {
				break
			}
		}
		b.ReportMetric(float64(maxBatch.Microseconds()), "max-batch-us")
		b.ReportMetric(float64(time.Since(start).Milliseconds()), "index-ms")
		b.ReportMetric(float64((batches-1)*25), "pacing-ms")
		if _, err := db.Exec(`INSERT INTO messages_fts(messages_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
			b.Fatal(err)
		}
	}
}
