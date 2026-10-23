package main

import (
	"testing"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

// A reply that also @-mentions must keep the quote while adding the mention.
func TestWithMentionsKeepsReplyQuote(t *testing.T) {
	msg := &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
		Text:        proto.String("@5511999990000 ok"),
		ContextInfo: &waProto.ContextInfo{StanzaID: proto.String("ABC")},
	}}
	if err := withMentions(msg, "@5511999990000 ok", []string{"5511999990000@s.whatsapp.net"}); err != nil {
		t.Fatal(err)
	}
	ci := msg.GetExtendedTextMessage().GetContextInfo()
	if ci.GetStanzaID() != "ABC" || len(ci.GetMentionedJID()) != 1 {
		t.Fatalf("quote or mention lost: %v", ci)
	}
}
