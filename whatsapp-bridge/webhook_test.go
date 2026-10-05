package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func samplePayload() webhookPayload {
	return webhookPayload{Event: "message.received", ChatJID: "1@s.whatsapp.net", MessageID: "ABC", Content: "hi"}
}

func TestWebhookDisabledAndRefusal(t *testing.T) {
	if d, err := newWebhookDispatcher("", ""); d != nil || err != nil {
		t.Fatalf("unset URL must be a silent no-op, got %v %v", d, err)
	}
	if d, err := newWebhookDispatcher("http://example.com/h", ""); d != nil || err == nil {
		t.Fatal("missing secret must refuse")
	}
	if d, err := newWebhookDispatcher("ftp://example.com/h", "s"); d != nil || err == nil {
		t.Fatal("non-http scheme must refuse")
	}
}

func TestWebhookSignatureAndHeaders(t *testing.T) {
	type got struct {
		body []byte
		h    http.Header
	}
	ch := make(chan got, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- got{b, r.Header.Clone()}
	}))
	defer srv.Close()
	d, err := newWebhookDispatcher(srv.URL, "topsecret")
	if err != nil {
		t.Fatal(err)
	}
	d.enqueue(samplePayload())
	select {
	case g := <-ch:
		if want := webhookSign([]byte("topsecret"), g.body); g.h.Get("X-Signature") != want {
			t.Fatalf("bad signature %q want %q", g.h.Get("X-Signature"), want)
		}
		if g.h.Get("X-Event") != "message.received" || g.h.Get("X-Delivery-Id") == "" {
			t.Fatalf("missing headers: %v", g.h)
		}
		var p webhookPayload
		if json.Unmarshal(g.body, &p) != nil || p.MessageID != "ABC" {
			t.Fatalf("bad body %s", g.body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no delivery")
	}
}

func TestWebhookRetriesOn500(t *testing.T) {
	webhookBackoff = time.Millisecond
	var n int32
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.WriteHeader(500)
			return
		}
		close(done)
	}))
	defer srv.Close()
	d, _ := newWebhookDispatcher(srv.URL, "s")
	d.enqueue(samplePayload())
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("no success after retries, attempts=%d", atomic.LoadInt32(&n))
	}
}

func TestWebhookDoesNotFollowRedirects(t *testing.T) {
	webhookBackoff = time.Millisecond
	var hits, first int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { atomic.AddInt32(&hits, 1) }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&first, 1)
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	d, _ := newWebhookDispatcher(redir.URL, "s")
	d.enqueue(samplePayload())
	time.Sleep(500 * time.Millisecond)
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("redirect target received the signed POST")
	}
	if atomic.LoadInt32(&first) == 0 {
		t.Fatal("receiver never called")
	}
}

func TestWebhookNeverBlocksOnHungReceiver(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	d, _ := newWebhookDispatcher(srv.URL, "s")
	start := time.Now()
	for i := 0; i < webhookQueueSize*3; i++ {
		d.enqueue(samplePayload())
	}
	if time.Since(start) > time.Second {
		t.Fatal("enqueue blocked on a hung receiver")
	}
}
