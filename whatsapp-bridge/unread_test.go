package main

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func unreadLive(t *testing.T, s *MessageStore, id, chat string, fromMe bool, stamp int64) {
	t.Helper()
	if err := s.StoreSender(chat, "Tester", "", "", ""); err != nil {
		t.Fatal(err)
	}
	jid, _ := types.ParseJID(chat)
	if !handleMessage(&whatsmeow.Client{}, s, &events.Message{
		Info:    types.MessageInfo{MessageSource: types.MessageSource{Chat: jid, Sender: types.NewJID("111", types.DefaultUserServer), IsFromMe: fromMe}, ID: id, Timestamp: time.Unix(stamp, 0)},
		Message: &waProto.Message{Conversation: proto.String("hello")},
	}, waLog.Noop) {
		t.Fatal("live message failed")
	}
}

func TestUnreadLiveWire(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	unreadLive(t, s, "incoming", chat, false, 100)
	unreadLive(t, s, "own", chat, true, 101)
	for _, include := range []bool{false, true} {
		got, err := getChat(s.db, ChatRequest{ChatJID: chat, IncludeLastMessage: &include})
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(got.Chat)
		var wire map[string]any
		_ = json.Unmarshal(data, &wire)
		if wire["unread_count"] != float64(1) {
			t.Fatalf("unread_count = %v, want 1", wire["unread_count"])
		}
	}
	got, err := listChats(s.db, ChatsRequest{UnreadOnly: true})
	if err != nil || len(got.Chats) != 1 || got.Chats[0].UnreadCount != 1 {
		t.Fatalf("unread chats = %+v, %v", got.Chats, err)
	}
}

func assertUnread(t *testing.T, s *MessageStore, chat string, count int, badge bool) {
	t.Helper()
	got, err := getChat(s.db, ChatRequest{ChatJID: chat})
	if err != nil {
		t.Fatal(err)
	}
	if got.Chat == nil || got.Chat.UnreadCount != count || got.Chat.MarkedAsUnread != badge {
		t.Fatalf("chat = %+v, want unread_count=%d marked_as_unread=%v", got.Chat, count, badge)
	}
}

func TestUnreadReceiptOwnership(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind types.ReceiptType
		own  bool
		want int
	}{
		{"read-self", types.ReceiptTypeReadSelf, true, 1},
		{"read", types.ReceiptTypeRead, true, 1},
		{"other read", types.ReceiptTypeRead, false, 3},
		{"other read-self", types.ReceiptTypeReadSelf, false, 3},
		{"delivery", types.ReceiptTypeDelivered, true, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupChatStore(t)
			chat := "111@s.whatsapp.net"
			for i, id := range []string{"a", "b", "c"} {
				unreadLive(t, s, id, chat, false, int64(100+i))
			}
			unreadLive(t, s, "a", "222@s.whatsapp.net", false, 100)
			err := handleReadReceipt(nil, s.db, &events.Receipt{
				MessageSource: types.MessageSource{Chat: types.NewJID("111", types.DefaultUserServer), IsFromMe: tc.own},
				Type:          tc.kind, MessageIDs: []types.MessageID{"b", "a"}, Timestamp: time.Unix(200, 0),
			})
			if err != nil {
				t.Fatal(err)
			}
			assertUnread(t, s, chat, tc.want, false)
			assertUnread(t, s, "222@s.whatsapp.net", 1, false)
		})
	}
}

func TestUnreadReadReceiptResolvesLID(t *testing.T) {
	s := setupChatStore(t)
	unreadLive(t, s, "a", "222@s.whatsapp.net", false, 100)
	client := &whatsmeow.Client{Store: &store.Device{LIDs: historyLIDStore{}}}
	if err := handleReadReceipt(client, s.db, &events.Receipt{
		MessageSource: types.MessageSource{Chat: types.NewJID("444", types.HiddenUserServer), IsFromMe: true},
		Type:          types.ReceiptTypeReadSelf, MessageIDs: []types.MessageID{"a"},
	}); err != nil {
		t.Fatal(err)
	}
	assertUnread(t, s, "222@s.whatsapp.net", 0, false)
	unreadLive(t, s, "a", "222@s.whatsapp.net", false, 100)
	assertUnread(t, s, "222@s.whatsapp.net", 0, false)
}

type unreadClient struct {
	markErr, stateErr error
	ids               []types.MessageID
	patch             *appstate.PatchInfo
}

func (c *unreadClient) MarkRead(_ context.Context, ids []types.MessageID, _ time.Time, _, _ types.JID, _ ...types.ReceiptType) error {
	c.ids = ids
	return c.markErr
}
func (c *unreadClient) SendAppState(_ context.Context, patch appstate.PatchInfo) error {
	c.patch = &patch
	return c.stateErr
}

func TestUnreadOwnMarkRead(t *testing.T) {
	for _, tc := range []struct {
		name              string
		markErr, stateErr error
		ids               []string
		want              int
		badge             bool
	}{
		{"success", nil, nil, []string{"a"}, 1, false},
		{"whole chat", nil, nil, nil, 1, false},
		{"receipt failed", fmt.Errorf("receipt failed"), nil, []string{"a"}, 3, true},
		{"patch failed after receipt", nil, fmt.Errorf("patch failed"), []string{"a"}, 2, true},
		{"patch failed without receipt", nil, fmt.Errorf("patch failed"), nil, 3, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupChatStore(t)
			chat := types.NewJID("111", types.DefaultUserServer)
			for i, id := range []string{"a", "b", "newer"} {
				unreadLive(t, s, id, chat.String(), false, int64(100+i))
			}
			if err := setChatReadState(s.db, chat.String(), false, time.Time{}); err != nil {
				t.Fatal(err)
			}
			client := &unreadClient{markErr: tc.markErr, stateErr: tc.stateErr}
			err := markChatRead(client, s.db, chat, types.EmptyJID, tc.ids, time.Unix(101, 0), chat.String())
			if (err != nil) != (tc.markErr != nil || tc.stateErr != nil) {
				t.Fatalf("markChatRead = %v", err)
			}
			if tc.markErr != nil && client.patch != nil {
				t.Fatal("sent app-state after failed receipt")
			}
			if client.patch != nil {
				action := client.patch.Mutations[0].Value.GetMarkChatAsReadAction()
				if !action.GetRead() {
					t.Fatal("sent unread patch")
				}
			}
			assertUnread(t, s, chat.String(), tc.want, tc.badge)
		})
	}
}

func TestUnreadAppStateAndFilter(t *testing.T) {
	s := setupChatStore(t)
	chat := types.NewJID("111", types.DefaultUserServer)
	unreadLive(t, s, "old", chat.String(), false, 100)
	unreadLive(t, s, "new", chat.String(), false, 102)
	unreadLive(t, s, "own", "222@s.whatsapp.net", true, 100)
	evt := &events.MarkChatAsRead{JID: chat, Timestamp: time.Unix(200, 0), Action: &waSyncAction.MarkChatAsReadAction{
		Read: proto.Bool(true), MessageRange: &waSyncAction.SyncActionMessageRange{LastMessageTimestamp: proto.Int64(100)},
	}}
	if err := handleChatReadState(nil, s.db, evt); err != nil {
		t.Fatal(err)
	}
	assertUnread(t, s, chat.String(), 1, false)
	evt.Action.MessageRange.LastMessageTimestamp = proto.Int64(102)
	if err := handleChatReadState(nil, s.db, evt); err != nil {
		t.Fatal(err)
	}
	evt.Action.Read = proto.Bool(false)
	if err := handleChatReadState(nil, s.db, evt); err != nil {
		t.Fatal(err)
	}
	assertUnread(t, s, chat.String(), 0, true)
	for _, include := range []bool{true, false} {
		got, err := listChats(s.db, ChatsRequest{UnreadOnly: true, IncludeLastMessage: &include, Limit: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Chats) != 1 || got.Chats[0].JID != chat.String() || got.Chats[0].UnreadCount != 0 || !got.Chats[0].MarkedAsUnread {
			t.Fatalf("filtered chats = %+v", got.Chats)
		}
		page, err := listChats(s.db, ChatsRequest{UnreadOnly: true, IncludeLastMessage: &include, Limit: 1, Page: 1})
		if err != nil || len(page.Chats) != 0 {
			t.Fatalf("second page = %+v, %v", page, err)
		}
	}
}

func TestUnreadHistory(t *testing.T) {
	for _, tc := range []struct {
		name  string
		count *uint32
		want  int
		older bool
	}{
		{"no count", nil, 0, false},
		{"zero", proto.Uint32(0), 0, false},
		{"two", proto.Uint32(2), 2, false},
		{"older chunk", proto.Uint32(2), 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupChatStore(t)
			chat := "123-456@g.us"
			old, fresh, read, own, status := historyMessage("old", 100), historyMessage("fresh", 103), historyMessage("read", 104), historyMessage("own", 105), historyMessage("status", 101)
			read.Message.Status = waProto.WebMessageInfo_READ.Enum()
			own.Message.Key.FromMe = proto.Bool(true)
			status.Message.Status = waProto.WebMessageInfo_DELIVERY_ACK.Enum()
			conv := &waProto.Conversation{ID: proto.String(chat), Name: proto.String("Test"), UnreadCount: tc.count,
				MarkedAsUnread: proto.Bool(true), Messages: []*waProto.HistorySyncMsg{nil, old, own, fresh, read, status}}
			if tc.older {
				conv.LastMsgTimestamp = proto.Uint64(1000)
			}
			sync := &events.HistorySync{Data: &waProto.HistorySync{Conversations: []*waProto.Conversation{conv}}}
			handleHistorySync(nil, s, sync, waLog.Noop)
			assertUnread(t, s, chat, tc.want, true)
			var ownRead, explicitRead bool
			if err := s.db.QueryRow(`SELECT is_read FROM messages WHERE id = 'own'`).Scan(&ownRead); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT is_read FROM messages WHERE id = 'read'`).Scan(&explicitRead); err != nil {
				t.Fatal(err)
			}
			if !ownRead || !explicitRead {
				t.Fatal("own or READ history was unread")
			}
			if err := setChatReadState(s.db, chat, true, time.Unix(500, 0)); err != nil {
				t.Fatal(err)
			}
			handleHistorySync(nil, s, sync, waLog.Noop)
			assertUnread(t, s, chat, 0, false)
		})
	}
}

func TestUnreadUnknownHistoryPreservesLiveState(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	unreadLive(t, s, "live", chat, false, 100)
	handleHistorySync(nil, s, &events.HistorySync{Data: &waProto.HistorySync{Conversations: []*waProto.Conversation{{
		ID: proto.String(chat), Messages: []*waProto.HistorySyncMsg{historyMessage("live", 100)},
	}}}}, waLog.Noop)
	assertUnread(t, s, chat, 1, false)
}

func TestUnreadMigration(t *testing.T) {
	s := setupChatStore(t)
	unreadLive(t, s, "legacy", "111@s.whatsapp.net", false, 100)
	mustExec(t, s, `DROP INDEX idx_messages_unread`)
	mustExec(t, s, `ALTER TABLE messages DROP COLUMN is_read`)
	mustExec(t, s, `ALTER TABLE chats DROP COLUMN marked_as_unread`)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	assertUnread(t, migrated, "111@s.whatsapp.net", 0, false)
	if err := migrateReadState(migrated.db); err != nil {
		t.Fatal(err)
	}
	unreadLive(t, migrated, "fresh", "111@s.whatsapp.net", false, 101)
	assertUnread(t, migrated, "111@s.whatsapp.net", 1, false)
}

func TestUnreadLIDMergeAndDelete(t *testing.T) {
	s := setupChatStore(t)
	lid, pn := "444@lid", "111@s.whatsapp.net"
	for _, chat := range []string{lid, pn} {
		unreadLive(t, s, "duplicate", chat, false, 100)
	}
	unreadLive(t, s, "lid-only", lid, false, 101)
	unreadLive(t, s, "pn-only", pn, false, 102)
	unreadLive(t, s, "pending", pn, false, 103)
	if err := markMessagesRead(s.db, lid, []string{"pending", "pending-late"}); err != nil {
		t.Fatal(err)
	}
	if err := markMessagesRead(s.db, lid, []string{"duplicate"}); err != nil {
		t.Fatal(err)
	}
	if err := setChatReadState(s.db, lid, false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := mergeLIDChat(s.db, lid, pn); err != nil {
		t.Fatal(err)
	}
	assertUnread(t, s, pn, 2, true)
	unreadLive(t, s, "pending-late", pn, false, 104)
	assertUnread(t, s, pn, 2, true)
	if chatExists(s.db, lid) {
		t.Fatal("LID chat survived merge")
	}
	if err := forgetChatLocally(s.db, pn); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`SELECT COUNT(*) FROM chats`, `SELECT COUNT(*) FROM messages`, `SELECT COUNT(*) FROM read_receipts`} {
		var count int
		if err := s.db.QueryRow(q).Scan(&count); err != nil || count != 0 {
			t.Fatalf("after delete: count=%d, err=%v", count, err)
		}
	}
}

func TestUnreadReceiptBeforeHistory(t *testing.T) {
	s := setupChatStore(t)
	chat := "123-456@g.us"
	if err := handleReadReceipt(nil, s.db, &events.Receipt{
		MessageSource: types.MessageSource{Chat: types.NewJID("123-456", types.GroupServer), IsFromMe: true},
		Type:          types.ReceiptTypeReadSelf, MessageIDs: []string{"late"},
	}); err != nil {
		t.Fatal(err)
	}
	syncHistory(s, nil, &waProto.HistorySyncMsg{Message: &waProto.WebMessageInfo{
		Key:              &waProto.MessageKey{ID: proto.String("late"), Participant: proto.String("111@s.whatsapp.net")},
		MessageTimestamp: proto.Uint64(100), Status: waProto.WebMessageInfo_DELIVERY_ACK.Enum(),
		Message: &waProto.Message{Conversation: proto.String("delayed history")},
	}})
	assertUnread(t, s, chat, 0, false)
}

// Receipts for stored messages leave no row behind, and status views create no chat.
func TestUnreadReceiptsLeaveNoResidue(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	unreadLive(t, s, "seen", chat, false, 100)
	for _, evt := range []*events.Receipt{
		{MessageSource: types.MessageSource{Chat: types.NewJID("111", types.DefaultUserServer), IsFromMe: true}, Type: types.ReceiptTypeRead, MessageIDs: []string{"seen"}},
		{MessageSource: types.MessageSource{Chat: types.StatusBroadcastJID, IsFromMe: true}, Type: types.ReceiptTypeReadSelf, MessageIDs: []string{"status"}},
	} {
		if err := handleReadReceipt(nil, s.db, evt); err != nil {
			t.Fatal(err)
		}
	}
	assertUnread(t, s, chat, 0, false)
	var receipts, chats int
	if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM read_receipts), (SELECT COUNT(*) FROM chats WHERE jid != ?)`, chat).Scan(&receipts, &chats); err != nil {
		t.Fatal(err)
	}
	if receipts != 0 || chats != 0 {
		t.Fatalf("read_receipts=%d other chats=%d", receipts, chats)
	}
}

// Reading a whole chat covers messages delivered later with an older timestamp (delayed
// delivery, history with a stale unread count).
func TestUnreadChatReadThroughCoversLateMessages(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	unreadLive(t, s, "first", chat, false, 100)
	if err := setChatReadState(s.db, chat, true, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	unreadLive(t, s, "late", chat, false, 150)
	assertUnread(t, s, chat, 0, false)
	handleHistorySync(nil, s, &events.HistorySync{Data: &waProto.HistorySync{Conversations: []*waProto.Conversation{{
		ID: proto.String(chat), UnreadCount: proto.Uint32(2), Messages: []*waProto.HistorySyncMsg{historyMessage("old", 120)},
	}}}}, waLog.Noop)
	assertUnread(t, s, chat, 0, false)
	unreadLive(t, s, "new", chat, false, 300)
	assertUnread(t, s, chat, 1, false)
}

// A receipt for a LID chat with no message yet moves to the number chat once the mapping is
// known, and the message stored there later is read.
func TestUnreadLIDReceiptMigrates(t *testing.T) {
	s := setupChatStore(t)
	if err := handleReadReceipt(nil, s.db, &events.Receipt{
		MessageSource: types.MessageSource{Chat: types.NewJID("999", types.HiddenUserServer), IsFromMe: true},
		Type:          types.ReceiptTypeRead, MessageIDs: []string{"m"},
	}); err != nil {
		t.Fatal(err)
	}
	migrateLIDChats(lidClient(map[string]string{"999": "222"}), s, waLog.Noop)
	unreadLive(t, s, "m", "222@s.whatsapp.net", false, 100)
	assertUnread(t, s, "222@s.whatsapp.net", 0, false)
}

func TestUnreadReadBeforeFirstMessage(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	if err := setChatReadState(s.db, chat, true, time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	unreadLive(t, s, "late", chat, false, 150)
	assertUnread(t, s, chat, 0, false)
}

func TestUnreadLIDMergeAppliesReadThrough(t *testing.T) {
	for _, readLID := range []bool{true, false} {
		s := setupChatStore(t)
		lid, pn := "999@lid", "222@s.whatsapp.net"
		read, unread := pn, lid
		if readLID {
			read, unread = lid, pn
		}
		unreadLive(t, s, "m", unread, false, 150)
		if err := setChatReadState(s.db, read, true, time.Unix(200, 0)); err != nil {
			t.Fatal(err)
		}
		if err := mergeLIDChat(s.db, lid, pn); err != nil {
			t.Fatal(err)
		}
		assertUnread(t, s, pn, 0, false)
	}
}
