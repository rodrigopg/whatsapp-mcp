package main

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old; r.Close() }()
	out := make(chan string, 1)
	go func() { b, _ := io.ReadAll(r); out <- string(b) }()
	f()
	w.Close()
	return <-out
}

func TestMessageLogKeepsTextOutUnlessAsked(t *testing.T) {
	for setting, wantText := range map[string]bool{"": false, "0": false, "false": false, "1": true, "true": true, "yes": true, " ON ": true} {
		t.Run("WHATSAPP_LOG_CONTENT="+setting, func(t *testing.T) {
			t.Setenv("WHATSAPP_LOG_CONTENT", setting)
			t.Setenv("WEBHOOK_URL", "")
			s := setupChatStore(t)
			if err := s.StoreChat("123-456@g.us", "Test group", time.Time{}); err != nil {
				t.Fatal(err)
			}
			for name, msg := range map[string]*waProto.Message{
				"text":    {Conversation: proto.String("PRIVATE TEXT")},
				"caption": {ImageMessage: &waProto.ImageMessage{Caption: proto.String("PRIVATE TEXT"), Mimetype: proto.String("image/jpeg")}},
			} {
				out := captureStdout(t, func() {
					handleMessage(&whatsmeow.Client{}, s, typeEvent("id-"+name, "111", 100, msg), waLog.Noop)
				})
				if strings.Contains(out, "PRIVATE TEXT") != wantText {
					t.Errorf("%s: text in log = %v, want %v: %s", name, !wantText, wantText, out)
				}
				if !strings.Contains(out, "123-456@g.us") || !strings.Contains(out, "length=12") {
					t.Errorf("%s: metadata missing: %s", name, out)
				}
			}
		})
	}
}
