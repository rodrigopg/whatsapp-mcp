package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"go.mau.fi/whatsmeow/types"
)

func knownChat(context.Context) (bool, error) { return false, nil }
func newChat(context.Context) (bool, error)   { return true, nil }

func TestSendRateKnownChat(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(250*time.Millisecond, 30*time.Second)
		start := time.Now()
		for i := range 4 {
			done, err := limiter.acquire(context.Background(), knownChat)
			if err != nil {
				t.Fatal(err)
			}
			if elapsed := time.Since(start); elapsed != time.Duration(i)*250*time.Millisecond {
				t.Fatalf("send %d at %v", i, elapsed)
			}
			done()
		}
	})
}

func TestSendRateEnv(t *testing.T) {
	t.Setenv("WHATSAPP_SEND_INTERVAL_MS", "")
	t.Setenv("WHATSAPP_FIRST_CONTACT_INTERVAL_MS", "")
	l, err := sendLimiterFromEnv()
	if err != nil || l.interval != 250*time.Millisecond || l.firstInterval != 30*time.Second {
		t.Fatalf("defaults: %v, %v", l, err)
	}
	t.Setenv("WHATSAPP_SEND_INTERVAL_MS", "0")
	t.Setenv("WHATSAPP_FIRST_CONTACT_INTERVAL_MS", "0")
	l, err = sendLimiterFromEnv()
	if err != nil || l.interval != 0 || l.firstInterval != 0 {
		t.Fatalf("zero: %v, %v", l, err)
	}
	for _, name := range []string{"WHATSAPP_SEND_INTERVAL_MS", "WHATSAPP_FIRST_CONTACT_INTERVAL_MS"} {
		for _, value := range []string{"-1", "oops", "1.5", "9223372036854775807"} {
			t.Setenv(name, value)
			if _, err := sendLimiterFromEnv(); err == nil {
				t.Fatalf("accepted %s=%s", name, value)
			}
		}
		t.Setenv(name, "0")
	}
}

func useSendLimiter(t *testing.T, limiter *sendLimiter) {
	t.Helper()
	previous := outboundLimiter
	outboundLimiter = limiter
	t.Cleanup(func() { outboundLimiter = previous })
}

func TestSendRateUsesMessagesNotChatRow(t *testing.T) {
	store := setupChatStore(t)
	chat := types.JID{User: "peer", Server: types.DefaultUserServer}
	if err := store.StoreChat(chat.String(), "peer", time.Now()); err != nil {
		t.Fatal(err)
	}
	limiter := newSendLimiter(0, time.Hour)
	useSendLimiter(t, limiter)
	done, err := beginOutbound(context.Background(), store, chat)
	if err != nil {
		t.Fatal(err)
	}
	done()
	_, err = beginOutbound(context.Background(), store, chat)
	var limited *sendRateError
	if !errors.As(err, &limited) {
		t.Fatalf("empty chat treated as known: %v", err)
	}
	seedActionMessage(t, store, "message", chat.String(), "peer", "hello", false)
	done, err = beginOutbound(context.Background(), store, chat)
	if err != nil {
		t.Fatalf("history did not make chat known: %v", err)
	}
	done()
	lid := types.JID{User: "peer-lid", Server: types.HiddenUserServer}
	done, err = beginOutbound(context.Background(), store, lid, chat)
	if err != nil {
		t.Fatalf("known alias treated as new contact: %v", err)
	}
	done()
	_, err = beginOutbound(context.Background(), store, types.EmptyJID)
	if !errors.As(err, &limited) {
		t.Fatalf("new group bypassed first-contact limit: %v", err)
	}
}

func TestSendRateActionsShareLimiter(t *testing.T) {
	limiter := newSendLimiter(time.Hour, 0)
	useSendLimiter(t, limiter)
	done, err := limiter.acquire(context.Background(), knownChat)
	if err != nil {
		t.Fatal(err)
	}
	done()
	fake := newFakeMessageActions()
	for _, action := range []string{"react", "revoke"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/"+action, strings.NewReader(`{"chat_jid":"peer@s.whatsapp.net","message_id":"id","from_me":true}`))
		groupActionHandler(action, fake, nil)(w, r)
		if w.Code != http.StatusTooManyRequests || fake.sent != 0 {
			t.Fatalf("%s bypassed limiter: status=%d, sent=%d", action, w.Code, fake.sent)
		}
	}
}

func TestSendRateQueuedChatRechecked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(250*time.Millisecond, time.Hour)
		known := false
		check := func(context.Context) (bool, error) { return !known, nil }
		done, err := limiter.acquire(context.Background(), check)
		if err != nil {
			t.Fatal(err)
		}
		result := make(chan error, 1)
		go func() {
			finish, err := limiter.acquire(context.Background(), check)
			if err == nil {
				finish()
			}
			result <- err
		}()
		synctest.Wait()
		known = true
		done()
		if err := <-result; err != nil {
			t.Fatalf("queued reply treated as a new contact: %v", err)
		}
	})
}

func TestSendRateFirstContact(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(250*time.Millisecond, 2*time.Second)
		start := time.Now()
		for i, check := range []func(context.Context) (bool, error){newChat, knownChat, newChat, knownChat} {
			done, err := limiter.acquire(context.Background(), check)
			if err != nil {
				t.Fatal(err)
			}
			want := []time.Duration{0, 250 * time.Millisecond, 2 * time.Second, 2250 * time.Millisecond}[i]
			if got := time.Since(start); got != want {
				t.Fatalf("send %d at %v, want %v", i, got, want)
			}
			done()
		}
	})
}

func TestSendRateLongQueue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(250*time.Millisecond, 30*time.Second)
		done, err := limiter.acquire(context.Background(), newChat)
		if err != nil {
			t.Fatal(err)
		}
		done()
		_, err = limiter.acquire(context.Background(), newChat)
		var limited *sendRateError
		if !errors.As(err, &limited) || limited.retryAfter != 30*time.Second {
			t.Fatalf("got %v, want 30s retry", err)
		}
		w := httptest.NewRecorder()
		writeActionError(w, err)
		if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "30" {
			t.Fatalf("status %d, retry %q", w.Code, w.Header().Get("Retry-After"))
		}
		// Rejection must not consume a slot for ordinary replies.
		done, err = limiter.acquire(context.Background(), knownChat)
		if err != nil {
			t.Fatal(err)
		}
		done()
	})
}

func TestSendRateZero(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, tc := range []struct {
			interval, first time.Duration
			check           func(context.Context) (bool, error)
		}{
			{0, 0, newChat}, {0, time.Second, knownChat},
		} {
			limiter := newSendLimiter(tc.interval, tc.first)
			start := time.Now()
			for range 5 {
				done, err := limiter.acquire(context.Background(), tc.check)
				if err != nil {
					t.Fatal(err)
				}
				done()
			}
			if time.Since(start) != 0 {
				t.Fatal("disabled interval delayed a send")
			}
		}
		limiter := newSendLimiter(250*time.Millisecond, 0)
		start := time.Now()
		for range 3 {
			done, err := limiter.acquire(context.Background(), newChat)
			if err != nil {
				t.Fatal(err)
			}
			done()
		}
		if time.Since(start) != 500*time.Millisecond {
			t.Fatal("zero first-contact interval changed the global gap")
		}
	})
}

func TestSendRateConcurrent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(time.Second, 0)
		start := time.Now()
		times := make(chan time.Duration, 5)
		var wg sync.WaitGroup
		for range 5 {
			wg.Go(func() {
				done, err := limiter.acquire(context.Background(), knownChat)
				if err != nil {
					t.Error(err)
					return
				}
				times <- time.Since(start)
				done()
			})
		}
		wg.Wait()
		for i := range 5 {
			if got := <-times; got != time.Duration(i)*time.Second {
				t.Fatalf("send %d at %v", i, got)
			}
		}
	})
}

func TestSendRateGapAfterSlowSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(250*time.Millisecond, 0)
		done, err := limiter.acquire(context.Background(), knownChat)
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		done()
		start := time.Now()
		done, err = limiter.acquire(context.Background(), knownChat)
		if err != nil {
			t.Fatal(err)
		}
		if time.Since(start) != 250*time.Millisecond {
			t.Fatal("slow send allowed the next send to bunch up")
		}
		done()
	})
}

func TestSendRateCancellationAndQueueTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(time.Second, 0)
		done, err := limiter.acquire(context.Background(), knownChat)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() { _, err := limiter.acquire(ctx, knownChat); result <- err }()
		synctest.Wait()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		start := time.Now()
		_, err = limiter.acquire(context.Background(), knownChat)
		var limited *sendRateError
		if !errors.As(err, &limited) || time.Since(start) != maxSendWait {
			t.Fatalf("got %v after %v", err, time.Since(start))
		}
		done()
		ctx, cancel = context.WithCancel(context.Background())
		go func() { _, err := limiter.acquire(ctx, knownChat); result <- err }()
		synctest.Wait()
		cancel()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
		done, err = limiter.acquire(context.Background(), knownChat)
		if err != nil {
			t.Fatal(err)
		}
		done()
	})
}

func TestSendRateRequestDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(time.Second, 0)
		done, err := limiter.acquire(context.Background(), knownChat)
		if err != nil {
			t.Fatal(err)
		}
		done()
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		start := time.Now()
		_, err = limiter.acquire(ctx, knownChat)
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 100*time.Millisecond {
			t.Fatalf("got %v after %v, want request deadline", err, time.Since(start))
		}
	})
}
