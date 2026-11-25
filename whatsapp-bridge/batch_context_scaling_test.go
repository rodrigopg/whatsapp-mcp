package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"
)

func TestBatchMessageContextScaling(t *testing.T) {
	for _, size := range []int{10000, 20000} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			s := setupChatStore(t)
			chat := "large@g.us"
			start := ts("2026-10-01T00:00:00Z")
			if err := s.StoreChat(chat, "Large", start); err != nil {
				t.Fatal(err)
			}
			tx, err := s.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			stmt, err := tx.Prepare(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me)
				VALUES (?, ?, 'sender', ?, ?, false)`)
			if err != nil {
				t.Fatal(err)
			}
			defer stmt.Close()
			const matches = 400
			spacing := size / matches
			for i := 0; i < size; i++ {
				content := "ordinary"
				if i%spacing == spacing/2 {
					content = "needle"
				}
				if _, err := stmt.Exec(fmt.Sprint(i), chat, content, start.Add(time.Duration(i)*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			db, err := openUnaccentMessagesDB()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			filtered := []string{}
			for i := size - spacing + spacing/2; i > 0; i -= spacing {
				filtered = append(filtered, fmt.Sprint(i-1), fmt.Sprint(i), fmt.Sprint(i+1))
			}
			page := []string{fmt.Sprint(size - 22), fmt.Sprint(size - 21), fmt.Sprint(size - 20)}
			for i := size - 23; i >= size-41; i-- {
				page = append(page, fmt.Sprint(i))
			}
			for _, tc := range []struct {
				name string
				req  MessagesRequest
				want []string
			}{
				{"all chats", MessagesRequest{Limit: 20, Page: 1}, page},
				{"filtered chat", MessagesRequest{ChatJID: &chat, Query: strp("needle"), Limit: matches}, filtered},
			} {
				t.Run(tc.name, func(t *testing.T) {
					tc.req.IncludeContext, tc.req.ContextBefore, tc.req.ContextAfter = true, 1, 1
					began := time.Now()
					got, err := listMessages(db, tc.req)
					elapsed := time.Since(began)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(readIDs(got.Messages), tc.want) {
						t.Fatalf("context IDs = %v, want %v", readIDs(got.Messages), tc.want)
					}
					t.Logf("%d messages, %d matches, context ±1: %s", size, tc.req.Limit, elapsed)
					if elapsed >= 250*time.Millisecond {
						t.Fatalf("context expansion took %s, want < 250ms", elapsed)
					}
				})
			}
		})
	}
}
