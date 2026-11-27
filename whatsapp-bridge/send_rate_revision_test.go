package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func TestSendRateContactHistoryBothAddresses(t *testing.T) {
	pn := types.JID{User: "peer", Server: types.DefaultUserServer}
	lid := types.JID{User: "peer-lid", Server: types.HiddenUserServer}
	for _, tc := range []struct {
		name       string
		from, next types.JID
	}{
		{"LID to PN", lid, pn},
		{"PN to LID", pn, lid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := setupChatStore(t)
			useSendLimiter(t, newSendLimiter(0, time.Hour))
			client := newFakeMessageActions()
			client.Store.LIDs = fakeLIDs{pairs: map[types.JID]types.JID{pn: lid}}
			done, err := beginOutbound(context.Background(), store, client.NormalizeChat(tc.from), tc.from)
			if err != nil {
				t.Fatal(err)
			}
			// Contact cards and polls retain the address used to send them.
			seedActionMessage(t, store, "sent", tc.from.String(), "111", "[contact] peer", true)
			done()

			w := httptest.NewRecorder()
			body := fmt.Sprintf(`{"chat_jid":%q,"message_id":"sent","emoji":"ok"}`, tc.next.String())
			handleReact(client, store)(w, httptest.NewRequest(http.MethodPost, "/api/react", strings.NewReader(body)))
			if w.Code != http.StatusOK || client.sent != 1 {
				t.Fatalf("known contact rejected: status=%d sent=%d body=%s", w.Code, client.sent, w.Body.String())
			}
		})
	}
}

func TestGroupSettingsSharedWaitBudget(t *testing.T) {
	for _, queued := range []bool{false, true} {
		t.Run(fmt.Sprintf("queued=%t", queued), func(t *testing.T) {
			store := setupChatStore(t)
			synctest.Test(t, func(t *testing.T) {
				limiter := newSendLimiter(9*time.Second, 0)
				useSendLimiter(t, limiter)
				if queued {
					done, err := limiter.acquire(context.Background(), knownChat)
					if err != nil {
						t.Fatal(err)
					}
					done()
				}
				client := &fakeGroupSettings{}
				start := time.Now()
				wantApplied := []string{"name", "topic"}
				if queued {
					wantApplied = []string{"name"}
				}
				body := `{"group_jid":"123@g.us","name":"test","topic":"topic","announce":false,"locked":false}`
				w := settingsRequest(t, handleGroupSettings(client, store), body, http.StatusTooManyRequests, wantApplied)
				if elapsed := time.Since(start); elapsed > maxSendWait {
					t.Fatalf("request waited %v, limit %v", elapsed, maxSendWait)
				}
				if !reflect.DeepEqual(client.calls, wantApplied) {
					t.Fatalf("calls = %v, want %v", client.calls, wantApplied)
				}
				if retry := w.Header().Get("Retry-After"); retry != "8" && retry != "9" {
					t.Fatalf("Retry-After = %q, want remaining interval", retry)
				}
			})
		})
	}
}
