package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	sqlite3 "github.com/mattn/go-sqlite3"
)

const messageFTSBatchSize = 500

const messageFTSStateSchema = `
	CREATE TABLE IF NOT EXISTS message_fts_state (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		last_rowid INTEGER NOT NULL,
		ready INTEGER NOT NULL
	);
`

const messageFTSTriggers = `
	CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages
	WHEN (SELECT ready = 1 OR new.rowid <= last_rowid FROM message_fts_state WHERE id = 1)
	BEGIN
		INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
	END;
	CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages
	WHEN (SELECT ready = 1 OR old.rowid <= last_rowid FROM message_fts_state WHERE id = 1)
	BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
	END;
	CREATE TRIGGER IF NOT EXISTS messages_fts_update AFTER UPDATE OF content, rowid ON messages
	BEGIN
		INSERT INTO messages_fts(messages_fts, rowid, content)
			SELECT 'delete', old.rowid, old.content
			WHERE (SELECT ready = 1 OR old.rowid <= last_rowid FROM message_fts_state WHERE id = 1);
		INSERT INTO messages_fts(rowid, content)
			SELECT new.rowid, new.content
			WHERE (SELECT ready = 1 OR new.rowid <= last_rowid FROM message_fts_state WHERE id = 1);
	END;
`

func initMessageFTS(db *sql.DB) (bool, error) {
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()
	// Lock before reading the schema: upgrading a read transaction can bypass busy_timeout.
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return false, err
	}
	defer conn.ExecContext(ctx, `ROLLBACK`)
	if _, err := conn.ExecContext(ctx, messageFTSStateSchema); err != nil {
		return false, err
	}
	// Probe even when messages_fts already exists: an untagged binary may reopen this database.
	_, err = conn.ExecContext(ctx, `CREATE VIRTUAL TABLE temp.message_fts_probe USING fts5(content)`)
	if err != nil {
		if !strings.Contains(err.Error(), "no such module: fts5") {
			return false, err
		}
		if _, err := conn.ExecContext(ctx, `DROP TRIGGER IF EXISTS messages_fts_insert;
			DROP TRIGGER IF EXISTS messages_fts_update;
			DROP TRIGGER IF EXISTS messages_fts_delete;
			INSERT INTO message_fts_state VALUES (1, 0, -1)
			ON CONFLICT(id) DO UPDATE SET last_rowid = 0, ready = -1;`); err != nil {
			return false, err
		}
		_, err = conn.ExecContext(ctx, `COMMIT`)
		return false, err
	}
	if _, err := conn.ExecContext(ctx, `DROP TABLE temp.message_fts_probe;
		CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
			content, content='messages', content_rowid='rowid', tokenize='unicode61 remove_diacritics 2'
		);
		INSERT OR IGNORE INTO message_fts_state VALUES (1, 0, -1);`); err != nil {
		return false, err
	}
	if _, err := conn.ExecContext(ctx, messageFTSTriggers); err != nil {
		return false, err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return true, err
}

func messageFTSReady(db *sql.DB) (bool, error) {
	var ready int
	err := db.QueryRow(`SELECT ready FROM message_fts_state WHERE id = 1`).Scan(&ready)
	if errors.Is(err, sql.ErrNoRows) || (err != nil && strings.Contains(err.Error(), "no such table: message_fts_state")) {
		return false, nil
	}
	return ready == 1, err
}

func backfillMessageFTS(ctx context.Context, db *sql.DB) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	// Take the writer lock before reading the watermark; triggers and this batch advance atomically.
	if _, err := tx.ExecContext(ctx, `UPDATE message_fts_state SET last_rowid = last_rowid WHERE id = 1`); err != nil {
		return false, err
	}
	var last int64
	var ready int
	if err := tx.QueryRowContext(ctx, `SELECT last_rowid, ready FROM message_fts_state WHERE id = 1`).Scan(&last, &ready); err != nil {
		return false, err
	}
	if ready == 1 {
		return true, tx.Commit()
	}
	if ready == -1 {
		// An untagged binary may have changed messages while the index triggers were disabled.
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages_fts(messages_fts) VALUES ('delete-all');
			UPDATE message_fts_state SET last_rowid = 0, ready = NOT EXISTS (SELECT 1 FROM messages) WHERE id = 1;`); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	var end sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(rowid) FROM (
		SELECT rowid FROM messages WHERE rowid > ? ORDER BY rowid LIMIT ?
	)`, last, messageFTSBatchSize).Scan(&end); err != nil {
		return false, err
	}
	if end.Valid {
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages_fts(rowid, content)
			SELECT rowid, content FROM messages WHERE rowid > ? AND rowid <= ?`, last, end.Int64); err != nil {
			return false, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE message_fts_state SET last_rowid = ? WHERE id = 1`, end.Int64); err != nil {
			return false, err
		}
	} else {
		if _, err := tx.ExecContext(ctx, `UPDATE message_fts_state SET ready = 1 WHERE id = 1`); err != nil {
			return false, err
		}
	}
	return !end.Valid, tx.Commit()
}

func (store *MessageStore) startMessageFTSBackfill() {
	ctx, cancel := context.WithCancel(context.Background())
	store.ftsCancel = cancel
	store.ftsDone = make(chan struct{})
	go func() {
		defer close(store.ftsDone)
		for {
			done, err := backfillMessageFTS(ctx, store.db)
			if ctx.Err() != nil {
				return
			}
			if err != nil {
				var sqliteErr sqlite3.Error
				if !errors.As(err, &sqliteErr) || (sqliteErr.Code != sqlite3.ErrBusy && sqliteErr.Code != sqlite3.ErrLocked) {
					fmt.Printf("Message FTS backfill failed: %v; using LIKE until next restart\n", err)
					return
				}
			} else if done {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(25 * time.Millisecond):
			}
		}
	}()
}

// Every word is a literal prefix, so a stem still finds its inflections as LIKE did.
func literalFTSQuery(query string) string {
	var terms []string
	for _, word := range strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) && !unicode.Is(unicode.Co, r)
	}) {
		terms = append(terms, `"`+word+`"*`)
	}
	return strings.Join(terms, " ")
}
