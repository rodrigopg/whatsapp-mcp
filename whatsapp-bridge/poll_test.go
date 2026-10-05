package main

import (
	"crypto/sha256"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestValidatePoll(t *testing.T) {
	opts := func(n int) []string {
		o := make([]string, n)
		for i := range o {
			o[i] = string(rune('a' + i))
		}
		return o
	}
	cases := []struct {
		name     string
		question string
		options  []string
		sel      int
		want     int
		wantErr  bool
	}{
		{"default selectable", "q?", opts(2), 0, 1, false},
		{"max options", "q?", opts(12), 12, 12, false},
		{"one option", "q?", opts(1), 1, 0, true},
		{"too many options", "q?", opts(13), 1, 0, true},
		{"empty question", "  ", opts(2), 1, 0, true},
		{"long question", strings.Repeat("x", 256), opts(2), 1, 0, true},
		{"long option", "q?", []string{"a", strings.Repeat("x", 101)}, 1, 0, true},
		{"empty option", "q?", []string{"a", " "}, 1, 0, true},
		{"duplicate option", "q?", []string{"a", "a "}, 1, 0, true},
		{"selectable too big", "q?", opts(3), 4, 0, true},
		{"selectable negative", "q?", opts(3), -1, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, _, err := validatePoll(c.question, c.options, c.sel)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got (%d, %v), want (%d, err=%v)", got, err, c.want, c.wantErr)
			}
		})
	}
}

func TestOptionsForHashes(t *testing.T) {
	h := func(s string) []byte { x := sha256.Sum256([]byte(s)); return x[:] }
	got := optionsForHashes([]string{"yes", "no", "maybe"}, [][]byte{h("maybe"), h("yes"), h("bogus")})
	if want := []string{"maybe", "yes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestPollStoreAndVotes(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(pollSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := getPollVotes(db, "c@s.whatsapp.net", "P1"); !errors.Is(err, errPollNotFound) {
		t.Fatalf("want errPollNotFound, got %v", err)
	}
	if err := storePoll(db, "P1", "c@s.whatsapp.net", "lunch?", []string{"pizza", "sushi"}, 1); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, v := range []struct{ voter, sel string }{{"u1", `["pizza"]`}, {"u2", `["pizza"]`}, {"u1", `["sushi"]`}} {
		if _, err := db.Exec(`INSERT OR REPLACE INTO poll_votes VALUES ('P1','c@s.whatsapp.net',?,?,?)`, v.voter, v.sel, now); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := getPollVotes(db, "c@s.whatsapp.net", "P1")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Tally["pizza"] != 1 || resp.Tally["sushi"] != 1 || len(resp.Votes) != 2 {
		t.Fatalf("latest vote per voter should win: %+v", resp)
	}
}
