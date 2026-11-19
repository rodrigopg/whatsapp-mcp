package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type backfillClient struct {
	*whatsmeow.Client
	connected bool
	sent      *waE2E.Message
	err       error
}

func (c *backfillClient) IsConnected() bool { return c.connected }

func (c *backfillClient) SendPeerMessage(ctx context.Context, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	c.sent = msg
	if _, ok := ctx.Deadline(); !ok {
		return whatsmeow.SendResponse{}, errors.New("missing request deadline")
	}
	return whatsmeow.SendResponse{}, c.err
}

func TestChatBackfillOldestMessage(t *testing.T) {
	for _, fromMe := range []bool{false, true} {
		t.Run(map[bool]string{false: "incoming", true: "outgoing"}[fromMe], func(t *testing.T) {
			s := setupChatStore(t)
			for _, row := range []struct{ id, chat, ts string }{
				{"newer", "111@s.whatsapp.net", "2026-01-01T08:00:00Z"},
				{"oldest", "111@s.whatsapp.net", "2026-01-01T09:00:00+02:00"},
				{"other-chat", "222@s.whatsapp.net", "2025-01-01T00:00:00Z"},
			} {
				ts, err := time.Parse(time.RFC3339, row.ts)
				if err != nil {
					t.Fatal(err)
				}
				if err := s.EnsureChat(row.chat, time.Time{}); err != nil {
					t.Fatal(err)
				}
				if err := s.StoreMessage(row.id, row.chat, "111", "text", ts, fromMe, "", "", "", nil, nil, nil, 0); err != nil {
					t.Fatal(err)
				}
			}
			c := &backfillClient{connected: true}
			rec := httptest.NewRecorder()
			handleChatBackfill(c, s)(rec, httptest.NewRequest("POST", "/api/chat/backfill", strings.NewReader(`{"chat_jid":"111@s.whatsapp.net","count":50}`)))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			var result MarkChatResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || !result.Success {
				t.Fatalf("response %s, %v", rec.Body.String(), err)
			}
			req := c.sent.GetProtocolMessage().GetPeerDataOperationRequestMessage().GetHistorySyncOnDemandRequest()
			if req.GetChatJID() != "111@s.whatsapp.net" || req.GetOldestMsgID() != "oldest" || req.GetOldestMsgFromMe() != fromMe || req.GetOnDemandMsgCount() != 50 || req.GetOldestMsgTimestampMS() != 1767250800 {
				t.Fatalf("wrong history request: %v", req)
			}
		})
	}
}

func TestChatBackfillValidation(t *testing.T) {
	s := setupChatStore(t)
	for _, tc := range []struct {
		name, method, body string
		want               int
	}{
		{"method", "GET", "", 405},
		{"json", "POST", "{", 400},
		{"missing chat", "POST", `{"count":1}`, 400},
		{"invalid jid", "POST", `{"chat_jid":"x:y:z@s.whatsapp.net","count":1}`, 400},
		{"bare jid", "POST", `{"chat_jid":"111","count":1}`, 400},
		{"empty user", "POST", `{"chat_jid":"@s.whatsapp.net","count":1}`, 400},
		{"unsupported chat", "POST", `{"chat_jid":"status@broadcast","count":1}`, 400},
		{"missing count", "POST", `{"chat_jid":"111@s.whatsapp.net"}`, 400},
		{"negative count", "POST", `{"chat_jid":"111@s.whatsapp.net","count":-1}`, 400},
		{"too many", "POST", `{"chat_jid":"111@s.whatsapp.net","count":51}`, 400},
		{"overflow", "POST", `{"chat_jid":"111@s.whatsapp.net","count":4294967297}`, 400},
		{"empty chat", "POST", `{"chat_jid":"111@s.whatsapp.net","count":1}`, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &backfillClient{connected: true}
			rec := httptest.NewRecorder()
			handleChatBackfill(c, s)(rec, httptest.NewRequest(tc.method, "/api/chat/backfill", strings.NewReader(tc.body)))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
			if c.sent != nil {
				t.Fatal("invalid request woke phone")
			}
			if tc.want == 404 && !strings.Contains(rec.Body.String(), "No stored messages") {
				t.Fatal(rec.Body.String())
			}
		})
	}
}

func TestChatBackfillFailures(t *testing.T) {
	s := setupChatStore(t)
	if err := s.EnsureChat("111@s.whatsapp.net", time.Time{}); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("m", "111@s.whatsapp.net", "111", "text", time.Unix(100, 0), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		client *backfillClient
		want   int
	}{
		{"disconnected", &backfillClient{}, 503},
		{"send failed", &backfillClient{connected: true, err: errors.New("phone offline")}, 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			handleChatBackfill(tc.client, s)(rec, httptest.NewRequest("POST", "/api/chat/backfill", strings.NewReader(`{"chat_jid":"111@s.whatsapp.net","count":1}`)))
			if rec.Code != tc.want || strings.Contains(rec.Body.String(), "phone offline") {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	c := &backfillClient{connected: true}
	rec := httptest.NewRecorder()
	handleChatBackfill(c, s)(rec, httptest.NewRequest("POST", "/api/chat/backfill", strings.NewReader(`{"chat_jid":"111@s.whatsapp.net","count":1}`)))
	if rec.Code != 500 || c.sent != nil || strings.Contains(rec.Body.String(), "closed") {
		t.Fatalf("database failure: %d, sent %v", rec.Code, c.sent)
	}
}

func TestChatBackfillOnDemandStored(t *testing.T) {
	s := setupChatStore(t)
	if err := s.StoreChat("111@s.whatsapp.net", "Test", time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("existing", "111@s.whatsapp.net", "111", "transcription", time.Unix(100, 0), false, "audio", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	existing := historyMessage("existing", 100)
	existing.Message.Message = &waProto.Message{AudioMessage: &waProto.AudioMessage{}}
	handleHistorySync(nil, s, &events.HistorySync{Data: &waProto.HistorySync{
		SyncType:      waProto.HistorySync_ON_DEMAND.Enum(),
		Conversations: []*waProto.Conversation{{ID: proto.String("111@s.whatsapp.net"), Messages: []*waProto.HistorySyncMsg{historyMessage("older", 50), existing}}},
	}}, waLog.Noop)
	var content string
	if err := s.db.QueryRow(`SELECT content FROM messages WHERE id='older'`).Scan(&content); err != nil || content != "history text" {
		t.Fatalf("on demand not saved: %q, %v", content, err)
	}
	if err := s.db.QueryRow(`SELECT content FROM messages WHERE id='existing'`).Scan(&content); err != nil || content != "transcription" {
		t.Fatalf("transcription lost: %q, %v", content, err)
	}
	var ts time.Time
	if err := s.db.QueryRow(`SELECT last_message_time FROM chats WHERE jid='111@s.whatsapp.net'`).Scan(&ts); err != nil || ts.Unix() != 200 {
		t.Fatalf("last message moved back: %v, %v", ts, err)
	}
}
