package main

import (
	"testing"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// TestHistoryUnwrap covers the extraction path handleHistorySync uses:
// unwrapHistoryMessage, then extractTextContent / extractMediaInfo. History
// sync delivers the raw message, still inside EphemeralMessage (disappearing
// messages) or DeviceSentMessage (sent from another of the user's devices);
// without the unwrap both extractors see an empty outer message and the entry
// is dropped. No SQLite needed.
func TestHistoryUnwrap(t *testing.T) {
	image := func() *waProto.ImageMessage {
		return &waProto.ImageMessage{
			Caption:       proto.String("ephemeral caption"),
			URL:           proto.String("https://example.invalid/media"),
			MediaKey:      []byte("mediakey"),
			FileSHA256:    []byte("filesha"),
			FileEncSHA256: []byte("fileencsha"),
			FileLength:    proto.Uint64(1234),
		}
	}

	t.Run("plain message is unchanged", func(t *testing.T) {
		m := &waProto.Message{Conversation: proto.String("plain")}
		inner := unwrapHistoryMessage(m)
		if got := extractTextContent(inner); got != "plain" {
			t.Fatalf("text = %q, want %q", got, "plain")
		}
	})

	t.Run("nil message", func(t *testing.T) {
		if got := unwrapHistoryMessage(nil); got != nil {
			t.Fatalf("unwrapHistoryMessage(nil) = %v, want nil", got)
		}
	})

	t.Run("text in ephemeral", func(t *testing.T) {
		inner := unwrapHistoryMessage(&waProto.Message{
			EphemeralMessage: &waProto.FutureProofMessage{
				Message: &waProto.Message{Conversation: proto.String("ephemeral text")},
			},
		})
		if got := extractTextContent(inner); got != "ephemeral text" {
			t.Fatalf("text = %q, want %q", got, "ephemeral text")
		}
	})

	t.Run("image with caption in ephemeral", func(t *testing.T) {
		inner := unwrapHistoryMessage(&waProto.Message{
			EphemeralMessage: &waProto.FutureProofMessage{
				Message: &waProto.Message{ImageMessage: image()},
			},
		})
		if got := extractTextContent(inner); got != "ephemeral caption" {
			t.Fatalf("caption = %q, want %q", got, "ephemeral caption")
		}
		mediaType, _, url, mediaKey, _, _, fileLength := extractMediaInfo(inner)
		if mediaType != "image" || url != "https://example.invalid/media" || string(mediaKey) != "mediakey" || fileLength != 1234 {
			t.Fatalf("media = %q %q %q %d, want image with its url, key and length", mediaType, url, mediaKey, fileLength)
		}
	})

	t.Run("text in device sent", func(t *testing.T) {
		inner := unwrapHistoryMessage(&waProto.Message{
			DeviceSentMessage: &waProto.DeviceSentMessage{
				Message: &waProto.Message{Conversation: proto.String("sent from phone")},
			},
		})
		if got := extractTextContent(inner); got != "sent from phone" {
			t.Fatalf("text = %q, want %q", got, "sent from phone")
		}
	})

	t.Run("image in device sent in ephemeral", func(t *testing.T) {
		// Wrappers nest: UnwrapRaw peels DeviceSent, then Ephemeral.
		inner := unwrapHistoryMessage(&waProto.Message{
			DeviceSentMessage: &waProto.DeviceSentMessage{
				Message: &waProto.Message{
					EphemeralMessage: &waProto.FutureProofMessage{
						Message: &waProto.Message{ImageMessage: image()},
					},
				},
			},
		})
		if mediaType, _, _, _, _, _, _ := extractMediaInfo(inner); mediaType != "image" {
			t.Fatalf("media type = %q, want image", mediaType)
		}
	})
}
