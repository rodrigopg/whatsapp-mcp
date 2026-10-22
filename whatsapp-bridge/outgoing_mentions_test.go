package main

import (
	"reflect"
	"testing"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestWithMentions(t *testing.T) {
	text := "@5511999990000 and @123456789, please check"
	jids := []string{"5511999990000@s.whatsapp.net", "123456789@lid"}
	msg := &waProto.Message{Conversation: proto.String(text)}
	if err := withMentions(msg, text, append(jids, jids[0])); err != nil {
		t.Fatal(err)
	}
	if msg.Conversation != nil || msg.GetExtendedTextMessage().GetText() != text {
		t.Fatalf("text not moved to an extended text message: %v", msg)
	}
	if got := mentionedJIDs(msg); !reflect.DeepEqual(got, jids) {
		t.Fatalf("mentioned = %v, want %v", got, jids)
	}

	t.Run("keeps_existing_context", func(t *testing.T) {
		msg := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String(text), ContextInfo: &waProto.ContextInfo{StanzaID: proto.String("Q1")},
		}}
		if err := withMentions(msg, text, jids); err != nil {
			t.Fatal(err)
		}
		if ctx := msg.GetExtendedTextMessage().GetContextInfo(); ctx.GetStanzaID() != "Q1" || len(ctx.GetMentionedJID()) != 2 {
			t.Fatalf("context = %v", ctx)
		}
	})

	t.Run("refuses_non_user_jids", func(t *testing.T) {
		for _, jid := range []string{"", "123456789", "123456789@g.us", "123456789@broadcast", "123456789:2@s.whatsapp.net", "123456789:0@s.whatsapp.net",
			"123456789.1@s.whatsapp.net", "123456789@lid@extra", "123456789@@lid", "abc@s.whatsapp.net"} {
			if err := withMentions(&waProto.Message{}, "@123456789 hi", []string{jid}); err == nil {
				t.Errorf("accepted %q", jid)
			}
		}
	})

	t.Run("refuses_a_mention_missing_from_the_text", func(t *testing.T) {
		for _, text := range []string{"please check", "@1234567890 please check", "@123456789abc please check", "@Maria please check"} {
			if err := withMentions(&waProto.Message{}, text, []string{"123456789@s.whatsapp.net"}); err == nil {
				t.Errorf("accepted %q", text)
			}
		}
	})
}
