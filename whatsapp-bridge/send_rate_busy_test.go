package main

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestSendRateBusyQueueRetryCoversFirstContact(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(250*time.Millisecond, 30*time.Second)
		done, err := limiter.acquire(context.Background(), newChat)
		if err != nil {
			t.Fatal(err)
		}
		done()
		slow, err := limiter.acquire(context.Background(), knownChat)
		if err != nil {
			t.Fatal(err)
		}
		defer slow()
		_, err = limiter.acquire(context.Background(), newChat)
		var limited *sendRateError
		if !errors.As(err, &limited) {
			t.Fatalf("err = %v, want rate limit", err)
		}
		// Queued 0.25s..10.25s; the next first contact is allowed at 30s.
		if want := 30*time.Second - 250*time.Millisecond - maxSendWait; limited.retryAfter != want {
			t.Fatalf("Retry-After = %v, want %v", limited.retryAfter, want)
		}
	})
}

func TestSendRateUnknownContactRetryIsConservative(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limiter := newSendLimiter(250*time.Millisecond, 30*time.Second)
		done, err := limiter.acquire(context.Background(), newChat)
		if err != nil {
			t.Fatal(err)
		}
		done()
		_, err = limiter.acquire(context.Background(), func(ctx context.Context) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		})
		var limited *sendRateError
		if !errors.As(err, &limited) || limited.retryAfter != 30*time.Second-maxSendWait {
			t.Fatalf("err = %v, want Retry-After covering the first-contact gap", err)
		}
	})
}
