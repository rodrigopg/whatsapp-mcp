package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

func TestFTSInitWaitsForConcurrentWriter(t *testing.T) {
	for _, journal := range []string{"DELETE", "WAL"} {
		t.Run(journal, func(t *testing.T) {
			dsn := filepath.Join(t.TempDir(), "messages.db") + "?_busy_timeout=2000"
			writer, err := sql.Open("sqlite3", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Close()
			if _, err := writer.Exec(`PRAGMA journal_mode=` + journal + `;
				CREATE TABLE messages (content TEXT);
				INSERT INTO messages VALUES ('original');` + messageFTSStateSchema); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite3", dsn)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			db.SetMaxOpenConns(1)
			conn, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			begun := make(chan struct{}, 1)
			err = conn.Raw(func(raw any) error {
				raw.(*sqlite3.SQLiteConn).RegisterAuthorizer(func(op int, arg, _, _ string) int {
					if op == sqlite3.SQLITE_TRANSACTION && arg == "BEGIN" {
						begun <- struct{}{}
					}
					return sqlite3.SQLITE_OK
				})
				return nil
			})
			conn.Close()
			if err != nil {
				t.Fatal(err)
			}
			tx, err := writer.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.Exec(`UPDATE messages SET content = 'transcribed'`); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				_, err := initMessageFTS(db)
				result <- err
			}()
			select {
			case <-begun:
			case <-time.After(5 * time.Second):
				t.Fatal("FTS initialization did not begin")
			}
			select {
			case err := <-result:
				t.Fatalf("FTS initialization returned before writer committed: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatalf("FTS initialization after writer commit: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("FTS initialization did not finish")
			}
			var content string
			if err := db.QueryRow(`SELECT content FROM messages`).Scan(&content); err != nil || content != "transcribed" {
				t.Fatalf("concurrent transcript = %q, %v", content, err)
			}
		})
	}
}
