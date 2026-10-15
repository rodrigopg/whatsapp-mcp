package main

import (
	"fmt"
	"strings"
	"testing"
)

// TestMessageStoreIndexes checks the schema NewMessageStore creates. An index is
// only worth anything if the planner actually picks it - existing in
// sqlite_master is not the same as being used, so this asserts both.
func TestMessageStoreIndexes(t *testing.T) {
	store := setupChatStore(t)

	for _, name := range []string{
		"idx_messages_chat_time",
		"idx_messages_audio_pending",
		"idx_senders_names",
	} {
		var found string
		err := store.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&found)
		if err != nil {
			t.Errorf("index %s not created: %v", name, err)
		}
	}

	// The reads that matter must resolve through the index instead of scanning.
	for _, tc := range []struct {
		name  string
		query string
		args  []any
		want  string
	}{
		{
			name:  "recent messages of one chat",
			query: "SELECT id FROM messages WHERE chat_jid=? ORDER BY timestamp DESC LIMIT 20",
			args:  []any{"x@g.us"},
			want:  "idx_messages_chat_time",
		},
		{
			name:  "audio pending transcription",
			query: "SELECT id FROM messages WHERE media_type='audio' AND (content IS NULL OR content='')",
			args:  nil,
			want:  "idx_messages_audio_pending",
		},
	} {
		rows, err := store.db.Query("EXPLAIN QUERY PLAN "+tc.query, tc.args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", tc.name, err)
		}
		var plan strings.Builder
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			cells := make([]any, len(cols))
			for i := range cells {
				cells[i] = new(any)
			}
			if err := rows.Scan(cells...); err != nil {
				t.Fatalf("%s: scan: %v", tc.name, err)
			}
			// The last column is the human-readable detail ("SEARCH ... USING INDEX ...").
			fmt.Fprintf(&plan, "%v ", *(cells[len(cells)-1].(*any)))
		}
		rows.Close()
		if !strings.Contains(plan.String(), tc.want) {
			t.Errorf("%s: plan does not use %s\n  plan: %s", tc.name, tc.want, plan.String())
		}
	}
}
