package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"go.mau.fi/whatsmeow/types"
)

type fakeGroupSettings struct {
	fail  string
	calls []string
	times []time.Time
}

func (f *fakeGroupSettings) IsConnected() bool { return true }
func (f *fakeGroupSettings) set(field string) error {
	f.calls = append(f.calls, field)
	f.times = append(f.times, time.Now())
	if f.fail == field {
		return errors.New("private bridge detail")
	}
	return nil
}
func (f *fakeGroupSettings) SetGroupName(context.Context, types.JID, string) error {
	return f.set("name")
}
func (f *fakeGroupSettings) SetGroupTopic(context.Context, types.JID, string, string, string) error {
	return f.set("topic")
}
func (f *fakeGroupSettings) SetGroupAnnounce(context.Context, types.JID, bool) error {
	return f.set("announce")
}
func (f *fakeGroupSettings) SetGroupLocked(context.Context, types.JID, bool) error {
	return f.set("locked")
}

func settingsRequest(t *testing.T, handler http.HandlerFunc, body string, status int, applied []string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest(http.MethodPost, "/api/group_settings", strings.NewReader(body)))
	if w.Code != status {
		t.Fatalf("status = %d, want %d: %s", w.Code, status, w.Body.String())
	}
	var result struct {
		Applied []string `json:"applied"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.Applied, applied) {
		t.Fatalf("applied = %v, want %v: %s", result.Applied, applied, w.Body.String())
	}
	return w
}

func TestGroupSettingsFirstContactOnce(t *testing.T) {
	store := setupChatStore(t)
	synctest.Test(t, func(t *testing.T) {
		useSendLimiter(t, newSendLimiter(250*time.Millisecond, 30*time.Second))
		client := &fakeGroupSettings{}
		body := `{"group_jid":"123@g.us","name":"test","topic":"topic","announce":false,"locked":false}`
		settingsRequest(t, handleGroupSettings(client, store), body, http.StatusOK, []string{"name", "topic", "announce", "locked"})
		if len(client.times) != 4 {
			t.Fatalf("calls = %v", client.calls)
		}
		for i := 1; i < len(client.times); i++ {
			if gap := client.times[i].Sub(client.times[i-1]); gap != 250*time.Millisecond {
				t.Fatalf("action gap = %v", gap)
			}
		}
		_, err := beginOutbound(context.Background(), store, types.EmptyJID)
		var limited *sendRateError
		if !errors.As(err, &limited) {
			t.Fatalf("another new contact bypassed first-contact interval: %v", err)
		}
	})
}

func TestGroupSettingsPartialFailureAndRetry(t *testing.T) {
	store := setupChatStore(t)
	synctest.Test(t, func(t *testing.T) {
		useSendLimiter(t, newSendLimiter(250*time.Millisecond, 30*time.Second))
		client := &fakeGroupSettings{fail: "topic"}
		handler := handleGroupSettings(client, store)
		body := `{"group_jid":"123@g.us","name":"test","topic":"topic","locked":true}`
		settingsRequest(t, handler, body, http.StatusInternalServerError, []string{"name"})
		if !reflect.DeepEqual(client.calls, []string{"name", "topic"}) {
			t.Fatalf("calls = %v", client.calls)
		}
		client.fail = ""
		w := settingsRequest(t, handler, body, http.StatusTooManyRequests, nil)
		if w.Header().Get("Retry-After") == "" || len(client.calls) != 2 {
			t.Fatalf("rejected retry sent an action or omitted Retry-After: %v", client.calls)
		}
		retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
		if err != nil || retry <= 0 {
			t.Fatalf("invalid Retry-After: %q", w.Header().Get("Retry-After"))
		}
		time.Sleep(time.Duration(retry) * time.Second)
		settingsRequest(t, handler, body, http.StatusOK, []string{"name", "topic", "locked"})
		if !reflect.DeepEqual(client.calls, []string{"name", "topic", "name", "topic", "locked"}) {
			t.Fatalf("retry calls = %v", client.calls)
		}
	})
}

func TestGroupSettingsPartialRateLimitAndRetryRemaining(t *testing.T) {
	store := setupChatStore(t)
	synctest.Test(t, func(t *testing.T) {
		useSendLimiter(t, newSendLimiter(11*time.Second, 30*time.Second))
		client := &fakeGroupSettings{}
		handler := handleGroupSettings(client, store)
		w := settingsRequest(t, handler, `{"group_jid":"123@g.us","name":"test","topic":"topic"}`, http.StatusTooManyRequests, []string{"name"})
		retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
		if err != nil || retry <= 0 {
			t.Fatalf("invalid Retry-After: %q", w.Header().Get("Retry-After"))
		}
		time.Sleep(time.Duration(retry) * time.Second)
		settingsRequest(t, handler, `{"group_jid":"123@g.us","topic":"topic"}`, http.StatusOK, []string{"topic"})
		if !reflect.DeepEqual(client.calls, []string{"name", "topic"}) {
			t.Fatalf("retry resent applied fields: %v", client.calls)
		}
	})
}
