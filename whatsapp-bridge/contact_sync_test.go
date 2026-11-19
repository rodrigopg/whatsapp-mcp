package main

import (
	"context"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	waLog "go.mau.fi/whatsmeow/util/log"
)

type fakeContacts struct {
	store.ContactStore
	all map[types.JID]types.ContactInfo
}

func (f fakeContacts) GetAllContacts(context.Context) (map[types.JID]types.ContactInfo, error) {
	return f.all, nil
}

func TestSyncAllContactsInOneTransaction(t *testing.T) {
	s := setupChatStore(t)
	known := types.NewJID("5511900000001", types.DefaultUserServer)
	if err := s.StoreSender(known.String(), "", "Known Full", "", ""); err != nil {
		t.Fatal(err)
	}
	fresh := types.NewJID("5511900000002", types.DefaultUserServer)
	client := &whatsmeow.Client{Store: &store.Device{Contacts: fakeContacts{all: map[types.JID]types.ContactInfo{
		known: {PushName: "Known Push"},
		fresh: {FullName: "Fresh Full"},
	}}}}

	SyncAllContacts(client, s, waLog.Noop)

	get := func(jid string) (push, full string) {
		if err := s.db.QueryRow("SELECT COALESCE(push_name,''), COALESCE(full_name,'') FROM senders WHERE jid=?", jid).Scan(&push, &full); err != nil {
			t.Fatalf("%s: %v", jid, err)
		}
		return
	}
	if push, full := get(known.String()); push != "Known Push" || full != "Known Full" {
		t.Errorf("known contact = (%q, %q), want push updated and full name kept", push, full)
	}
	if _, full := get(fresh.String()); full != "Fresh Full" {
		t.Errorf("new contact full name = %q", full)
	}
}
