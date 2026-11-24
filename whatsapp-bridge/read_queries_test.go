package main

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func readMessage(t *testing.T, db *sql.DB, chat, id, sender, at string, own bool) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename)
		VALUES (?, ?, ?, ?, ?, ?, '', '')`, id, chat, sender, id, ts(at), own)
	if err != nil {
		t.Fatal(err)
	}
}

func readIDs(messages []APIMessage) []string {
	ids := []string{}
	for _, m := range messages {
		ids = append(ids, m.ID)
	}
	return ids
}

func TestReadMessagesByInstant(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	if err := s.StoreChat(chat, "Test", ts("2026-10-01T09:00:00Z")); err != nil {
		t.Fatal(err)
	}
	readMessage(t, s.db, chat, "old", "111", "2026-10-01T10:00:00+03:00", false)
	readMessage(t, s.db, chat, "target", "111", "2026-10-01T08:00:00Z", false)
	readMessage(t, s.db, chat, "tie", "111", "2026-10-01T05:00:00-03:00", false)
	readMessage(t, s.db, chat, "new", "111", "2026-10-01T06:00:00-03:00", false)
	for _, tc := range []struct {
		name string
		req  MessagesRequest
		want []string
	}{
		{"all", MessagesRequest{}, []string{"new", "tie", "target", "old"}},
		{"after", MessagesRequest{After: strp("2026-10-01T11:00:00+03:00")}, []string{"new"}},
		{"before", MessagesRequest{Before: strp("2026-10-01T05:00:00-03:00")}, []string{"old"}},
		{"window", MessagesRequest{After: strp("2026-10-01T07:30:00Z"), Before: strp("2026-10-01T08:30:00Z")}, []string{"tie", "target"}},
		{"page", MessagesRequest{ChatJID: &chat, Limit: 1, Page: 2}, []string{"target"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := listMessages(s.db, tc.req)
			if err != nil || !reflect.DeepEqual(readIDs(got.Messages), tc.want) {
				t.Fatalf("messages = %v, %v; want %v", readIDs(got.Messages), err, tc.want)
			}
		})
	}
	t.Run("store", func(t *testing.T) {
		got, err := s.GetMessages(chat, 2)
		if err != nil || len(got) != 2 || got[0].Content != "new" || got[1].Content != "tie" {
			t.Fatalf("GetMessages = %+v, %v", got, err)
		}
	})
	for _, tc := range []struct {
		id            string
		before, after []string
	}{
		{"target", []string{"old"}, []string{"tie", "new"}},
		{"tie", []string{"target", "old"}, []string{"new"}},
	} {
		t.Run("context "+tc.id, func(t *testing.T) {
			got, found, err := getMessageContext(s.db, MessageContextRequest{MessageID: tc.id})
			if err != nil || !found || !reflect.DeepEqual(readIDs(got.Before), tc.before) || !reflect.DeepEqual(readIDs(got.After), tc.after) {
				t.Fatalf("context = %+v, %v, %v", got, found, err)
			}
		})
	}
}

func TestReadChatsLatestAndPagination(t *testing.T) {
	s := setupChatStore(t)
	a, b := "111@s.whatsapp.net", "222@s.whatsapp.net"
	seedChatWithLastMessage(t, s.db, a, "A", "first", "111", false, ts("2026-10-01T08:00:00Z"))
	readMessage(t, s.db, a, "latest", "999", "2026-10-01T08:00:00Z", true)
	readMessage(t, s.db, a, "older", "111", "2026-10-01T10:00:00+03:00", false)
	seedChatWithLastMessage(t, s.db, b, "B", "B last", "222", false, ts("2026-10-01T09:00:00Z"))
	for _, at := range []string{"2026-10-01T08:00:00Z", "2026-10-01T11:00:00+03:00"} {
		if _, err := s.db.Exec(`UPDATE chats SET last_message_time = ? WHERE jid = ?`, ts(at), a); err != nil {
			t.Fatal(err)
		}
		t.Run("list "+at, func(t *testing.T) {
			for page, want := range []string{b, a} {
				got, err := listChats(s.db, ChatsRequest{Limit: 1, Page: page})
				if err != nil || len(got.Chats) != 1 || got.Chats[0].JID != want {
					t.Fatalf("page %d = %+v, %v", page, got, err)
				}
			}
			got, err := listChats(s.db, ChatsRequest{})
			if err != nil || len(got.Chats) != 2 || got.Chats[1].LastMessage == nil || *got.Chats[1].LastMessage != "latest" {
				t.Fatalf("chats = %+v, %v", got, err)
			}
		})
		for name, lookup := range map[string]func() (ChatResponse, error){
			"get": func() (ChatResponse, error) { return getChat(s.db, ChatRequest{ChatJID: a}) },
			"direct": func() (ChatResponse, error) {
				return getDirectChatByContact(s.db, ChatByContactRequest{SenderPhoneNumber: "111"})
			},
		} {
			t.Run(name+" "+at, func(t *testing.T) {
				got, err := lookup()
				if err != nil || got.Chat == nil || got.Chat.LastMessage == nil || *got.Chat.LastMessage != "latest" || got.Chat.LastIsFromMe == nil || !*got.Chat.LastIsFromMe {
					t.Fatalf("chat = %+v, %v", got.Chat, err)
				}
			})
		}
	}
}

func TestReadContactChats(t *testing.T) {
	s := setupChatStore(t)
	for _, chat := range []string{"group@g.us", "111@s.whatsapp.net"} {
		if err := s.StoreChat(chat, "Test", ts("2026-10-01T11:00:00+03:00")); err != nil {
			t.Fatal(err)
		}
		readMessage(t, s.db, chat, "old", "111", "2026-10-01T10:00:00+03:00", false)
		readMessage(t, s.db, chat, "middle", "111", "2026-10-01T08:00:00Z", false)
		readMessage(t, s.db, chat, "latest", "999", "2026-10-01T05:00:00-03:00", true)
	}
	t.Run("chats", func(t *testing.T) {
		got, err := getContactChats(s.db, ContactChatsRequest{JID: "111@s.whatsapp.net"})
		if err != nil || len(got.Chats) != 2 {
			t.Fatalf("contact chats = %+v, %v", got, err)
		}
		for _, c := range got.Chats {
			if c.LastMessage == nil || *c.LastMessage != "latest" {
				t.Errorf("last message = %+v", c)
			}
		}
	})
	t.Run("last interaction", func(t *testing.T) {
		last, err := getLastInteraction(s.db, "111@s.whatsapp.net")
		if err != nil || last.Message == nil || last.Message.ID != "latest" {
			t.Fatalf("last interaction = %+v, %v", last, err)
		}
	})
}

func TestReadSenderPNAndLID(t *testing.T) {
	s := setupChatStore(t)
	mapping, err := sql.Open("sqlite3", "store/whatsapp.db")
	if err != nil {
		t.Fatal(err)
	}
	defer mapping.Close()
	if _, err := mapping.Exec(`CREATE TABLE whatsmeow_lid_map (lid TEXT, pn TEXT); INSERT INTO whatsmeow_lid_map VALUES ('222', '111')`); err != nil {
		t.Fatal(err)
	}
	for _, chat := range []string{"group@g.us", "111@s.whatsapp.net"} {
		if err := s.StoreChat(chat, "Test", ts("2026-10-01T08:00:00Z")); err != nil {
			t.Fatal(err)
		}
		for _, sender := range []string{"111", "111@s.whatsapp.net", "222", "222@lid", "333", "999"} {
			readMessage(t, s.db, chat, sender+chat, sender, "2026-10-01T08:00:00Z", sender == "999")
		}
	}
	for _, chat := range []*string{nil, strp("group@g.us")} {
		got, err := listMessages(s.db, MessagesRequest{SenderPhoneNumber: strp("+111"), ChatJID: chat})
		want := 8
		if chat != nil {
			want = 4
		}
		if err != nil || len(got.Messages) != want {
			t.Fatalf("sender messages = %+v, %v; want %d", got, err, want)
		}
		for _, m := range got.Messages {
			if m.IsFromMe || m.Sender == "333" {
				t.Errorf("wrong author: %+v", m)
			}
		}
	}
	got, err := listMessages(s.db, MessagesRequest{SenderPhoneNumber: strp("999")})
	if err != nil || len(got.Messages) != 2 || !got.Messages[0].IsFromMe {
		t.Fatalf("owner filter = %+v, %v", got, err)
	}
}

func TestReadQueryIndexes(t *testing.T) {
	s := setupChatStore(t)
	seedChatWithLastMessage(t, s.db, "111@s.whatsapp.net", "Test", "preserved", "111", false, ts("2026-10-01T08:00:00Z"))
	for _, name := range []string{"idx_messages_chat_time", "idx_messages_chat_jtime", "idx_messages_time", "idx_messages_sender_time", "idx_chats_time"} {
		if _, err := s.db.Exec("DROP INDEX IF EXISTS " + name); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version string
	if err := s.db.QueryRow("SELECT sqlite_version()").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Log("SQLite", version)
	got, err := getChat(s.db, ChatRequest{ChatJID: "111@s.whatsapp.net"})
	if err != nil || got.Chat == nil || got.Chat.LastMessage == nil || *got.Chat.LastMessage != "preserved" {
		t.Fatalf("reopened chat = %+v, %v", got, err)
	}
	for _, tc := range []struct{ name, query, index string }{
		{"raw chat time", `SELECT id FROM messages WHERE chat_jid = '111' ORDER BY timestamp DESC`, "idx_messages_chat_time"},
		{"chat time", `SELECT m.timestamp, m.sender, c.name, m.content, m.is_from_me, c.jid, m.id, m.media_type FROM messages m JOIN chats c ON c.jid = m.chat_jid WHERE m.chat_jid = '111' AND julianday(m.timestamp) > julianday('2026-10-01') ORDER BY julianday(m.timestamp) DESC, m.rowid DESC LIMIT 20`, "idx_messages_chat_jtime"},
		{"time", `SELECT m.id FROM messages m JOIN chats c ON c.jid = m.chat_jid WHERE julianday(m.timestamp) > julianday('2026-10-01') ORDER BY julianday(m.timestamp) DESC, m.rowid DESC LIMIT 20`, "idx_messages_time"},
		{"sender", `SELECT id FROM messages WHERE sender = '111' ORDER BY julianday(timestamp) DESC, rowid DESC LIMIT 20`, "idx_messages_sender_time"},
		{"chats", `SELECT c.jid, c.name, c.last_message_time, m.content, m.sender, m.is_from_me FROM chats c LEFT JOIN messages m ON m.rowid = (SELECT rowid FROM messages WHERE chat_jid = c.jid ORDER BY julianday(timestamp) DESC, rowid DESC LIMIT 1) ORDER BY julianday(c.last_message_time) DESC, c.jid LIMIT 20`, "idx_messages_chat_jtime"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := s.db.Query("EXPLAIN QUERY PLAN " + tc.query)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			var plan []string
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					t.Fatal(err)
				}
				plan = append(plan, detail)
			}
			joined := strings.Join(plan, "\n")
			if err := rows.Err(); err != nil || !strings.Contains(joined, tc.index) || strings.Contains(joined, "TEMP B-TREE") {
				t.Fatalf("plan = %s; error = %v; want %s without sorting", joined, err, tc.index)
			}
			t.Log(joined)
		})
	}
}

func TestReadOtherTimeQueries(t *testing.T) {
	s := setupChatStore(t)
	for _, c := range []struct{ jid, at string }{
		{"111@s.whatsapp.net", "2026-10-01T10:00:00+03:00"},
		{"222@s.whatsapp.net", "2026-10-01T08:00:00Z"},
		{"333@s.whatsapp.net", "2026-10-01T05:00:00-03:00"},
	} {
		if err := s.StoreChat(c.jid, "Test", ts(c.at)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`INSERT INTO favorites VALUES (?, 'favorite', 'owner', ?)`, c.jid, ts(c.at)); err != nil {
			t.Fatal(err)
		}
	}
	favorites, err := listFavorites(s.db)
	if err != nil || len(favorites) != 3 || favorites[0].JID != "111@s.whatsapp.net" || favorites[1].JID != "222@s.whatsapp.net" {
		t.Errorf("favorites = %+v, %v", favorites, err)
	}
	if err := storePoll(s.db, "poll", "group@g.us", "Question", []string{"A", "B"}, 1); err != nil {
		t.Fatal(err)
	}
	for i, at := range []string{"2026-10-01T10:00:00+03:00", "2026-10-01T08:00:00Z", "2026-10-01T05:00:00-03:00"} {
		if _, err := s.db.Exec(`INSERT INTO poll_votes VALUES ('poll', 'group@g.us', ?, '["A"]', ?)`, i, ts(at)); err != nil {
			t.Fatal(err)
		}
	}
	votes, err := getPollVotes(s.db, "group@g.us", "poll")
	if err != nil || len(votes.Votes) != 3 || votes.Votes[0].Voter != "0" || votes.Votes[1].Voter != "1" {
		t.Errorf("votes = %+v, %v", votes, err)
	}
}

func TestReadLIDMergeTime(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("444@lid", "Test", ts("2026-10-01T08:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreChat("222@s.whatsapp.net", "Test", ts("2026-10-01T10:00:00+03:00")); err != nil {
		t.Fatal(err)
	}
	client := &whatsmeow.Client{Store: &store.Device{LIDs: historyLIDStore{}}}
	migrateLIDChats(client, s, waLog.Noop)
	chats, err := s.GetChats()
	if err != nil || !chats["222@s.whatsapp.net"].Equal(ts("2026-10-01T08:00:00Z")) {
		t.Fatalf("merged chats = %v, %v", chats, err)
	}
}

// A timestamp julianday() cannot parse has no instant: it sorts after every dated
// row and no time window can claim it, instead of landing at a random text position.
func TestReadUnparseableTimestamp(t *testing.T) {
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	if err := s.StoreChat(chat, "Test", ts("2026-10-01T09:00:00Z")); err != nil {
		t.Fatal(err)
	}
	readMessage(t, s.db, chat, "dated", "111", "2026-10-01T08:00:00Z", false)
	if _, err := s.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename)
		VALUES ('broken', ?, '111', 'broken', 'not a time', 0, '', '')`, chat); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		req  MessagesRequest
		want []string
	}{
		{"all", MessagesRequest{ChatJID: &chat}, []string{"dated", "broken"}},
		{"after", MessagesRequest{ChatJID: &chat, After: strp("2026-01-01T00:00:00Z")}, []string{"dated"}},
		{"before", MessagesRequest{ChatJID: &chat, Before: strp("2027-01-01T00:00:00Z")}, []string{"dated"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := listMessages(s.db, tc.req)
			if err != nil || !reflect.DeepEqual(readIDs(got.Messages), tc.want) {
				t.Fatalf("messages = %v, %v; want %v", readIDs(got.Messages), err, tc.want)
			}
		})
	}
}
