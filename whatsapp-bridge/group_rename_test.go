package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

func TestHandleGroupInfoRename(t *testing.T) {
	store := setupChatStore(t)
	group := types.NewJID("120363000000000001", types.GroupServer)
	if err := store.StoreChat(group.String(), "Old name", time.Now()); err != nil {
		t.Fatal(err)
	}
	name := func(jid string) string {
		var s string
		if err := store.db.QueryRow("SELECT name FROM chats WHERE jid=?", jid).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	log := waLog.Noop

	handleGroupInfo(store, &events.GroupInfo{JID: group}, log)
	if got := name(group.String()); got != "Old name" {
		t.Errorf("non-name event changed name to %q", got)
	}

	handleGroupInfo(store, &events.GroupInfo{JID: group, Name: &types.GroupName{Name: "New name"}}, log)
	if got := name(group.String()); got != "New name" {
		t.Errorf("name = %q, want %q", got, "New name")
	}
	if got := GetChatName(nil, store, group, group.String(), nil, "", log); got != "New name" {
		t.Errorf("GetChatName = %q, want %q", got, "New name")
	}

	if changed, _ := store.updateGroupName(group.String(), ""); changed {
		t.Error("empty name must not overwrite")
	}
	if changed, _ := store.updateGroupName(group.String(), "New name"); changed {
		t.Error("same name reported as changed")
	}
	other := types.NewJID("120363000000000002", types.GroupServer).String()
	if changed, _ := store.updateGroupName(other, "X"); changed {
		t.Error("unknown group must not be inserted")
	}
}
