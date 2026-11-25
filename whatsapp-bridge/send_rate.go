package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const maxSendWait = 10 * time.Second

// 250ms keeps replies fluid; 30s separates new contacts without claiming a WhatsApp-safe threshold.
const defaultSendInterval, defaultFirstContactInterval = 250 * time.Millisecond, 30 * time.Second

var outboundLimiter = newSendLimiter(0, 0) // Configured once before the REST server starts.

type sendRateError struct{ retryAfter time.Duration }

func (e *sendRateError) Error() string {
	return fmt.Sprintf("Outbound rate limit reached; retry in %d seconds", retrySeconds(e.retryAfter))
}

func retrySeconds(d time.Duration) int { return max(1, int(math.Ceil(d.Seconds()))) }

type sendLimiter struct {
	gate                    chan struct{}
	interval, firstInterval time.Duration
	mu                      sync.Mutex
	next, nextFirst         time.Time
}

func newSendLimiter(interval, first time.Duration) *sendLimiter {
	return &sendLimiter{gate: make(chan struct{}, 1), interval: interval, firstInterval: first}
}

func sendLimiterFromEnv() (*sendLimiter, error) {
	values := []time.Duration{defaultSendInterval, defaultFirstContactInterval}
	for i, name := range []string{"WHATSAPP_SEND_INTERVAL_MS", "WHATSAPP_FIRST_CONTACT_INTERVAL_MS"} {
		if raw := os.Getenv(name); raw != "" {
			ms, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || ms < 0 || ms > math.MaxInt64/int64(time.Millisecond) {
				return nil, fmt.Errorf("%s must be a nonnegative integer in milliseconds", name)
			}
			values[i] = time.Duration(ms) * time.Millisecond
		}
	}
	return newSendLimiter(values[0], values[1]), nil
}

func (l *sendLimiter) retry(first bool) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	next := l.next
	if first && l.nextFirst.After(next) {
		next = l.nextFirst
	}
	return max(0, time.Until(next))
}

// The queue stayed busy past the wait budget; Retry-After must still cover first-contact pacing.
func (l *sendLimiter) busy(ctx context.Context, firstContact func(context.Context) (bool, error)) error {
	if l.firstInterval == 0 {
		return &sendRateError{l.retry(false)}
	}
	checkCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	first, err := firstContact(checkCtx)
	return &sendRateError{l.retry(first || err != nil)}
}

// Keep the turn through sending and persistence: a parallel request must see the first send in history.
func (l *sendLimiter) acquire(ctx context.Context, firstContact func(context.Context) (bool, error)) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if l.interval == 0 && l.firstInterval == 0 {
		return func() {}, nil
	}
	waitDeadline := time.Now().Add(maxSendWait)
	waitCtx, cancel := context.WithDeadline(ctx, waitDeadline)
	defer cancel()
	select {
	case l.gate <- struct{}{}:
	case <-waitCtx.Done():
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, l.busy(ctx, firstContact)
	}
	release := func() { <-l.gate }
	first := false
	if l.firstInterval > 0 {
		var err error
		first, err = firstContact(waitCtx)
		if err != nil {
			release()
			if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
				return nil, &sendRateError{l.retry(true)} // unknown contact: assume first
			}
			return nil, err
		}
	}
	delay := l.retry(first)
	if delay > time.Until(waitDeadline) {
		release()
		return nil, &sendRateError{delay}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-waitCtx.Done():
		release()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &sendRateError{l.retry(first)}
	}
	if err := ctx.Err(); err != nil {
		release()
		return nil, err
	}
	return func() {
		l.mu.Lock()
		l.next = time.Now().Add(l.interval)
		if first {
			l.nextFirst = time.Now().Add(l.firstInterval)
		}
		l.mu.Unlock()
		release()
	}, nil
}

func outboundChatJIDs(ctx context.Context, client *whatsmeow.Client, chat types.JID) []types.JID {
	chat = chat.ToNonAD()
	chats := []types.JID{chat}
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return chats
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var alias types.JID
	var err error
	switch chat.Server {
	case types.DefaultUserServer:
		alias, err = client.Store.LIDs.GetLIDForPN(ctx, chat)
	case types.HiddenUserServer:
		alias, err = client.Store.LIDs.GetPNForLID(ctx, chat)
	}
	alias = alias.ToNonAD()
	if err == nil && !alias.IsEmpty() && alias != chat {
		chats = append(chats, alias)
	}
	return chats
}

func outboundFirstContact(ctx context.Context, store *MessageStore, chats ...types.JID) (bool, error) {
	if store == nil || len(chats) == 0 || chats[0].IsEmpty() {
		return true, nil
	}
	var conditions []string
	var args []any
	for _, chat := range chats {
		conditions = append(conditions, "chat_jid = ?")
		args = append(args, chat.ToNonAD().String())
	}
	var known bool
	err := store.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE `+strings.Join(conditions, " OR ")+`)`, args...).Scan(&known)
	return !known, err
}

func beginOutbound(ctx context.Context, store *MessageStore, chats ...types.JID) (func(), error) {
	return outboundLimiter.acquire(ctx, func(ctx context.Context) (bool, error) {
		return outboundFirstContact(ctx, store, chats...)
	})
}

func writeSendRateError(w http.ResponseWriter, err error) bool {
	var limited *sendRateError
	if !errors.As(err, &limited) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(retrySeconds(limited.retryAfter)))
	// Both keys: some MCP tools read "error", others read "message".
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": err.Error(), "message": err.Error()})
	return true
}
