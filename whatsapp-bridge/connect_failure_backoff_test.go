package main

import (
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestConnectFailureBacksOff(t *testing.T) {
	h := newSupervisorHarness()
	h.session.connected = false
	for _, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		h.handle(&events.ConnectFailure{Reason: events.ConnectFailureClientOutdated})
		if got := h.timers[len(h.timers)-1]; got != want {
			t.Fatalf("pause = %v, want %v", got, want)
		}
		h.reconnect()
		if h.session.connects != 0 {
			t.Fatal("watchdog reconnected during the pause")
		}
	}
}
