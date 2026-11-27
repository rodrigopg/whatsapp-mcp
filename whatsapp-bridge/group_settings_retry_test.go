package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestGroupSettingsRetryAfterCoversNextRequest(t *testing.T) {
	for _, known := range []bool{false, true} {
		for _, interval := range []time.Duration{9 * time.Second, 11 * time.Second} {
			t.Run(strconv.FormatBool(known)+"/"+interval.String(), func(t *testing.T) {
				store := setupChatStore(t)
				if known {
					seedActionMessage(t, store, "message", "123@g.us", "peer", "hello", false)
				}
				synctest.Test(t, func(t *testing.T) {
					useSendLimiter(t, newSendLimiter(interval, 30*time.Second))
					handler := handleGroupSettings(&fakeGroupSettings{}, store)
					applied := []string{"name"}
					if interval == 9*time.Second {
						applied = append(applied, "topic")
					}
					w := settingsRequest(t, handler, `{"group_jid":"123@g.us","name":"test","topic":"topic","locked":true}`, http.StatusTooManyRequests, applied)
					retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
					if err != nil || retry <= 0 {
						t.Fatalf("invalid Retry-After: %q", w.Header().Get("Retry-After"))
					}
					if known && retry > int(interval.Seconds()) {
						t.Fatalf("known group delayed by first-contact interval: %ds", retry)
					}
					time.Sleep(time.Duration(retry) * time.Second)
					settingsRequest(t, handler, `{"group_jid":"123@g.us","locked":true}`, http.StatusOK, []string{"locked"})
				})
			})
		}
	}
}

func TestGroupSettingsPartialMessageKeepsAppliedFields(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			store := setupChatStore(t)
			synctest.Test(t, func(t *testing.T) {
				interval := 11 * time.Second
				client := &fakeGroupSettings{}
				if status == http.StatusInternalServerError {
					interval = 250 * time.Millisecond
					client.fail = "topic"
				}
				useSendLimiter(t, newSendLimiter(interval, 30*time.Second))
				w := settingsRequest(t, handleGroupSettings(client, store), `{"group_jid":"123@g.us","name":"test","topic":"topic"}`, status, []string{"name"})
				var result GroupActionResponse
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(result.Message, "applied: name") {
					t.Fatalf("partial message omitted applied fields: %q", result.Message)
				}
			})
		})
	}
}
