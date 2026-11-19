package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type fakeSession struct {
	paired, connected bool
	connects          int
}

func (f *fakeSession) Connect() error    { f.connects++; f.connected = true; return nil }
func (f *fakeSession) IsConnected() bool { return f.connected }
func (f *fakeSession) IsPaired() bool    { return f.paired }

type supervisorHarness struct {
	*connectionSupervisor
	session *fakeSession
	deleted bool
	deletes []error // results of the supervisor's own delete attempts, in order
	clock   time.Time
	timers  []time.Duration
	pending []func()
	exited  []int
}

func newSupervisorHarness() *supervisorHarness {
	h := &supervisorHarness{session: &fakeSession{paired: true}, clock: time.Unix(1_000_000, 0)}
	h.connectionSupervisor = &connectionSupervisor{
		session:        h.session,
		sessionDeleted: func() bool { return h.deleted },
		deleteSession: func() error {
			err := h.deletes[0]
			h.deletes = h.deletes[1:]
			if err == nil {
				h.deleted = true
			}
			return err
		},
		log:  waLog.Noop,
		exit: func(code int) { h.exited = append(h.exited, code) },
		now:  func() time.Time { return h.clock },
		after: func(d time.Duration, f func()) {
			h.timers = append(h.timers, d)
			h.pending = append(h.pending, f)
		},
	}
	return h
}

// fire advances the clock by the last scheduled delay and runs that timer.
func (h *supervisorHarness) fire() {
	h.clock = h.clock.Add(h.timers[len(h.timers)-1])
	h.pending[len(h.pending)-1]()
}

func TestStreamReplacedReconnectsWithBackoff(t *testing.T) {
	h := newSupervisorHarness()
	for _, want := range []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute} {
		h.handle(&events.StreamReplaced{})
		if got := h.timers[len(h.timers)-1]; got != want {
			t.Fatalf("pause = %v, want %v", got, want)
		}
		h.reconnect() // the watchdog must not jump the pause
		if h.session.connects != 0 {
			t.Fatal("reconnected during the pause")
		}
	}
	h.fire()
	if h.session.connects != 1 {
		t.Fatalf("connects = %d, want 1 after the pause", h.session.connects)
	}

	h.session.connected = false
	h.clock = h.clock.Add(2 * time.Hour)
	h.handle(&events.StreamReplaced{})
	if got := h.timers[len(h.timers)-1]; got != 30*time.Second {
		t.Fatalf("pause after a quiet hour = %v, want 30s", got)
	}
}

func TestStreamReplacedPauseIsCapped(t *testing.T) {
	h := newSupervisorHarness()
	for range 20 {
		h.handle(&events.StreamReplaced{})
	}
	if got := h.timers[len(h.timers)-1]; got != streamReplacedMaxPause {
		t.Fatalf("pause = %v, want %v", got, streamReplacedMaxPause)
	}
}

func TestTemporaryBanWaitsForExpiry(t *testing.T) {
	for name, tc := range map[string]struct{ expire, want time.Duration }{
		"known expiry":   {expire: 10 * time.Minute, want: 10 * time.Minute},
		"unknown expiry": {want: time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSupervisorHarness()
			h.handle(&events.TemporaryBan{Expire: tc.expire})
			h.reconnect()
			if h.session.connects != 0 || h.timers[0] != tc.want {
				t.Fatalf("connects = %d, timer = %v; want 0 and %v", h.session.connects, h.timers[0], tc.want)
			}
			h.fire()
			if h.session.connects != 1 {
				t.Fatalf("connects = %d after the ban, want 1", h.session.connects)
			}
		})
	}
}

func TestClientOutdatedStopsReconnecting(t *testing.T) {
	h := newSupervisorHarness()
	h.handle(&events.ClientOutdated{})
	h.reconnect()
	if h.session.connects != 0 || len(h.exited) != 0 {
		t.Fatalf("connects = %d, exits = %v; want neither", h.session.connects, h.exited)
	}
}

func TestLoggedOutExitsOnceSessionIsDeleted(t *testing.T) {
	h := newSupervisorHarness()
	h.handle(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	h.fire()
	h.reconnect()
	if len(h.exited) != 0 || h.session.connects != 0 {
		t.Fatalf("exits = %v, connects = %d; want to wait for the deletion without reconnecting", h.exited, h.session.connects)
	}
	h.deleted = true
	h.fire()
	if len(h.exited) != 1 || h.exited[0] != 1 {
		t.Fatalf("exits = %v, want [1]", h.exited)
	}
}

func TestLoggedOutDeletesSessionItselfUntilItWorks(t *testing.T) {
	h := newSupervisorHarness()
	h.deletes = []error{errors.New("database is locked"), nil}
	h.handle(&events.LoggedOut{Reason: events.ConnectFailureLoggedOut})
	for len(h.exited) == 0 && len(h.timers) < 1000 {
		h.fire()
	}
	if len(h.exited) != 1 || !h.deleted || len(h.deletes) != 0 {
		t.Fatalf("exits = %v, deleted = %v, delete attempts left = %d; want one exit after the second delete",
			h.exited, h.deleted, len(h.deletes))
	}
	if polls := time.Duration(len(h.timers)) * logoutPollInterval; polls != 2*logoutDeleteTimeout {
		t.Fatalf("exited after %v of polling, want %v", polls, 2*logoutDeleteTimeout)
	}
}

func TestStoppedOrHeldDuringRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		evt           any
		stopped, held bool
	}{
		"outdated":        {evt: &events.ClientOutdated{}, stopped: true},
		"logged out":      {evt: &events.LoggedOut{}, stopped: true},
		"ban":             {evt: &events.TemporaryBan{Expire: time.Minute}, held: true},
		"stream replaced": {evt: &events.StreamReplaced{}, held: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSupervisorHarness()
			h.deleted = true
			if h.isStopped() || h.held() {
				t.Fatal("blocked before any refusal")
			}
			h.handle(tc.evt)
			if h.isStopped() != tc.stopped || h.held() != tc.held {
				t.Fatalf("stopped = %v, held = %v; want %v, %v", h.isStopped(), h.held(), tc.stopped, tc.held)
			}
			if tc.held {
				h.clock = h.clock.Add(time.Hour)
				if h.held() {
					t.Fatal("still held after the pause; pairing would never get a fresh QR code")
				}
			}
		})
	}
}

func TestQRTimeoutRestartsOnlyWhenPairingCanWork(t *testing.T) {
	for name, tc := range map[string]struct {
		evt     any
		restart bool
		waited  time.Duration
	}{
		"no refusal": {restart: true},
		"ban":        {evt: &events.TemporaryBan{Expire: 10 * time.Minute}, restart: true, waited: 10 * time.Minute},
		"outdated":   {evt: &events.ClientOutdated{}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSupervisorHarness()
			if tc.evt != nil {
				h.handle(tc.evt)
			}
			start := h.clock
			got := h.restartForNewQR(func(d time.Duration) { h.clock = h.clock.Add(d) })
			if got != tc.restart || h.clock.Sub(start) != tc.waited {
				t.Fatalf("restart = %v after %v, want %v after %v", got, h.clock.Sub(start), tc.restart, tc.waited)
			}
		})
	}
}

func TestFailOnPanicReportsFailure(t *testing.T) {
	handler := failOnPanic(waLog.Noop, func(any) bool { panic("boom") })
	if handler(&events.Message{}) {
		t.Fatal("a panicking handler reported success, so whatsmeow would ack the message")
	}
	if !failOnPanic(waLog.Noop, func(any) bool { return true })(nil) {
		t.Fatal("a successful handler reported failure")
	}
}

func TestWatchdogReconnectsOnlyPairedOfflineClient(t *testing.T) {
	for name, tc := range map[string]struct {
		paired, connected bool
		want              int
	}{
		"paired offline":   {paired: true, want: 1},
		"paired connected": {paired: true, connected: true},
		"not paired":       {},
	} {
		t.Run(name, func(t *testing.T) {
			h := newSupervisorHarness()
			h.session.paired, h.session.connected = tc.paired, tc.connected
			h.reconnect()
			if h.session.connects != tc.want {
				t.Fatalf("connects = %d, want %d", h.session.connects, tc.want)
			}
		})
	}
}

func TestHandleMessageReportsFailedWrites(t *testing.T) {
	text := &waProto.Message{Conversation: proto.String("text")}
	edit := &waProto.Message{ProtocolMessage: &waProto.ProtocolMessage{
		Type: waProto.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waProto.MessageKey{ID: proto.String("old")}, EditedMessage: text,
	}}
	for name, tc := range map[string]struct {
		body       *waProto.Message
		failWrite  bool
		failTable  string // only this table refuses writes
		updateOnly bool
		want       bool
	}{
		"stored":       {body: text, want: true},
		"nothing":      {body: &waProto.Message{}, want: true},
		"write fails":  {body: text, failWrite: true},
		"edit applied": {body: edit, want: true},
		"edit fails":   {body: edit, failWrite: true},
		"chat fails":   {body: text, failTable: "chats", updateOnly: true}, // EnsureChat passes, StoreChat fails
		"mention fails": {body: &waProto.Message{ExtendedTextMessage: &waProto.ExtendedTextMessage{
			Text: proto.String("hi @333"), ContextInfo: &waProto.ContextInfo{MentionedJID: []string{"333@s.whatsapp.net"}},
		}}, failTable: "message_mentions"},
		"poll fails": {body: &waProto.Message{PollCreationMessageV3: &waProto.PollCreationMessage{
			Name: proto.String("q"), Options: []*waProto.PollCreationMessage_Option{{OptionName: proto.String("a")}},
		}}, failTable: "polls"},
	} {
		t.Run(name, func(t *testing.T) {
			s := setupChatStore(t)
			chat := types.NewJID("111", types.DefaultUserServer)
			if err := s.StoreChat(chat.String(), "Contact", time.Unix(100, 0)); err != nil {
				t.Fatal(err)
			}
			if err := s.StoreMessage("old", chat.String(), "111", "before", time.Unix(100, 0), false, "", "", "", nil, nil, nil, 0); err != nil {
				t.Fatal(err)
			}
			if tc.failWrite {
				if _, err := s.db.Exec(`
					CREATE TRIGGER fail_insert BEFORE INSERT ON messages BEGIN SELECT RAISE(FAIL, 'test write failure'); END;
					CREATE TRIGGER fail_update BEFORE UPDATE ON messages BEGIN SELECT RAISE(FAIL, 'test write failure'); END`); err != nil {
					t.Fatal(err)
				}
			}
			if tc.failTable != "" {
				q := `CREATE TRIGGER fail_update BEFORE UPDATE ON ` + tc.failTable + ` BEGIN SELECT RAISE(FAIL, 'test write failure'); END`
				if !tc.updateOnly {
					q += `; CREATE TRIGGER fail_insert BEFORE INSERT ON ` + tc.failTable + ` BEGIN SELECT RAISE(FAIL, 'test write failure'); END`
				}
				if _, err := s.db.Exec(q); err != nil {
					t.Fatal(err)
				}
			}
			got := handleMessage(&whatsmeow.Client{}, s, &events.Message{
				Info:    types.MessageInfo{ID: "live", Timestamp: time.Unix(200, 0), MessageSource: types.MessageSource{Chat: chat, Sender: chat}},
				Message: tc.body,
			}, waLog.Noop)
			if got != tc.want {
				t.Fatalf("handleMessage = %v, want %v", got, tc.want)
			}
		})
	}
}

type msgSecrets struct {
	store.MsgSecretStore
	err error
}

func (m msgSecrets) GetMessageSecret(context.Context, types.JID, types.JID, types.MessageID) ([]byte, types.JID, error) {
	return nil, types.JID{}, m.err
}

func TestPollVoteReadFailureIsNotAcked(t *testing.T) {
	chat := types.NewJID("111", types.DefaultUserServer)
	vote := &events.Message{
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: chat, Sender: chat}},
		Message: &waProto.Message{PollUpdateMessage: &waProto.PollUpdateMessage{
			PollCreationMessageKey: &waProto.MessageKey{ID: proto.String("p1"), RemoteJID: proto.String(chat.String())},
		}},
	}
	client := func(err error) *whatsmeow.Client {
		return &whatsmeow.Client{Store: &store.Device{MsgSecrets: msgSecrets{err: err}}}
	}
	s := setupChatStore(t)
	if !recordPollVote(client(nil), s, vote, chat.String(), "222", waLog.Noop) {
		t.Fatal("a vote on an untracked poll was reported as failed; whatsmeow would redeliver it forever")
	}
	if _, err := s.db.Exec(`INSERT INTO polls (id, chat_jid, question, options, selectable_count) VALUES ('p1', ?, 'q', '["a"]', 1)`, chat.String()); err != nil {
		t.Fatal(err)
	}
	if !recordPollVote(client(nil), s, vote, chat.String(), "222", waLog.Noop) {
		t.Fatal("a vote whose poll key is gone for good was reported as failed")
	}
	if recordPollVote(client(errors.New("database is locked")), s, vote, chat.String(), "222", waLog.Noop) {
		t.Fatal("a failed read of the poll key was acked, dropping the vote")
	}
	if _, err := s.db.Exec(`ALTER TABLE polls RENAME TO polls_gone`); err != nil {
		t.Fatal(err)
	}
	if recordPollVote(client(nil), s, vote, chat.String(), "222", waLog.Noop) {
		t.Fatal("a database failure was acked, dropping the vote")
	}
}
