package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestGroupCreateFirstContactOnlyForNewParticipants(t *testing.T) {
	store := setupChatStore(t)
	known := types.JID{User: "known", Server: types.DefaultUserServer}
	stranger := types.JID{User: "stranger", Server: types.DefaultUserServer}
	seedActionMessage(t, store, "message", known.String(), "known", "hello", false)
	useSendLimiter(t, newSendLimiter(0, time.Hour))

	done, err := beginOutboundTo(context.Background(), nil, store, []types.JID{stranger})
	if err != nil {
		t.Fatal(err)
	}
	done()
	done, err = beginOutboundTo(context.Background(), nil, store, []types.JID{known})
	if err != nil {
		t.Fatalf("group with a known participant waited for first contact: %v", err)
	}
	done()
	_, err = beginOutboundTo(context.Background(), nil, store, []types.JID{known, stranger})
	var limited *sendRateError
	if !errors.As(err, &limited) {
		t.Fatalf("group with a new participant bypassed first-contact interval: %v", err)
	}
}
