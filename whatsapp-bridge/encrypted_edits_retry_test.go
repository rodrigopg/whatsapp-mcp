package main

import (
	"errors"
	"fmt"
	"testing"
	"time"

	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func TestEncryptedSecretStoreFailureRetriesAfterRecovery(t *testing.T) {
	for _, reaction := range []bool{false, true} {
		t.Run(fmt.Sprint(reaction), func(t *testing.T) {
			s := setupChatStore(t)
			syncHistory(s, nil, originalEditMessage())
			client, event := encryptedEditEvent(t, reaction, "updated", 300001)
			raw := proto.Clone(event.Message)
			secrets := client.Store.MsgSecrets
			storeErr := errors.New("temporary store failure")
			client.Store.MsgSecrets = editSecrets{err: storeErr}
			if handleMessage(client, s, event, waLog.Noop) {
				t.Fatal("secret store failure acknowledged")
			}
			if client.Store.MsgSecrets.(editSecrets).err != storeErr {
				t.Fatal("shared secret store changed")
			}
			editContent(t, s, "original")
			var count int
			if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM reactions) + (SELECT COUNT(*) FROM message_edits)`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("actions after failure = %d, %v", count, err)
			}
			client.Store.MsgSecrets = secrets
			for range 2 {
				if !handleMessage(client, s, event, waLog.Noop) {
					t.Fatal("retry after secret store recovery failed")
				}
			}
			if !proto.Equal(event.Message, raw) {
				t.Fatal("encrypted envelope changed during retry")
			}
			if reaction {
				var text string
				var at time.Time
				if err := s.db.QueryRow(`SELECT emoji, timestamp FROM reactions WHERE message_id='target' AND sender='111'`).Scan(&text, &at); err != nil || text != "updated" || at.UnixMilli() != 300001 {
					t.Fatalf("retried reaction = %q %v, %v", text, at, err)
				}
				editContent(t, s, "original")
			} else {
				editContent(t, s, "updated")
			}
			if err := s.db.QueryRow(`SELECT (SELECT COUNT(*) FROM reactions) + (SELECT COUNT(*) FROM message_edits)`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("actions after retry = %d, %v", count, err)
			}
		})
	}
}
