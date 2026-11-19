package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"go.mau.fi/whatsmeow/types"
)

type fakePresenceSender struct {
	sent    []types.Presence
	failsOn types.Presence
}

func (f *fakePresenceSender) SendPresence(_ context.Context, state types.Presence) error {
	if state == f.failsOn {
		return errors.New("simulated failure")
	}
	f.sent = append(f.sent, state)
	return nil
}

// TestPulsePresence: the keepalive marks the device in use and always ends
// unavailable, so the phone keeps showing notifications.
func TestPulsePresence(t *testing.T) {
	t.Run("available_then_unavailable", func(t *testing.T) {
		f := &fakePresenceSender{}
		if err := pulsePresence(f, 0); err != nil {
			t.Fatalf("pulsePresence: %v", err)
		}
		want := []types.Presence{types.PresenceAvailable, types.PresenceUnavailable}
		if !reflect.DeepEqual(f.sent, want) {
			t.Fatalf("sent = %v, want %v", f.sent, want)
		}
	})

	t.Run("available_failure_sends_nothing_else", func(t *testing.T) {
		f := &fakePresenceSender{failsOn: types.PresenceAvailable}
		if err := pulsePresence(f, 0); err == nil {
			t.Fatal("pulsePresence returned nil, want the available error")
		}
		if len(f.sent) != 0 {
			t.Fatalf("sent = %v, want nothing", f.sent)
		}
	})
}
