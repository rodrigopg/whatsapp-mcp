package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func TestMessageTypesLiveAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name, want, media string
		body              *waProto.Message
	}{
		{"sticker", "[sticker]", "sticker", &waProto.Message{StickerMessage: &waProto.StickerMessage{
			URL: proto.String("https://example.invalid/sticker"), MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, FileLength: proto.Uint64(42),
		}}},
		{"direct-path sticker", "[sticker]", "sticker", &waProto.Message{StickerMessage: &waProto.StickerMessage{
			DirectPath: proto.String("/sticker"), MediaKey: []byte{1}, FileSHA256: []byte{2}, FileEncSHA256: []byte{3}, FileLength: proto.Uint64(42),
		}}},
		{"location", "[location] 55.75,37.62 Park Main street", "", &waProto.Message{LocationMessage: &waProto.LocationMessage{
			DegreesLatitude: proto.Float64(55.75), DegreesLongitude: proto.Float64(37.62), Name: proto.String("Park"), Address: proto.String("Main street"),
		}}},
		{"live location", "[live location] 0,-1.25 Walking", "", &waProto.Message{LiveLocationMessage: &waProto.LiveLocationMessage{
			DegreesLatitude: proto.Float64(0), DegreesLongitude: proto.Float64(-1.25), Caption: proto.String("Walking"),
		}}},
		{"legacy live location", "[live location] 1,2", "", &waProto.Message{LocationMessage: &waProto.LocationMessage{
			DegreesLatitude: proto.Float64(1), DegreesLongitude: proto.Float64(2), IsLive: proto.Bool(true),
		}}},
		{"contact", "[contact] Alice +12025550123", "", &waProto.Message{ContactMessage: &waProto.ContactMessage{
			DisplayName: proto.String("Alice"), Vcard: proto.String("BEGIN:VCARD\r\nVERSION:3.0\r\nFN:Alice\r\nTEL;type=CELL;waid=12025550123:+12025550123\r\nEND:VCARD"),
		}}},
		{"vcard name", "[contact] Doe, Jane +12025550123", "", &waProto.Message{ContactMessage: &waProto.ContactMessage{
			Vcard: proto.String("BEGIN:VCARD\nFN:Doe\\, Jane\nitem1.TEL:+12025550123\nEND:VCARD"),
		}}},
		{"contacts array", "[contact] Alice +12025550123; [contact] Bob", "", &waProto.Message{ContactsArrayMessage: &waProto.ContactsArrayMessage{
			Contacts: []*waProto.ContactMessage{{DisplayName: proto.String("Alice"), Vcard: proto.String("TEL:+12025550123")}, {DisplayName: proto.String("Bob")}},
		}}},
		{"folded vcard", "[contact] Alice +12025550123, +12025550124", "", &waProto.Message{ContactMessage: &waProto.ContactMessage{
			Vcard: proto.String("BEGIN:VCARD\r\nFN:Ali\r\n ce\r\nTEL;VALUE=uri:tel:+12025550123\r\nTEL:+12025550124\r\nEND:VCARD"),
		}}},
		{"empty contacts array", "[contacts]", "", &waProto.Message{ContactsArrayMessage: &waProto.ContactsArrayMessage{}}},
		{"empty contact", "[contact]", "", &waProto.Message{ContactMessage: &waProto.ContactMessage{}}},
	} {
		for _, history := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{false: "/live", true: "/history"}[history], func(t *testing.T) {
				s := setupChatStore(t)
				if err := s.StoreChat("123-456@g.us", "Test group", time.Time{}); err != nil {
					t.Fatal(err)
				}
				if history {
					m := historyMessage("typed", 100)
					m.Message.Message = &waProto.Message{EphemeralMessage: &waProto.FutureProofMessage{Message: tc.body}}
					syncHistory(s, nil, m)
				} else if !handleMessage(&whatsmeow.Client{}, s, typeEvent("typed", "111", 100, tc.body), waLog.Noop) {
					t.Fatal("live message was not acknowledged")
				}
				got, err := listMessages(s.db, MessagesRequest{})
				if err != nil || len(got.Messages) != 1 {
					t.Fatalf("messages = %+v, %v", got, err)
				}
				m := got.Messages[0]
				if m.Content != tc.want || m.MediaType == nil || *m.MediaType != tc.media {
					t.Fatalf("message = %+v, want content %q media %q", m, tc.want, tc.media)
				}
				ctx, found, err := getMessageContext(s.db, MessageContextRequest{MessageID: "typed"})
				if err != nil || !found || ctx.Message.Content != tc.want {
					t.Fatalf("context = %+v, found %v, %v", ctx, found, err)
				}
				if tc.media == "sticker" {
					media, filename, url, key, sha, enc, length, err := s.GetMediaInfo("typed", m.ChatJID)
					wantURL := "https://example.invalid/sticker"
					if tc.name == "direct-path sticker" {
						wantURL = "https://mmg.whatsapp.net/sticker"
					}
					if err != nil || media != "sticker" || !strings.HasSuffix(filename, ".webp") || url != wantURL || !bytes.Equal(key, []byte{1}) || !bytes.Equal(sha, []byte{2}) || !bytes.Equal(enc, []byte{3}) || length != 42 {
						t.Fatalf("sticker metadata: %q %q %q %v %v %v %d %v", media, filename, url, key, sha, enc, length, err)
					}
					// A nil client reaches the real downloader without making a network request.
					if _, _, _, _, err := downloadMedia(nil, s, "typed", m.ChatJID); !errors.Is(err, whatsmeow.ErrClientIsNil) {
						t.Fatalf("sticker did not reach downloader: %v", err)
					}
				}
			})
		}
	}
}

func typeEvent(id, sender string, at int64, body *waProto.Message) *events.Message {
	return &events.Message{Info: types.MessageInfo{ID: id, Timestamp: time.Unix(at, 0), MessageSource: types.MessageSource{
		Chat: types.NewJID("123-456", types.GroupServer), Sender: types.NewJID(sender, types.DefaultUserServer), IsGroup: true,
	}}, Message: body}
}

func reactionBody(target, emoji string, millis int64) *waProto.Message {
	return &waProto.Message{ReactionMessage: &waProto.ReactionMessage{
		Key: &waProto.MessageKey{ID: proto.String(target)}, Text: proto.String(emoji), SenderTimestampMS: proto.Int64(millis),
	}}
}

func TestReactionsStayWithTargetAndNewestWins(t *testing.T) {
	s := setupChatStore(t)
	client := &whatsmeow.Client{}
	send := func(id, sender string, at int64, body *waProto.Message) {
		t.Helper()
		if !handleMessage(client, s, typeEvent(id, sender, at, body), waLog.Noop) {
			t.Fatal("write failed")
		}
	}
	// Reactions may arrive before the original message and must not create a chat.
	send("r1", "111", 100, reactionBody("target", "👍", 100100))
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&count); err != nil || count != 0 {
		t.Fatalf("reaction-only chat count %d: %v", count, err)
	}
	if err := s.StoreChat("123-456@g.us", "Test group", time.Time{}); err != nil {
		t.Fatal(err)
	}
	send("target", "222", 50, &waProto.Message{Conversation: proto.String("Original")})
	send("r2", "111", 101, reactionBody("target", "❤️", 100200))
	send("old", "111", 102, reactionBody("target", "👍", 100150))
	send("r3", "333", 103, reactionBody("target", "🎉", 0))
	assertReactionJSON(t, s, 2, "❤️", "🎉")
	send("remove", "111", 104, reactionBody("target", "", 104001))
	send("stale", "111", 105, reactionBody("target", "👍", 104000))
	assertReactionJSON(t, s, 1, "🎉")
	assertHistoryChat(t, s, "Test group", time.Unix(50, 0))
	// The removal survives a bridge restart and a stale history delivery.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	m := historyMessage("history-reaction", 200)
	m.Message.Participant = proto.String("111@s.whatsapp.net")
	m.Message.Message = reactionBody("target", "👍", 104000)
	syncHistory(reopened, nil, m)
	assertReactionJSON(t, reopened, 1, "🎉")
}

func assertReactionJSON(t *testing.T, s *MessageStore, count int, emojis ...string) {
	t.Helper()
	list, err := listMessages(s.db, MessagesRequest{})
	if err != nil || len(list.Messages) != 1 || list.Messages[0].ID != "target" {
		t.Fatalf("reaction became a timeline message: %+v, %v", list, err)
	}
	ctx, found, err := getMessageContext(s.db, MessageContextRequest{MessageID: "target"})
	if err != nil || !found {
		t.Fatalf("context: found %v, %v", found, err)
	}
	for _, message := range []APIMessage{list.Messages[0], ctx.Message} {
		data, err := json.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Reactions []struct{ Sender, Emoji, Timestamp string }
		}
		if err := json.Unmarshal(data, &decoded); err != nil || len(decoded.Reactions) != count {
			t.Fatalf("reactions: %s, %v", data, err)
		}
		for i, emoji := range emojis {
			if decoded.Reactions[i].Emoji != emoji || decoded.Reactions[i].Sender == "" || decoded.Reactions[i].Timestamp == "" {
				t.Fatalf("reactions: %s", data)
			}
		}
	}
}

func TestReactionWriteFailureIsNotAcknowledged(t *testing.T) {
	s := setupChatStore(t)
	if _, err := s.db.Exec(`CREATE TRIGGER reject_reaction BEFORE INSERT ON reactions BEGIN SELECT RAISE(FAIL, 'test'); END`); err != nil {
		t.Fatal(err)
	}
	if handleMessage(&whatsmeow.Client{}, s, typeEvent("r", "111", 100, reactionBody("target", "👍", 0)), waLog.Noop) {
		t.Fatal("failed reaction write acknowledged")
	}
}

func TestHistoryEmbeddedReactionsAndContextNeighbors(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("123-456@g.us", "Test group", time.Time{}); err != nil {
		t.Fatal(err)
	}
	own := types.NewJID("999", types.DefaultUserServer)
	client := &whatsmeow.Client{Store: &store.Device{ID: &own}}
	for i, id := range []string{"before", "target", "after"} {
		m := historyMessage(id, uint64(100+i))
		m.Message.Reactions = []*waProto.Reaction{
			{Key: &waProto.MessageKey{Participant: proto.String("111@s.whatsapp.net")}, Text: proto.String("👍"), SenderTimestampMS: proto.Int64(200001)},
			{Key: &waProto.MessageKey{FromMe: proto.Bool(true)}, Text: proto.String("❤️"), SenderTimestampMS: proto.Int64(200002)},
			{Key: &waProto.MessageKey{}, Text: proto.String("unknown")},
		}
		syncHistory(s, client, m)
	}
	ctx, found, err := getMessageContext(s.db, MessageContextRequest{MessageID: "target"})
	if err != nil || !found || len(ctx.Before) != 1 || len(ctx.After) != 1 {
		t.Fatalf("context = %+v, found %v, %v", ctx, found, err)
	}
	for _, m := range []APIMessage{ctx.Before[0], ctx.Message, ctx.After[0]} {
		if len(m.Reactions) != 2 || m.Reactions[0].Sender != "111" || m.Reactions[0].Emoji != "👍" || m.Reactions[1].Sender != "999" || m.Reactions[1].Emoji != "❤️" {
			t.Fatalf("message reactions: %+v", m)
		}
	}
	// Standalone history reactions identify the reactor by the envelope, not the target's key.
	m := historyMessage("own-reaction", 300)
	m.Message.Key.FromMe = proto.Bool(true)
	m.Message.Message = reactionBody("target", "", 300001)
	syncHistory(s, client, m)
	ctx, _, err = getMessageContext(s.db, MessageContextRequest{MessageID: "target"})
	if err != nil || len(ctx.Message.Reactions) != 1 || ctx.Message.Reactions[0].Sender != "111" {
		t.Fatalf("own history removal: %+v, %v", ctx, err)
	}
}

func TestReactionsRemainScopedToChat(t *testing.T) {
	s := setupChatStore(t)
	for _, chat := range []string{"123-456@g.us", "other@g.us"} {
		if err := s.StoreChat(chat, "Test group", time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
		if err := s.StoreMessage("target", chat, "222", "Original", time.Unix(100, 0), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
	}
	if !handleMessage(&whatsmeow.Client{}, s, typeEvent("r", "111", 200, reactionBody("target", "👍", 0)), waLog.Noop) {
		t.Fatal("reaction write failed")
	}
	chat := "other@g.us"
	list, err := listMessages(s.db, MessagesRequest{ChatJID: &chat})
	if err != nil || len(list.Messages) != 1 || len(list.Messages[0].Reactions) != 0 {
		t.Fatalf("reaction leaked into another chat: %+v, %v", list, err)
	}
}

func TestReactionsFollowLIDMigration(t *testing.T) {
	s := setupChatStore(t)
	// There need not be a chat row yet when the first reaction arrives.
	for _, r := range []struct {
		chat, sender, emoji string
		at                  int64
	}{
		{"999@lid", "888@lid", "👍", 100},
		{"222@s.whatsapp.net", "333", "❤️", 200},
		{"999@lid", "333", "", 300},
	} {
		mustExec(t, s, `INSERT INTO reactions VALUES (?, 'target', ?, ?, ?)`, r.chat, r.sender, r.emoji, time.Unix(r.at, 0))
	}
	migrateLIDChats(lidClient(map[string]string{"999": "222", "888": "333"}), s, waLog.Noop)
	var chat, sender, emoji string
	var at time.Time
	if err := s.db.QueryRow(`SELECT chat_jid, sender, emoji, timestamp FROM reactions`).Scan(&chat, &sender, &emoji, &at); err != nil || chat != "222@s.whatsapp.net" || sender != "333" || emoji != "" || !at.Equal(time.Unix(300, 0)) {
		t.Fatalf("LID reaction migration: %q %q %q %v, %v", chat, sender, emoji, at, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("duplicate reactions after LID migration: %d, %v", count, err)
	}
}

func TestForgetChatRemovesReactions(t *testing.T) {
	s := setupChatStore(t)
	mustExec(t, s, `INSERT INTO reactions VALUES ('test@g.us', 'target', '111', '👍', ?)`, time.Unix(100, 0))
	if err := forgetChatLocally(s.db, "test@g.us"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM reactions`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("forgotten chat kept %d reactions: %v", count, err)
	}
}

func TestStatusBroadcastStoreGuards(t *testing.T) {
	s := setupChatStore(t)
	for _, err := range []error{
		s.EnsureChat("status@broadcast", time.Now()),
		s.StoreChat("status@broadcast", "Status", time.Now()),
		s.StoreMessage("status", "status@broadcast", "111", "status", time.Now(), false, "", "", "", nil, nil, nil, 0),
		s.StoreReaction("status@broadcast", "status", "111", "👍", 0, time.Now()),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, table := range []string{"messages", "chats", "reactions"} {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("status stored in %s: %d, %v", table, count, err)
		}
	}
}

func TestStatusBroadcastIgnoredLiveAndHistory(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreSender("status@broadcast", "", "Status", "", ""); err != nil {
		t.Fatal(err)
	}
	event := typeEvent("status", "111", 100, &waProto.Message{Conversation: proto.String("status")})
	event.Info.Chat = types.StatusBroadcastJID
	if !handleMessage(&whatsmeow.Client{}, s, event, waLog.Noop) {
		t.Fatal("status should be acknowledged")
	}
	handleHistorySync(nil, s, &events.HistorySync{Data: &waProto.HistorySync{Conversations: []*waProto.Conversation{
		{ID: proto.String("status@broadcast"), Messages: []*waProto.HistorySyncMsg{historyMessage("status-history", 100)}},
		{ID: proto.String("123-456@g.us"), Messages: []*waProto.HistorySyncMsg{historyMessage("normal", 100)}},
	}}}, waLog.Noop)
	for _, table := range []string{"messages", "chats"} {
		var count int
		if err := s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count = %d, %v", table, count, err)
		}
	}
}

func TestStatusBroadcastStartupCleanup(t *testing.T) {
	s := setupChatStore(t)
	// Seed the old schema's status rows directly; migration must handle existing installs.
	for _, q := range []string{
		`INSERT INTO chats VALUES ('status@broadcast', 'Status', '2026-10-01')`,
		`INSERT INTO chats VALUES ('normal', 'Normal', '2026-10-01')`,
		`INSERT INTO messages (id, chat_jid, content) VALUES ('status', 'status@broadcast', 'old status')`,
		`INSERT INTO messages (id, chat_jid, content) VALUES ('normal', 'normal', 'keep')`,
		`INSERT INTO message_mentions VALUES ('status@broadcast', 'status', '111')`,
		`INSERT INTO polls (id, chat_jid) VALUES ('status', 'status@broadcast')`,
		`INSERT INTO poll_votes (poll_id, chat_jid) VALUES ('status', 'status@broadcast')`,
		`INSERT INTO favorites VALUES ('status@broadcast', 'accepted', 'whatsapp', '2026-10-01')`,
		`INSERT INTO matters (id, chat_jid, owed_by, origin, origin_message_id, created_at, certainty, valid_until) VALUES (1, 'status@broadcast', 'other', 'request', 'status', '2026-10-01', 'clear', '2026-11-01')`,
		`INSERT INTO matter_events (matter_id, kind, at) VALUES (1, 'opened', '2026-10-01')`,
		`INSERT INTO drafts (matter_id, chat_jid, text, state, written_at) VALUES (1, 'status@broadcast', 'draft', 'written', '2026-10-01')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	// The feature table is optional in the old schema but must also be cleaned on later starts.
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS reactions (chat_jid TEXT, message_id TEXT, sender TEXT, emoji TEXT, timestamp TIMESTAMP, PRIMARY KEY(chat_jid, message_id, sender)); INSERT INTO reactions VALUES ('status@broadcast', 'status', '111', '👍', '2026-10-01')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reopened, err := NewMessageStore()
		if err != nil {
			t.Fatal(err)
		}
		for _, table := range []string{"message_mentions", "polls", "poll_votes", "favorites", "matters", "matter_events", "drafts", "reactions"} {
			var count int
			if err := reopened.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
				t.Errorf("%s left %d rows: %v", table, count, err)
			}
		}
		var chat, content string
		if err := reopened.db.QueryRow(`SELECT chat_jid, content FROM messages`).Scan(&chat, &content); err != nil || chat != "normal" || content != "keep" {
			t.Errorf("normal message changed: %q %q %v", chat, content, err)
		}
		var count int
		if err := reopened.db.QueryRow("SELECT COUNT(*) FROM chats").Scan(&count); err != nil || count != 1 {
			t.Errorf("chats count %d: %v", count, err)
		}
		reopened.Close()
	}
	if _, err := os.Stat("store/messages.db"); err != nil {
		t.Fatal(err)
	}
}

func TestReactionsLoadForManyMessages(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	messages := make([]APIMessage, 40000) // more bound parameters than SQLite allows in one query
	for i := range messages {
		messages[i] = APIMessage{ChatJID: chat, ID: fmt.Sprintf("m%d", i)}
	}
	if err := s.StoreReaction(chat, "m39999", "222", "👍", 0, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := loadReactions(s.db, messages); err != nil {
		t.Fatal(err)
	}
	if got := messages[39999].Reactions; len(got) != 1 || got[0].Emoji != "👍" {
		t.Fatalf("reactions on the last message: %+v", got)
	}
}
