package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

type fakeMessageActions struct {
	whatsmeowActions
	sender, chat types.JID
	id           types.MessageID
	message      *waProto.Message
	builds, sent int
	sendErr      error
	group        types.GroupInfo
	lids         fakeLIDs
}

// fakeLIDs is the LID <-> number store; only the two lookups are used.
type fakeLIDs struct {
	store.LIDStore
	pairs map[types.JID]types.JID // number -> LID
	err   error
}

func (f fakeLIDs) GetLIDForPN(_ context.Context, pn types.JID) (types.JID, error) {
	return f.pairs[pn], f.err
}

func (f fakeLIDs) GetPNForLID(_ context.Context, lid types.JID) (types.JID, error) {
	for pn, l := range f.pairs {
		if l == lid {
			return pn, nil
		}
	}
	return types.EmptyJID, nil
}

func (f *fakeMessageActions) GroupAuthor(ctx context.Context, _ types.JID, author types.JID) (types.JID, error) {
	return groupActionAuthor(ctx, &f.group, author, f.lids)
}

func newFakeMessageActions() *fakeMessageActions {
	own := types.NewADJID("111", 0, 1)
	return &fakeMessageActions{whatsmeowActions: whatsmeowActions{&whatsmeow.Client{Store: &store.Device{ID: &own}}}}
}

func (f *fakeMessageActions) IsConnected() bool { return true }

func (f *fakeMessageActions) BuildReaction(chat, sender types.JID, id types.MessageID, emoji string) *waProto.Message {
	f.chat, f.sender, f.id = chat, sender, id
	f.builds++
	return f.Client.BuildReaction(chat, sender, id, emoji)
}

func (f *fakeMessageActions) BuildRevoke(chat, sender types.JID, id types.MessageID) *waProto.Message {
	f.chat, f.sender, f.id = chat, sender, id
	f.builds++
	return f.Client.BuildRevoke(chat, sender, id)
}

func (f *fakeMessageActions) SendMessage(_ context.Context, chat types.JID, msg *waProto.Message, _ ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error) {
	f.sent++
	f.message = msg
	if chat != f.chat {
		return whatsmeow.SendResponse{}, fmt.Errorf("sent to %s, built for %s", chat, f.chat)
	}
	return whatsmeow.SendResponse{}, f.sendErr
}

func groupActionHandler(action string, client messageActionClient, store *MessageStore) http.HandlerFunc {
	if action == "react" {
		return handleReact(client, store)
	}
	return handleRevoke(client, store)
}

func seedActionMessage(t *testing.T, store *MessageStore, id, chat, sender, content string, fromMe bool) {
	t.Helper()
	now := time.Now()
	if err := store.StoreChat(chat, "chat", now); err != nil {
		t.Fatal(err)
	}
	if err := store.StoreMessage(id, chat, sender, content, now, fromMe, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
}

func TestGroupMessageActions(t *testing.T) {
	for _, action := range []string{"react", "revoke"} {
		for _, c := range []struct {
			name, chat, sender, wantSender string
			fromMe                         bool
		}{
			{"group PN", "123@g.us", "222", "222@s.whatsapp.net", false},
			{"group PN removal", "123@g.us", "222", "222@s.whatsapp.net", false},
			{"group full PN", "123@g.us", "222@s.whatsapp.net", "222@s.whatsapp.net", false},
			{"group LID", "123@g.us", "333@lid", "333@lid", false},
			{"own group", "123@g.us", "", "111@s.whatsapp.net", true},
			{"direct", "222@s.whatsapp.net", "", "222@s.whatsapp.net", false},
			{"direct LID", "333@lid", "", "333@lid", false},
			{"own direct", "222@s.whatsapp.net", "", "111@s.whatsapp.net", true},
		} {
			t.Run(action+"/"+c.name, func(t *testing.T) {
				store := setupChatStore(t)
				seedActionMessage(t, store, "target", c.chat, c.sender, "original", c.fromMe)
				// The same message ID in another chat must not supply the author.
				seedActionMessage(t, store, "target", "999@g.us", "444", "other chat", false)
				client := newFakeMessageActions()
				if strings.HasSuffix(c.sender, "@lid") {
					client.group.AddressingMode = types.AddressingModeLID
				}
				emoji := "👍"
				if c.name == "group PN removal" {
					emoji = ""
				}
				body, _ := json.Marshal(ReactRequest{ChatJID: c.chat, MessageID: "target", Emoji: emoji, FromMe: c.fromMe})
				rec := doHandlerRequest(t, groupActionHandler(action, client, store), http.MethodPost, body)
				if rec.Code != http.StatusOK {
					t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
				}
				if client.sender.String() != c.wantSender || client.chat.String() != c.chat || client.id != "target" || client.builds != 1 || client.sent != 1 {
					t.Fatalf("built for chat=%s sender=%s id=%s; builds=%d sent=%d", client.chat, client.sender, client.id, client.builds, client.sent)
				}
				if action == "react" && client.message.GetReactionMessage().GetText() != emoji {
					t.Fatalf("reaction = %v, want emoji %q", client.message.GetReactionMessage(), emoji)
				}
				key := client.message.GetReactionMessage().GetKey()
				wantContent := "original"
				if action == "revoke" {
					key = client.message.GetProtocolMessage().GetKey()
					if c.fromMe || strings.HasSuffix(c.chat, "@g.us") {
						wantContent = revokedContent
					}
				}
				wantParticipant := ""
				if !c.fromMe && strings.HasSuffix(c.chat, "@g.us") {
					wantParticipant = c.wantSender
				}
				if key.GetFromMe() != c.fromMe || key.GetParticipant() != wantParticipant || key.GetID() != "target" || key.GetRemoteJID() != c.chat {
					t.Fatalf("message key = %v", key)
				}
				var content, otherContent string
				if err := store.db.QueryRow("SELECT content FROM messages WHERE id = ? AND chat_jid = ?", "target", c.chat).Scan(&content); err != nil {
					t.Fatal(err)
				}
				if err := store.db.QueryRow("SELECT content FROM messages WHERE id = ? AND chat_jid = ?", "target", "999@g.us").Scan(&otherContent); err != nil {
					t.Fatal(err)
				}
				if content != wantContent || otherContent != "other chat" {
					t.Errorf("local content = %q (want %q), other chat = %q", content, wantContent, otherContent)
				}
			})
		}
	}
}

func TestGroupMessageActionsUnknownSender(t *testing.T) {
	for _, action := range []string{"react", "revoke"} {
		for _, sender := range []string{"", "123", "123@g.us", "999@g.us", "@lid", "x:y:z@lid"} {
			t.Run(action+"/"+sender, func(t *testing.T) {
				store := setupChatStore(t)
				seedActionMessage(t, store, "target", "123@g.us", sender, "original", false)
				client := newFakeMessageActions()
				rec := doHandlerRequest(t, groupActionHandler(action, client, store), http.MethodPost, []byte(`{"chat_jid":"123@g.us","message_id":"target","from_me":false}`))
				var resp MarkChatResponse
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatal(err)
				}
				if rec.Code != http.StatusBadRequest || resp.Success || !strings.Contains(resp.Message, "group message sender") || client.builds != 0 || client.sent != 0 {
					t.Fatalf("status=%d response=%+v builds=%d sent=%d", rec.Code, resp, client.builds, client.sent)
				}
			})
		}
		t.Run(action+"/missing in requested chat", func(t *testing.T) {
			store := setupChatStore(t)
			seedActionMessage(t, store, "target", "999@g.us", "222", "other chat", false)
			client := newFakeMessageActions()
			rec := doHandlerRequest(t, groupActionHandler(action, client, store), http.MethodPost, []byte(`{"chat_jid":"123@g.us","message_id":"target","from_me":false}`))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "group message sender") || client.sent != 0 {
				t.Fatalf("status=%d response=%s sent=%d", rec.Code, rec.Body.String(), client.sent)
			}
		})
	}
}

func TestGroupMessageActionSendError(t *testing.T) {
	for _, action := range []string{"react", "revoke"} {
		t.Run(action, func(t *testing.T) {
			store := setupChatStore(t)
			seedActionMessage(t, store, "target", "123@g.us", "333@lid", "original", false)
			client := newFakeMessageActions()
			client.group.AddressingMode = types.AddressingModeLID
			client.sendErr = errors.New("WhatsApp: forbidden (403), admin required")
			rec := doHandlerRequest(t, groupActionHandler(action, client, store), http.MethodPost, []byte(`{"chat_jid":"123@g.us","message_id":"target","from_me":false}`))
			var resp MarkChatResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusInternalServerError || resp.Success || resp.Message != "SendMessage error: "+client.sendErr.Error() || client.sent != 1 {
				t.Fatalf("status=%d response=%+v sent=%d", rec.Code, resp, client.sent)
			}
			var content string
			if err := store.db.QueryRow("SELECT content FROM messages WHERE id = ? AND chat_jid = ?", "target", "123@g.us").Scan(&content); err != nil {
				t.Fatal(err)
			}
			if content != "original" {
				t.Errorf("failed send changed local content to %q", content)
			}
		})
	}
}

func TestGroupActionAuthorFollowsAddressing(t *testing.T) {
	pn, lid := types.NewJID("222", types.DefaultUserServer), types.NewJID("333", types.HiddenUserServer)
	lids := fakeLIDs{pairs: map[types.JID]types.JID{pn: lid}}
	for _, c := range []struct {
		name   string
		group  types.GroupInfo
		author types.JID
		want   types.JID
	}{
		{"LID group member stored by number", types.GroupInfo{AddressingMode: types.AddressingModeLID,
			Participants: []types.GroupParticipant{{JID: lid, LID: lid, PhoneNumber: pn}}}, pn, lid},
		{"LID group former member", types.GroupInfo{AddressingMode: types.AddressingModeLID}, pn, lid},
		{"number group member stored by LID", types.GroupInfo{AddressingMode: types.AddressingModePN,
			Participants: []types.GroupParticipant{{JID: pn, LID: lid, PhoneNumber: pn}}}, lid, pn},
		{"number group former member", types.GroupInfo{AddressingMode: types.AddressingModePN}, lid, pn},
		{"LID group former member stored by LID", types.GroupInfo{AddressingMode: types.AddressingModeLID}, lid, lid},
	} {
		if got, err := groupActionAuthor(context.Background(), &c.group, c.author, lids); err != nil || got != c.want {
			t.Errorf("%s: %s (%v), want %s", c.name, got, err, c.want)
		}
	}

	// Through the handler: the key carries the LID although messages.sender holds the number.
	for _, action := range []string{"react", "revoke"} {
		s := setupChatStore(t)
		seedActionMessage(t, s, "target", "123@g.us", "222", "original", false)
		client := newFakeMessageActions()
		client.group = types.GroupInfo{AddressingMode: types.AddressingModeLID}
		client.lids = lids
		body, _ := json.Marshal(ReactRequest{ChatJID: "123@g.us", MessageID: "target", Emoji: "👍"})
		if rec := doHandlerRequest(t, groupActionHandler(action, client, s), http.MethodPost, body); rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", action, rec.Code, rec.Body.String())
		}
		key := client.message.GetReactionMessage().GetKey()
		if action == "revoke" {
			key = client.message.GetProtocolMessage().GetKey()
		}
		if key.GetParticipant() != "333@lid" {
			t.Errorf("%s: participant %q, want 333@lid", action, key.GetParticipant())
		}
	}
}

// A former member of a LID group whose LID is unknown, or a failed lookup, refuses the action: the
// key would carry the number, WhatsApp would drop it, and revoke would still blank the local text.
func TestGroupActionUnmappedAuthorRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		lids fakeLIDs
		code int
	}{
		{"unknown LID", fakeLIDs{}, http.StatusBadRequest},
		{"lookup error", fakeLIDs{err: errors.New("database is locked")}, http.StatusInternalServerError},
	} {
		for _, action := range []string{"react", "revoke"} {
			s := setupChatStore(t)
			seedActionMessage(t, s, "target", "123@g.us", "222", "original", false)
			client := newFakeMessageActions()
			client.group = types.GroupInfo{AddressingMode: types.AddressingModeLID}
			client.lids = c.lids
			body, _ := json.Marshal(ReactRequest{ChatJID: "123@g.us", MessageID: "target", Emoji: "👍"})
			rec := doHandlerRequest(t, groupActionHandler(action, client, s), http.MethodPost, body)
			if rec.Code != c.code || client.sent != 0 {
				t.Errorf("%s/%s: status=%d sent=%d: %s", c.name, action, rec.Code, client.sent, rec.Body.String())
			}
			var content string
			if err := s.db.QueryRow("SELECT content FROM messages WHERE id = ? AND chat_jid = ?", "target", "123@g.us").Scan(&content); err != nil || content != "original" {
				t.Errorf("%s/%s: content %q (%v)", c.name, action, content, err)
			}
		}
	}
}
