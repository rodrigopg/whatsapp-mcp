package main

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func loginFrom(mux *http.ServeMux, remote, key string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/status/login", strings.NewReader("key="+key))
	r.Host, r.RemoteAddr = "wa.example.test", remote
	r.Header.Set("Origin", "https://wa.example.test")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

// Wrong keys from one address lock that address only: nobody else is kept out of the panel.
func TestPanelLoginLockIsPerClient(t *testing.T) {
	_, _, mux := testPanel("secret-token", statusInfo{Connected: true})
	for i := 0; i < loginMaxFails+5; i++ {
		loginFrom(mux, "203.0.113.7:4000", "nope")
	}
	if w := loginFrom(mux, "203.0.113.7:4001", "secret-token"); w.Code != http.StatusTooManyRequests {
		t.Errorf("the address that guessed = %d, want 429", w.Code)
	}
	if w := loginFrom(mux, "203.0.113.8:4000", "secret-token"); w.Code != http.StatusSeeOther || len(w.Result().Cookies()) != 1 {
		t.Errorf("another address with the right key = %d, want signed in", w.Code)
	}
	// One IPv6 network (/64) is one client: its owner has all of those addresses.
	for i := 0; i < loginMaxFails; i++ {
		loginFrom(mux, "[2001:db8:1:2::"+strconv.FormatInt(int64(i+1), 16)+"]:4000", "nope")
	}
	if w := loginFrom(mux, "[2001:db8:1:2:ffff::1]:4000", "secret-token"); w.Code != http.StatusTooManyRequests {
		t.Errorf("same IPv6 /64 = %d, want 429", w.Code)
	}
	if w := loginFrom(mux, "[2001:db8:1:3::1]:4000", "secret-token"); w.Code != http.StatusSeeOther {
		t.Errorf("another IPv6 network = %d, want signed in", w.Code)
	}
}

// The table of clients is bounded: past the cap, new addresses share one counter.
func TestPanelLoginLockTableIsBounded(t *testing.T) {
	p, _, mux := testPanel("secret-token", statusInfo{Connected: true})
	for i := 0; i < loginMaxClients+50; i++ {
		loginFrom(mux, "10."+itoa(i/65536)+"."+itoa(i/256%256)+"."+itoa(i%256)+":1", "nope")
	}
	if n := len(p.fails); n > loginMaxClients+1 {
		t.Errorf("%d clients tracked, cap is %d", n, loginMaxClients)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += n
	return n, err
}

// A panel form is a few fields: a huge body is cut off instead of being parsed and spooled to disk.
func TestPanelFormBodyIsCapped(t *testing.T) {
	_, _, mux := testPanel("secret-token", statusInfo{Connected: true})
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	file, _ := mw.CreateFormFile("f", "big.bin")
	file.Write(bytes.Repeat([]byte("x"), 8<<20))
	mw.WriteField("key", "secret-token")
	mw.Close()
	src := &countingReader{r: &body}
	r := httptest.NewRequest("POST", "/status/login", src)
	r.Host = "wa.example.test"
	r.Header.Set("Origin", "https://wa.example.test")
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if len(w.Result().Cookies()) != 0 {
		t.Error("signed in from a body over the cap")
	}
	if src.n > 2*panelMaxBody {
		t.Errorf("read %d bytes of the body, cap is %d", src.n, panelMaxBody)
	}
}

// A client that never finishes its request (or never sends one) is dropped.
func TestRESTServerHasTimeouts(t *testing.T) {
	s := newRESTServer("127.0.0.1:0", http.NotFoundHandler())
	if s.ReadHeaderTimeout <= 0 || s.ReadTimeout <= 0 || s.IdleTimeout <= 0 {
		t.Errorf("timeouts not set: header=%v read=%v idle=%v", s.ReadHeaderTimeout, s.ReadTimeout, s.IdleTimeout)
	}
}
