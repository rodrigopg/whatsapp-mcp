package main

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

func migrateReadState(db *sql.DB) error {
	for _, col := range []struct {
		table, name, def string
	}{
		{"messages", "is_read", "BOOLEAN NOT NULL DEFAULT 1"},
		{"chats", "marked_as_unread", "BOOLEAN NOT NULL DEFAULT 0"},
		// Read through this time: a message stored later with an older timestamp is already read.
		{"chats", "read_through", "TIMESTAMP"},
	} {
		rows, err := db.Query("PRAGMA table_info(" + col.table + ")")
		if err != nil {
			return err
		}
		found := false
		for rows.Next() {
			var cid, notnull, pk int
			var name, kind string
			var def any
			if err := rows.Scan(&cid, &name, &kind, &notnull, &def, &pk); err != nil {
				rows.Close()
				return err
			}
			found = found || name == col.name
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !found {
			if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", col.table, col.name, col.def)); err != nil {
				return err
			}
		}
	}
	_, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_unread ON messages(chat_jid) WHERE is_read = 0 AND is_from_me = 0;
		CREATE TABLE IF NOT EXISTS read_receipts (
			chat_jid TEXT,
			message_id TEXT,
			PRIMARY KEY (chat_jid, message_id)
		);`)
	return err
}

func markMessagesRead(db *sql.DB, chat string, ids []types.MessageID) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		res, err := tx.Exec(`UPDATE messages SET is_read = 1 WHERE chat_jid = ? AND id = ? AND is_from_me = 0`, chat, id)
		if err != nil {
			return err
		}
		// A receipt can precede its message (history arrives later); keep it until then.
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n == 0 {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO read_receipts (chat_jid, message_id) VALUES (?, ?)`, chat, id); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func handleReadReceipt(client *whatsmeow.Client, db *sql.DB, evt *events.Receipt) error {
	if !evt.IsFromMe || evt.Chat.Server == types.BroadcastServer || (evt.Type != types.ReceiptTypeRead && evt.Type != types.ReceiptTypeReadSelf) {
		return nil
	}
	return markMessagesRead(db, resolveToPN(client, evt.Chat).String(), evt.MessageIDs)
}

func setChatReadState(db *sql.DB, chat string, read bool, through time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// The read may arrive before the chat's first message; its boundary must outlive that.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO chats (jid, name) VALUES (?, '')`, chat); err != nil {
		return err
	}
	if read {
		if _, err := tx.Exec(`UPDATE messages SET is_read = 1 WHERE chat_jid = ? AND is_from_me = 0 AND julianday(timestamp) <= julianday(?)`, chat, through); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE chats SET read_through = ? WHERE jid = ? AND (read_through IS NULL OR julianday(read_through) < julianday(?))`, through, chat, through); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`UPDATE chats SET marked_as_unread = ? WHERE jid = ?`, !read, chat); err != nil {
		return err
	}
	return tx.Commit()
}

func handleChatReadState(client *whatsmeow.Client, db *sql.DB, evt *events.MarkChatAsRead) error {
	if evt.Action == nil || evt.Action.Read == nil {
		return nil
	}
	through := evt.Timestamp
	if stamp := evt.Action.GetMessageRange().GetLastMessageTimestamp(); stamp > 0 {
		through = time.Unix(stamp, 0)
	}
	return setChatReadState(db, resolveToPN(client, evt.JID).String(), evt.Action.GetRead(), through)
}

type appStateSender interface {
	SendAppState(context.Context, appstate.PatchInfo) error
}

type readStateClient interface {
	appStateSender
	MarkRead(context.Context, []types.MessageID, time.Time, types.JID, types.JID, ...types.ReceiptType) error
}

func markChatRead(client readStateClient, db *sql.DB, chat, sender types.JID, ids []types.MessageID, at time.Time, localChat string) error {
	ctx := context.Background()
	if len(ids) > 0 {
		if err := client.MarkRead(ctx, ids, at, chat, sender); err != nil {
			return err
		}
		// The receipt already succeeded even if the following app-state patch fails.
		if err := markMessagesRead(db, localChat, ids); err != nil {
			return err
		}
	}
	if err := safeSendAppState(client, ctx, appstate.BuildMarkChatAsRead(chat, true, at, nil)); err != nil {
		return err
	}
	return setChatReadState(db, localChat, true, at)
}

type messageReadState int

const (
	messageReadUnknown messageReadState = iota
	messageUnread
	messageRead
)

// Older/on-demand chunks must not turn their last N ancient messages unread.
func historyReadStates(conv *waProto.Conversation) map[string]messageReadState {
	states := map[string]messageReadState{}
	candidates := []*waProto.WebMessageInfo{}
	var newest uint64
	for _, item := range conv.Messages {
		m := item.GetMessage()
		if m == nil {
			continue
		}
		if m.GetMessageTimestamp() > newest {
			newest = m.GetMessageTimestamp()
		}
		id := m.GetKey().GetID()
		if m.GetKey().GetFromMe() || (m.Status != nil && m.GetStatus() >= waProto.WebMessageInfo_READ) {
			states[id] = messageRead
			continue
		}
		if id == "" || m.GetMessageTimestamp() == 0 {
			continue
		}
		candidates = append(candidates, m)
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].GetMessageTimestamp() > candidates[j].GetMessageTimestamp() })
	// WhatsApp omits a zero unread count, so a missing one means all read (as mautrix-whatsapp
	// reads it). Inferred read is "unknown": a new row is stored read, a live unread row stays.
	latest := conv.LastMsgTimestamp == nil || newest >= conv.GetLastMsgTimestamp()
	for i, m := range candidates {
		state := messageReadUnknown
		if latest && uint32(i) < conv.GetUnreadCount() {
			state = messageUnread
		}
		states[m.GetKey().GetID()] = state
	}
	return states
}
