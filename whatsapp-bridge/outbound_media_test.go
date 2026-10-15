package main

import (
	"bytes"
	"testing"
	"time"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// TestPersistOutboundStoresMedia pins the contract behind transcribing (and
// downloading) your own sent media: the row written when we send must carry
// media_type and media_key, not just text.
func TestPersistOutboundStoresMedia(t *testing.T) {
	store := setupChatStore(t)

	const chat = "contact@s.whatsapp.net"
	key := []byte{1, 2, 3, 4}
	msg := &waProto.Message{AudioMessage: &waProto.AudioMessage{
		URL:           proto.String("https://mmg.whatsapp.net/x"),
		MediaKey:      key,
		FileSHA256:    []byte{5},
		FileEncSHA256: []byte{6},
		FileLength:    proto.Uint64(42),
	}}
	persistOutbound(store, chat, "me", "OUT1", "", time.Now(), msg, nil)

	var mediaType string
	var mediaKey []byte
	var fromMe bool
	err := store.db.QueryRow(
		"SELECT media_type, media_key, is_from_me FROM messages WHERE id = ? AND chat_jid = ?",
		"OUT1", chat).Scan(&mediaType, &mediaKey, &fromMe)
	if err != nil {
		t.Fatalf("outbound media row not stored: %v", err)
	}
	if mediaType != "audio" {
		t.Errorf("media_type = %q, want audio", mediaType)
	}
	if !bytes.Equal(mediaKey, key) {
		t.Errorf("media_key = %v, want %v", mediaKey, key)
	}
	if !fromMe {
		t.Error("is_from_me should be true")
	}
}
