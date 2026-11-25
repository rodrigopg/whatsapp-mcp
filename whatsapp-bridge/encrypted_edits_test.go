package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/gcmutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type editSecrets struct {
	store.MsgSecretStore
	secret []byte
	err    error
}

func (s editSecrets) GetMessageSecret(context.Context, types.JID, types.JID, types.MessageID) ([]byte, types.JID, error) {
	return s.secret, types.NewJID("111", types.DefaultUserServer), s.err
}

type editLogger struct {
	waLog.Logger
	warnings []string
}

func (l *editLogger) Warnf(format string, args ...any) {
	l.warnings = append(l.warnings, fmt.Sprintf(format, args...))
}

func encryptedEditEvent(t *testing.T, reaction bool, text string, millis int64, bare ...bool) (*whatsmeow.Client, *events.Message) {
	t.Helper()
	secret := []byte("01234567890123456789012345678901")
	target := &waProto.MessageKey{ID: proto.String("target"), Participant: proto.String("111@s.whatsapp.net")}
	var plaintext []byte
	var err error
	useCase := whatsmeow.EncSecretMessageEdit
	if reaction {
		useCase = whatsmeow.EncSecretReaction
		plaintext, err = proto.Marshal(&waProto.ReactionMessage{Text: proto.String(text), SenderTimestampMS: proto.Int64(millis)})
	} else {
		body := protoMsg(waProto.ProtocolMessage_MESSAGE_EDIT, "target", &waProto.Message{Conversation: proto.String(text)})
		body.ProtocolMessage.TimestampMS = proto.Int64(millis)
		if len(bare) > 0 && bare[0] {
			plaintext, err = proto.Marshal(body.ProtocolMessage.EditedMessage)
		} else {
			plaintext, err = proto.Marshal(&waProto.Message{EditedMessage: &waProto.FutureProofMessage{Message: body}})
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	key := hkdfutil.SHA256(secret, nil, []byte("target111@s.whatsapp.net111@s.whatsapp.net"+string(useCase)), 32)
	iv := []byte("012345678901")
	ciphertext, err := gcmutil.Encrypt(key, iv, plaintext, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := &waProto.Message{SecretEncryptedMessage: &waProto.SecretEncryptedMessage{
		SecretEncType: waE2E.SecretEncryptedMessage_MESSAGE_EDIT.Enum(), TargetMessageKey: target, EncPayload: ciphertext, EncIV: iv,
	}}
	if reaction {
		body = &waProto.Message{EncReactionMessage: &waProto.EncReactionMessage{TargetMessageKey: target, EncPayload: ciphertext, EncIV: iv}}
	}
	client := &whatsmeow.Client{Store: &store.Device{MsgSecrets: editSecrets{secret: secret}}}
	return client, typeEvent("encrypted", "111", 200, body)
}

func editContent(t *testing.T, s *MessageStore, want string) {
	t.Helper()
	var got string
	if err := s.db.QueryRow(`SELECT content FROM messages WHERE id='target' AND chat_jid='123-456@g.us'`).Scan(&got); err != nil || got != want {
		t.Fatalf("content = %q, %v; want %q", got, err, want)
	}
}
func originalEditMessage() *waProto.HistorySyncMsg {
	m := historyMessage("target", 100)
	m.Message.Participant = proto.String("111@s.whatsapp.net")
	m.Message.Message.Conversation = proto.String("original")
	return m
}
func historyEdit(id, text string, millis int64, revoke bool) *waProto.HistorySyncMsg {
	m := historyMessage(id, 200)
	m.Message.Participant = proto.String("111@s.whatsapp.net")
	kind := waProto.ProtocolMessage_MESSAGE_EDIT
	if revoke {
		kind = waProto.ProtocolMessage_REVOKE
	}
	body := protoMsg(kind, "target", &waProto.Message{Conversation: proto.String(text)})
	body.ProtocolMessage.TimestampMS = proto.Int64(millis)
	m.Message.Message = &waProto.Message{EditedMessage: &waProto.FutureProofMessage{Message: body}}
	return m
}

func TestEncryptedEditAndReaction(t *testing.T) {
	for _, reaction := range []bool{false, true} {
		t.Run(fmt.Sprint(reaction), func(t *testing.T) {
			s := setupChatStore(t)
			syncHistory(s, nil, originalEditMessage())
			client, event := encryptedEditEvent(t, reaction, "updated", 300001)
			raw := proto.Clone(event.Message)
			if !handleMessage(client, s, event, waLog.Noop) {
				t.Fatal("event not acknowledged")
			}
			if !proto.Equal(event.Message, raw) {
				t.Fatal("encrypted event mutated; retries need its original envelope")
			}
			if reaction {
				var text string
				var at time.Time
				if err := s.db.QueryRow(`SELECT emoji, timestamp FROM reactions WHERE message_id='target' AND sender='111'`).Scan(&text, &at); err != nil || text != "updated" || at.UnixMilli() != 300001 {
					t.Fatalf("reaction = %q %v, %v", text, at, err)
				}
				editContent(t, s, "original")
			} else {
				editContent(t, s, "updated")
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("timeline count = %d, %v", count, err)
			}
		})
	}
}

func TestEncryptedFailureIsAcknowledgedWithoutContent(t *testing.T) {
	for _, reaction := range []bool{false, true} {
		for _, failure := range []string{"missing secret", "corrupt ciphertext", "missing type", "unknown type", "poll edit", "event edit"} {
			if reaction && (failure == "unknown type" || failure == "poll edit" || failure == "event edit") {
				continue
			}
			t.Run(fmt.Sprint(reaction)+failure, func(t *testing.T) {
				s := setupChatStore(t)
				syncHistory(s, nil, originalEditMessage())
				client, event := encryptedEditEvent(t, reaction, "private content", 300000)
				switch failure {
				case "missing type":
					if reaction {
						event.Message.EncReactionMessage.TargetMessageKey = nil
					} else {
						event.Message.SecretEncryptedMessage.SecretEncType = nil
					}
				case "unknown type":
					event.Message.SecretEncryptedMessage.SecretEncType = waE2E.SecretEncryptedMessage_SecretEncType(999).Enum()
				case "poll edit":
					event.Message.SecretEncryptedMessage.SecretEncType = waE2E.SecretEncryptedMessage_POLL_EDIT.Enum()
				case "event edit":
					event.Message.SecretEncryptedMessage.SecretEncType = waE2E.SecretEncryptedMessage_EVENT_EDIT.Enum()
				case "missing secret":
					client.Store.MsgSecrets = editSecrets{}
				case "corrupt ciphertext":
					if reaction {
						event.Message.EncReactionMessage.EncPayload = []byte("private ciphertext")
					} else {
						event.Message.SecretEncryptedMessage.EncPayload = []byte("private ciphertext")
					}
				}
				logger := &editLogger{Logger: waLog.Noop}
				if !handleMessage(client, s, event, logger) {
					t.Fatal("undecryptable event not acknowledged")
				}
				editContent(t, s, "original")
				if failure != "poll edit" && failure != "event edit" && failure != "unknown type" && len(logger.warnings) != 1 || strings.Contains(strings.Join(logger.warnings, " "), "private content") {
					t.Fatalf("warnings = %v", logger.warnings)
				}
			})
		}
	}
}

func TestEncryptedWritesAreNotAcknowledgedOnFailure(t *testing.T) {
	for _, reaction := range []bool{false, true} {
		t.Run(fmt.Sprint(reaction), func(t *testing.T) {
			s := setupChatStore(t)
			syncHistory(s, nil, originalEditMessage())
			table, operation := "messages", "UPDATE"
			if reaction {
				table, operation = "reactions", "INSERT"
			}
			mustExec(t, s, `CREATE TRIGGER reject_encrypted BEFORE `+operation+` ON `+table+` BEGIN SELECT RAISE(FAIL, 'test'); END`)
			client, event := encryptedEditEvent(t, reaction, "updated", 300000)
			if handleMessage(client, s, event, waLog.Noop) {
				t.Fatal("failed write acknowledged")
			}
		})
	}
}

func TestHistoryProtocolOrdering(t *testing.T) {
	for _, before := range []bool{false, true} {
		for _, split := range []bool{false, true} {
			for _, revoke := range []bool{false, true} {
				t.Run(fmt.Sprintf("before=%v/split=%v/revoke=%v", before, split, revoke), func(t *testing.T) {
					s := setupChatStore(t)
					original := originalEditMessage()
					action := historyEdit("action", "latest", 400000, revoke)
					messages := []*waProto.HistorySyncMsg{original, action}
					if before {
						messages[0], messages[1] = action, original
					}
					if split {
						syncHistory(s, nil, messages[0])
						// A later chunk may arrive after reopening the database.
						reopened, err := NewMessageStore()
						if err != nil {
							t.Fatal(err)
						}
						defer reopened.Close()
						syncHistory(reopened, nil, messages[1])
					} else {
						syncHistory(s, nil, messages...)
					}
					want := "latest"
					if revoke {
						want = revokedContent
					}
					editContent(t, s, want)
					syncHistory(s, nil, historyEdit("older", "stale", 300000, false), original)
					editContent(t, s, want)
					var last time.Time
					if err := s.db.QueryRow(`SELECT last_message_time FROM chats WHERE jid='123-456@g.us'`).Scan(&last); err != nil || last.Unix() != 100 {
						t.Fatalf("chat time = %v, %v", last, err)
					}
					// An edit after a revoke cannot restore the message, even if newer.
					if revoke {
						syncHistory(s, nil, historyEdit("newer", "resurrected", 500000, false))
						editContent(t, s, revokedContent)
					}
					var count int
					if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 1 {
						t.Fatalf("timeline count = %d, %v", count, err)
					}
				})
			}
		}
	}
}

func TestHistoryProtocolAuthorAndAudio(t *testing.T) {
	s := setupChatStore(t)
	forged := historyEdit("forged", "forged", 900000, true)
	forged.Message.Participant = proto.String("222@s.whatsapp.net")
	syncHistory(s, nil, forged, historyEdit("valid", "latest", 300000, false), originalEditMessage())
	editContent(t, s, "latest")
	empty := historyEdit("empty", "", 400000, false)
	syncHistory(s, nil, empty)
	editContent(t, s, "latest")
	audio := originalEditMessage()
	audio.Message.Message = &waProto.Message{AudioMessage: &waProto.AudioMessage{URL: proto.String("url")}}
	syncHistory(s, nil, audio)
	// A local transcription must survive both edits and repeated history deliveries.
	mustExec(t, s, `UPDATE messages SET content='transcription' WHERE id='target'`)
	syncHistory(s, nil, historyEdit("audio-edit", "caption", 500000, false), audio)
	editContent(t, s, "transcription")
	syncHistory(s, nil, historyEdit("revoke", "", 600000, true), audio)
	editContent(t, s, revokedContent)
}

func TestLiveEditSurvivesHistory(t *testing.T) {
	s := setupChatStore(t)
	syncHistory(s, nil, originalEditMessage())
	client, event := encryptedEditEvent(t, false, "live latest", 400001)
	if !handleMessage(client, s, event, waLog.Noop) {
		t.Fatal("live edit failed")
	}
	syncHistory(s, nil, historyEdit("older", "old", 400000, false), originalEditMessage())
	editContent(t, s, "live latest")
	// Protocol timestamps take precedence over delivery time, including milliseconds.
	event.Info.Timestamp = time.Unix(900, 0)
	event.Message = historyEdit("fallback", "fallback", 0, false).Message.Message.GetEditedMessage().GetMessage()
	if !handleMessage(client, s, event, waLog.Noop) {
		t.Fatal("fallback edit failed")
	}
	syncHistory(s, nil, originalEditMessage(), historyEdit("older", "old", 800000, false))
	editContent(t, s, "fallback")
}

func TestPendingEditTransactionRollsBack(t *testing.T) {
	s := setupChatStore(t)
	syncHistory(s, nil, originalEditMessage())
	client, event := encryptedEditEvent(t, false, "updated", 300000)
	mustExec(t, s, `CREATE TRIGGER reject_projection BEFORE UPDATE ON messages BEGIN SELECT RAISE(FAIL, 'test'); END`)
	if handleMessage(client, s, event, waLog.Noop) {
		t.Fatal("failed projection acknowledged")
	}
	mustExec(t, s, `DROP TRIGGER reject_projection`)
	syncHistory(s, nil, originalEditMessage())
	editContent(t, s, "original")
	if !handleMessage(client, s, event, waLog.Noop) {
		t.Fatal("retry failed")
	}
	editContent(t, s, "updated")
}

func TestHistoryOwnProtocolAndMissingAuthor(t *testing.T) {
	for _, own := range []bool{false, true} {
		t.Run(fmt.Sprint(own), func(t *testing.T) {
			s := setupChatStore(t)
			original := originalEditMessage()
			original.Message.Key.FromMe = proto.Bool(own)
			action := historyEdit("action", "edited", 300000, false)
			action.Message.Key.FromMe = proto.Bool(own)
			action.Message.Participant = nil
			action.Message.Key.Participant = nil
			syncHistory(s, nil, action, original)
			want := "original"
			if own {
				want = "edited"
			}
			editContent(t, s, want)
		})
	}
}

func TestEditsFollowLIDMigration(t *testing.T) {
	s := setupChatStore(t)
	lid, pn := "999@lid", "222@s.whatsapp.net"
	// The original has not arrived under either chat key yet.
	if err := s.ApplyEdit("target", lid, "lid latest", false, "888@lid"); err != nil {
		t.Fatal(err)
	}
	migrateLIDChats(lidClient(map[string]string{"999": "222", "888": "333"}), s, waLog.Noop)
	if err := s.EnsureChat(pn, time.Unix(100, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("target", pn, "333", "original", time.Unix(100, 0), false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	var content string
	if err := s.db.QueryRow(`SELECT content FROM messages WHERE chat_jid=?`, pn).Scan(&content); err != nil || content != "lid latest" {
		t.Fatalf("migrated content = %q, %v", content, err)
	}
	// Both keys have an edit: the newer LID edit must win the merge.
	mustExec(t, s, `DELETE FROM message_edits`)
	if err := s.ApplyEdit("target", pn, "pn older", false, "333", time.Unix(200, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyEdit("target", lid, "lid newer", false, "888@lid", time.Unix(300, 0)); err != nil {
		t.Fatal(err)
	}
	migrateLIDChats(lidClient(map[string]string{"999": "222", "888": "333"}), s, waLog.Noop)
	if err := s.db.QueryRow(`SELECT content FROM messages WHERE chat_jid=?`, pn).Scan(&content); err != nil || content != "lid newer" {
		t.Fatalf("merged content = %q, %v", content, err)
	}
}

func TestForgetChatRemovesPendingEdits(t *testing.T) {
	s := setupChatStore(t)
	syncHistory(s, nil, historyEdit("edit", "stale edit", 300000, false))
	if err := forgetChatLocally(s.db, "123-456@g.us"); err != nil {
		t.Fatal(err)
	}
	syncHistory(s, nil, originalEditMessage())
	editContent(t, s, "original")
}

func TestEncryptedBareEditUsesTarget(t *testing.T) {
	s := setupChatStore(t)
	syncHistory(s, nil, originalEditMessage())
	client, event := encryptedEditEvent(t, false, "bare edit", 300000, true)
	if !handleMessage(client, s, event, waLog.Noop) {
		t.Fatal("bare edit failed")
	}
	editContent(t, s, "bare edit")
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("timeline count = %d, %v", count, err)
	}
}

func TestEditSchemaUpgradePreservesHistory(t *testing.T) {
	s := setupChatStore(t)
	syncHistory(s, nil, originalEditMessage())
	mustExec(t, s, `DROP TABLE message_edits`)
	upgraded, err := NewMessageStore()
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	editContent(t, upgraded, "original")
	syncHistory(upgraded, nil, historyEdit("upgrade-edit", "edited", 300000, false))
	editContent(t, upgraded, "edited")
}

func TestEncryptedPollEditIsNotStoredAsAMessage(t *testing.T) {
	s := setupChatStore(t)
	syncHistory(s, nil, originalEditMessage())
	secret := []byte("01234567890123456789012345678901")
	target := &waProto.MessageKey{ID: proto.String("target"), Participant: proto.String("111@s.whatsapp.net")}
	plaintext, err := proto.Marshal(&waProto.Message{Conversation: proto.String("edited poll")})
	if err != nil {
		t.Fatal(err)
	}
	key := hkdfutil.SHA256(secret, nil, []byte("target111@s.whatsapp.net111@s.whatsapp.net"+string(whatsmeow.EncSecretPollEdit)), 32)
	iv := []byte("012345678901")
	ciphertext, err := gcmutil.Encrypt(key, iv, plaintext, nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &whatsmeow.Client{Store: &store.Device{MsgSecrets: editSecrets{secret: secret}}}
	event := typeEvent("encrypted", "111", 200, &waProto.Message{SecretEncryptedMessage: &waProto.SecretEncryptedMessage{
		SecretEncType: waE2E.SecretEncryptedMessage_POLL_EDIT.Enum(), TargetMessageKey: target, EncPayload: ciphertext, EncIV: iv,
	}})
	if !handleMessage(client, s, event, waLog.Noop) {
		t.Fatal("poll edit not acknowledged")
	}
	editContent(t, s, "original")
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("timeline count = %d, %v", count, err)
	}
}

func TestRevokedWithoutRecordedEditSurvivesResync(t *testing.T) {
	s := setupChatStore(t)
	at := time.Unix(100, 0)
	if err := s.StoreChat("111@s.whatsapp.net", "", at); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("gone", "111@s.whatsapp.net", "111", "original", at, false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	mustExec(t, s, `UPDATE messages SET content=? WHERE id='gone'`, revokedContent)
	if err := s.StoreMessage("gone", "111@s.whatsapp.net", "111", "original", at, false, "", "", "", nil, nil, nil, 0); err != nil {
		t.Fatal(err)
	}
	var content string
	if err := s.db.QueryRow(`SELECT content FROM messages WHERE id='gone'`).Scan(&content); err != nil || content != revokedContent {
		t.Fatalf("content = %q, %v", content, err)
	}
}
