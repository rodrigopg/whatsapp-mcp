package main

import (
	"context"
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

type historyLIDStore struct{ store.LIDStore }

func (historyLIDStore) GetPNForLID(context.Context, types.JID) (types.JID, error) {
	return types.NewJID("222", types.DefaultUserServer), nil
}

func historyMessage(id string, timestamp uint64) *waProto.HistorySyncMsg {
	return &waProto.HistorySyncMsg{Message: &waProto.WebMessageInfo{
		Key:              &waProto.MessageKey{ID: proto.String(id)},
		MessageTimestamp: proto.Uint64(timestamp),
		Message:          &waProto.Message{Conversation: proto.String("history text")},
	}}
}

func syncHistory(store *MessageStore, client *whatsmeow.Client, messages ...*waProto.HistorySyncMsg) {
	handleHistorySync(client, store, &events.HistorySync{Data: &waProto.HistorySync{
		Conversations: []*waProto.Conversation{{ID: proto.String("123-456@g.us"), Messages: messages}},
	}}, waLog.Noop)
}

func TestHistorySender(t *testing.T) {
	for _, tc := range []struct {
		name, participant, keyParticipant, want string
		fromMe                                  bool
	}{
		{"top level", "111@s.whatsapp.net", "", "111", false},
		{"top level takes precedence", "111@s.whatsapp.net", "333@s.whatsapp.net", "111", false},
		{"key fallback", "", "333@s.whatsapp.net", "333", false},
		{"LID", "444@lid", "", "222", false},
		{"own", "111@s.whatsapp.net", "333@s.whatsapp.net", "999", true},
		{"unknown", "", "", "", false},
		{"group is not an author", "123-456@g.us", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := setupChatStore(t)
			if err := s.StoreChat("123-456@g.us", "Test group", time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
			own := types.NewJID("999", types.DefaultUserServer)
			client := &whatsmeow.Client{Store: &store.Device{ID: &own, LIDs: historyLIDStore{}}}
			m := historyMessage("msg", 100)
			m.Message.Participant = proto.String(tc.participant)
			m.Message.Key.Participant = proto.String(tc.keyParticipant)
			m.Message.Key.FromMe = proto.Bool(tc.fromMe)
			syncHistory(s, client, m)
			messages, err := s.GetMessages("123-456@g.us", 10)
			if err != nil || len(messages) != 1 {
				t.Fatalf("messages = %v, %v", messages, err)
			}
			if messages[0].Sender != tc.want || messages[0].IsFromMe != tc.fromMe {
				t.Fatalf("message = %+v, want sender %q, own %v", messages[0], tc.want, tc.fromMe)
			}
		})
	}
}

func TestHistorySenderWithoutClient(t *testing.T) {
	own := types.NewJID("999", types.DefaultUserServer)
	own.Device = 7
	for _, tc := range []struct {
		name    string
		chat    types.JID
		message *waProto.WebMessageInfo
		want    types.JID
	}{
		{"own non-AD", types.NewJID("123-456", types.GroupServer), &waProto.WebMessageInfo{Key: &waProto.MessageKey{FromMe: proto.Bool(true)}}, own.ToNonAD()},
		{"direct ignores participant", types.NewJID("111", types.DefaultUserServer), &waProto.WebMessageInfo{Participant: proto.String("333@s.whatsapp.net")}, types.NewJID("111", types.DefaultUserServer)},
		{"LID direct", types.NewJID("444", types.HiddenUserServer), nil, types.NewJID("444", types.HiddenUserServer)},
		{"missing key", types.NewJID("123-456", types.GroupServer), &waProto.WebMessageInfo{Participant: proto.String("111@s.whatsapp.net")}, types.NewJID("111", types.DefaultUserServer)},
		{"missing author", types.NewJID("123-456", types.GroupServer), nil, types.EmptyJID},
		{"malformed author", types.NewJID("123-456", types.GroupServer), &waProto.WebMessageInfo{Participant: proto.String("111:x@s.whatsapp.net")}, types.EmptyJID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := historySender(tc.chat, own, tc.message); got != tc.want {
				t.Fatalf("sender = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestHistoryResyncPreservesKnownAuthorAndTranscription(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("123-456@g.us", "Test group", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("audio", "123-456@g.us", "111", "transcription", time.Unix(100, 0), false, "audio", "clip.ogg", "url", []byte("key"), nil, nil, 42); err != nil {
		t.Fatal(err)
	}
	m := historyMessage("audio", 100)
	m.Message.Message = &waProto.Message{AudioMessage: &waProto.AudioMessage{URL: proto.String("url")}}
	syncHistory(s, nil, m)
	got, err := s.GetMessages("123-456@g.us", 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("messages = %+v, %v", got, err)
	}
	if got[0].Sender != "111" || got[0].Content != "transcription" {
		t.Fatalf("resync changed author or transcription: %+v", got[0])
	}
}

func TestHistorySkipsOnlyBadMessagesAndUsesMaximumTime(t *testing.T) {
	for _, first := range []*waProto.HistorySyncMsg{nil, {}, {Message: &waProto.WebMessageInfo{}}, historyMessage("no-time", 0), historyMessage("", 1000), historyMessage("oldest", 10)} {
		t.Run("first", func(t *testing.T) {
			s := setupChatStore(t)
			if err := s.StoreChat("123-456@g.us", "Test group", time.Unix(1, 0)); err != nil {
				t.Fatal(err)
			}
			empty := historyMessage("empty", 300)
			empty.Message.Message = nil
			syncHistory(s, nil, first, historyMessage("newest", 200), historyMessage("older", 100), empty)
			var count int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id IN ('newest', 'older')").Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 2 {
				t.Fatalf("stored %d good messages, want 2", count)
			}
			var bad int
			if err := s.db.QueryRow("SELECT COUNT(*) FROM messages WHERE id IN ('no-time', 'empty')").Scan(&bad); err != nil {
				t.Fatal(err)
			}
			if bad != 0 {
				t.Fatalf("stored %d bad messages", bad)
			}
			assertHistoryChat(t, s, "Test group", time.Unix(200, 0))
			syncHistory(s, nil, historyMessage("backfill", 50))
			assertHistoryChat(t, s, "Test group", time.Unix(200, 0))
		})
	}
}

func assertHistoryChat(t *testing.T, s *MessageStore, name string, timestamp time.Time) {
	t.Helper()
	var gotName string
	var gotTime time.Time
	if err := s.db.QueryRow("SELECT name, last_message_time FROM chats WHERE jid = '123-456@g.us'").Scan(&gotName, &gotTime); err != nil {
		t.Fatal(err)
	}
	if gotName != name || !gotTime.Equal(timestamp) {
		t.Fatalf("chat = %q %v, want %q %v", gotName, gotTime, name, timestamp)
	}
}

func TestStoreChatPreservesNameAndMonotonicTime(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("123-456@g.us", "Test group", ts("2026-10-22T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []string{"2026-10-22T08:00:00Z", "2026-10-22T10:30:00+03:00", "2026-10-22T08:00:00Z"} {
		if err := s.StoreChat("123-456@g.us", "", ts(stamp)); err != nil {
			t.Fatal(err)
		}
		assertHistoryChat(t, s, "Test group", ts("2026-10-22T08:00:00Z"))
	}
	if err := s.StoreChat("123-456@g.us", "Renamed", ts("2026-10-21T08:00:00Z")); err != nil {
		t.Fatal(err)
	}
	assertHistoryChat(t, s, "Renamed", ts("2026-10-22T08:00:00Z"))
}

func TestTouchChatTimeIsMonotonic(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("123-456@g.us", "Test group", ts("2026-10-22T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []string{"2026-10-22T08:00:00Z", "2026-10-22T10:30:00+03:00"} {
		if err := s.TouchChatLastMessageTime("123-456@g.us", ts(stamp)); err != nil {
			t.Fatal(err)
		}
		assertHistoryChat(t, s, "Test group", ts("2026-10-22T08:00:00Z"))
	}
}

func TestLiveChatTimeOnlyFollowsStoredMessages(t *testing.T) {
	for name, body := range map[string]*waProto.Message{
		"empty":       {},
		"reaction":    {ReactionMessage: &waProto.ReactionMessage{Text: proto.String("ok")}},
		"poll vote":   {PollUpdateMessage: &waProto.PollUpdateMessage{}},
		"write fails": {Conversation: proto.String("text")},
		"stored":      {Conversation: proto.String("text")},
	} {
		t.Run(name, func(t *testing.T) {
			s := setupChatStore(t)
			if err := s.StoreChat("123-456@g.us", "Test group", time.Unix(100, 0)); err != nil {
				t.Fatal(err)
			}
			if name == "write fails" {
				if _, err := s.db.Exec(`CREATE TRIGGER fail_message BEFORE INSERT ON messages BEGIN SELECT RAISE(FAIL, 'test write failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			handleMessage(&whatsmeow.Client{}, s, &events.Message{
				Info: types.MessageInfo{ID: "live", Timestamp: time.Unix(200, 0), MessageSource: types.MessageSource{
					Chat: types.NewJID("123-456", types.GroupServer), Sender: types.NewJID("111", types.DefaultUserServer), IsGroup: true,
				}}, Message: body,
			}, waLog.Noop)
			want := int64(100)
			if name == "stored" {
				want = 200
			}
			assertHistoryChat(t, s, "Test group", time.Unix(want, 0))
		})
	}
}

func TestHistoryDoesNotAdvanceChatTimeOnWriteFailure(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("123-456@g.us", "Test group", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_message BEFORE INSERT ON messages WHEN NEW.id = 'fail' BEGIN SELECT RAISE(FAIL, 'test write failure'); END`); err != nil {
		t.Fatal(err)
	}
	syncHistory(s, nil, historyMessage("fail", 300), historyMessage("stored", 200))
	assertHistoryChat(t, s, "Test group", time.Unix(200, 0))
}

func TestStartupRepairsGroupAuthorsWithoutLosingContent(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("123-456@g.us", "Test group", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	for id, sender := range map[string]string{"bare": "123-456", "jid": "123-456@g.us", "valid": "111"} {
		if err := s.StoreMessage(id, "123-456@g.us", sender, "transcription", time.Unix(100, 0), false, "audio", "clip.ogg", "url", []byte("key"), nil, nil, 42); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.StoreChat("123-456@s.whatsapp.net", "Direct", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("dm", "123-456@s.whatsapp.net", "123-456", "direct", time.Unix(100, 0), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		reopened, err := NewMessageStore()
		if err != nil {
			t.Fatal(err)
		}
		if err := reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{"bare": "", "jid": "", "valid": "111"} {
		var sender, content, filename, key string
		var length int
		if err := s.db.QueryRow("SELECT sender, content, filename, media_key, file_length FROM messages WHERE id=?", id).Scan(&sender, &content, &filename, &key, &length); err != nil {
			t.Fatal(err)
		}
		if sender != want || content != "transcription" || filename != "clip.ogg" || key != "key" || length != 42 {
			t.Fatalf("repair changed data: %q %q %q %q %d", sender, content, filename, key, length)
		}
	}
	var dm string
	if err := s.db.QueryRow("SELECT sender FROM messages WHERE id='dm'").Scan(&dm); err != nil {
		t.Fatal(err)
	}
	if dm != "123-456" {
		t.Fatalf("direct sender = %q", dm)
	}
	chat := "123-456@g.us"
	api, err := listMessages(s.db, MessagesRequest{ChatJID: &chat, Limit: 10})
	if err != nil || len(api.Messages) != 3 {
		t.Fatalf("API messages = %+v, %v", api, err)
	}
	for _, m := range api.Messages {
		if m.ID != "valid" && m.Sender != "" {
			t.Errorf("API sender = %q, want empty", m.Sender)
		}
	}
}

func TestUnknownSenderNameDoesNotMatchAnArbitraryChat(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("123-456@g.us", "Test group", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	got, err := getSenderName(s.db, "")
	if err != nil || got.Name != "Unknown sender" {
		t.Fatalf("sender name = %+v, %v", got, err)
	}
}

func TestMediaRetryRejectsUnknownGroupAuthor(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("123-456@g.us", "Test group", time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	for _, sender := range []string{"", "123-456", "123-456@g.us"} {
		if err := s.StoreMessage("retry", "123-456@g.us", sender, "", time.Unix(100, 0), false, "audio", "clip.ogg", "url", []byte("key"), nil, nil, 42); err != nil {
			t.Fatal(err)
		}
		err := requestMediaRetry(nil, s, "retry", "123-456@g.us")
		if err == nil || !strings.Contains(err.Error(), "unknown group message sender") {
			t.Fatalf("retry for %q: %v", sender, err)
		}
		if _, ok := mediaRetryCache.consume("retry"); ok {
			t.Error("invalid retry cached")
		}
	}
}

func TestResyncWithOtherOffsetKeepsLastMessageJoin(t *testing.T) {
	s := setupChatStore(t)
	if err := s.EnsureChat("123-456@g.us", ts("2026-10-22T06:00:00Z")); err != nil {
		t.Fatal(err)
	}
	for _, stamp := range []string{"2026-10-22T10:00:00+03:00", "2026-10-22T07:00:00Z"} {
		if err := s.StoreMessage("m1", "123-456@g.us", "111", "hello", ts(stamp), false, "", "", "", nil, nil, nil, 0); err != nil {
			t.Fatal(err)
		}
		if err := s.StoreChat("123-456@g.us", "Test group", ts(stamp)); err != nil {
			t.Fatal(err)
		}
	}
	resp, err := listChats(s.db, ChatsRequest{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Chats) != 1 || resp.Chats[0].LastMessage == nil || *resp.Chats[0].LastMessage != "hello" {
		t.Fatalf("chats = %+v, want last message %q", resp.Chats, "hello")
	}
}
