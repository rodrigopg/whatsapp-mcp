package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

type fakeReconnecter struct {
	calls int
	wait  time.Duration
	err   error
}

func (f *fakeReconnecter) forceReconnect() (time.Duration, error) {
	f.calls++
	return f.wait, f.err
}

const testConnector = "https://wa.example.test"

func testPanel(token string, si statusInfo) (*panel, *fakeReconnecter, *http.ServeMux) {
	rc := &fakeReconnecter{}
	p := newPanel(token, testConnector+"/", func() statusInfo { return si }, rc)
	mux := http.NewServeMux()
	p.register(mux)
	return p, rc, mux
}

// do sends a request the way a browser on host would; origin "" sends no Origin header.
func do(mux *http.ServeMux, method, path, host, origin, cookie string, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(form.Encode()))
	r.Host = host
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if cookie != "" {
		r.AddCookie(&http.Cookie{Name: panelCookie, Value: cookie})
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestPanelActionsNeedCookieAndSameOrigin(t *testing.T) {
	p, rc, mux := testPanel("secret-token", statusInfo{Connected: true})
	const host, origin = "wa.example.test", "https://wa.example.test"
	good := p.cookieValue()
	for _, path := range []string{"/status/reconnect", "/status/diagnose", "/status/logout"} {
		for name, c := range map[string]struct {
			method, origin, cookie string
			want                   int
		}{
			"GET":            {"GET", origin, good, 405},
			"no cookie":      {"POST", origin, "", 403},
			"wrong cookie":   {"POST", origin, "secret-token", 403},
			"other origin":   {"POST", "https://evil.example", good, 403},
			"missing origin": {"POST", "", good, 403},
		} {
			if w := do(mux, c.method, path, host, c.origin, c.cookie, nil); w.Code != c.want {
				t.Errorf("%s %s = %d, want %d", path, name, w.Code, c.want)
			}
		}
	}
	if rc.calls != 0 {
		t.Fatalf("a refused request reached the reconnect: %d calls", rc.calls)
	}
	if w := do(mux, "POST", "/status/reconnect", host, origin, good, nil); w.Code != 303 || rc.calls != 1 || w.Header().Get("Location") != "/status?m=rc" {
		t.Errorf("valid reconnect = %d, calls %d, to %q", w.Code, rc.calls, w.Header().Get("Location"))
	}
	rc.wait, rc.err = 90*time.Second, errReconnectCooldown
	if w := do(mux, "POST", "/status/reconnect", host, origin, good, nil); w.Header().Get("Location") != "/status?m=wait91" {
		t.Errorf("cooldown redirect = %q", w.Header().Get("Location"))
	}
}

func TestPanelLogin(t *testing.T) {
	p, _, mux := testPanel("secret-token", statusInfo{Connected: true})
	const host, origin = "wa.example.test", "https://wa.example.test"
	if w := do(mux, "POST", "/status/login", host, "https://evil.example", "", url.Values{"key": {"secret-token"}}); w.Code != 403 {
		t.Errorf("cross-origin login = %d, want 403", w.Code)
	}
	if w := do(mux, "POST", "/status/login", host, origin, "", url.Values{"key": {"nope"}}); w.Code != 303 || len(w.Result().Cookies()) != 0 {
		t.Errorf("wrong key = %d with %d cookies", w.Code, len(w.Result().Cookies()))
	}
	w := do(mux, "POST", "/status/login", host, origin, "", url.Values{"key": {"secret-token"}})
	cs := w.Result().Cookies()
	if w.Code != 303 || len(cs) != 1 {
		t.Fatalf("right key = %d with %d cookies", w.Code, len(cs))
	}
	if c := cs[0]; c.Value != p.cookieValue() || strings.Contains(c.Value, "secret-token") || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Secure {
		t.Errorf("cookie wrong: %+v", c)
	}
	r := httptest.NewRequest("POST", "/status/login", strings.NewReader("key=secret-token"))
	r.Host = host
	r.Header.Set("Origin", origin)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("X-Forwarded-Proto", "https")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if c := rec.Result().Cookies(); len(c) != 1 || !c[0].Secure {
		t.Errorf("cookie behind an HTTPS proxy must be Secure: %+v", c)
	}

	// The 11th attempt is locked out even with the right key, until the window passes.
	clock := time.Now()
	p.now = func() time.Time { return clock }
	for i := 0; i < loginMaxFails; i++ {
		do(mux, "POST", "/status/login", host, origin, "", url.Values{"key": {"nope"}})
	}
	if w := do(mux, "POST", "/status/login", host, origin, "", url.Values{"key": {"secret-token"}}); w.Code != 429 {
		t.Errorf("locked login = %d, want 429", w.Code)
	}
	clock = clock.Add(loginWindow + time.Second)
	if w := do(mux, "POST", "/status/login", host, origin, "", url.Values{"key": {"secret-token"}}); w.Code != 303 || len(w.Result().Cookies()) != 1 {
		t.Errorf("login after the window = %d", w.Code)
	}
}

func TestPanelConnectorLinkOnlyWhenSignedIn(t *testing.T) {
	p, _, mux := testPanel("secret-token", statusInfo{Connected: true})
	lastAPIUnix.Store(time.Now().Unix())
	defer lastAPIUnix.Store(0)
	open := do(mux, "GET", "/status", "wa.example.test", "", "", nil).Body.String()
	for _, leak := range []string{testConnector, "/mcp", "Reconectar</button>", "/status/diagnose", "secret-token", p.cookieValue(), "Último acesso"} {
		if strings.Contains(open, leak) {
			t.Errorf("open page leaks %q", leak)
		}
	}
	if !strings.Contains(open, `action="/status/login"`) {
		t.Error("open page has no sign-in form")
	}
	in := do(mux, "GET", "/status", "wa.example.test", "", p.cookieValue(), nil).Body.String()
	for _, want := range []string{`value="` + testConnector + `/mcp"`, `action="/status/reconnect"`, `action="/status/diagnose"`, "Último acesso"} {
		if !strings.Contains(in, want) {
			t.Errorf("signed-in page lacks %q", want)
		}
	}
	if strings.Contains(in, "secret-token") {
		t.Error("signed-in page shows the token")
	}
}

func TestPanelWithoutTokenOnlyOnLoopback(t *testing.T) {
	_, rc, mux := testPanel("", statusInfo{Connected: true})
	if b := do(mux, "GET", "/status", "localhost:8080", "", "", nil).Body.String(); !strings.Contains(b, testConnector+"/mcp") {
		t.Error("loopback page without a token should be unlocked")
	}
	// DNS rebinding: the browser reaches 127.0.0.1 but sends the attacker's Host.
	if b := do(mux, "GET", "/status", "evil.example:8080", "", "", nil).Body.String(); strings.Contains(b, testConnector) || !strings.Contains(b, "localhost") {
		t.Error("non-loopback Host must stay locked without a token")
	}
	for name, c := range map[string]struct {
		host, origin string
		want         int
	}{
		"local form":     {"localhost:8080", "http://localhost:8080", 303},
		"web page posts": {"localhost:8080", "https://evil.example", 403},
		"rebinding":      {"evil.example:8080", "http://evil.example:8080", 403},
	} {
		if w := do(mux, "POST", "/status/reconnect", c.host, c.origin, "", nil); w.Code != c.want {
			t.Errorf("%s = %d, want %d", name, w.Code, c.want)
		}
	}
	if rc.calls != 1 {
		t.Errorf("reconnect calls = %d, want 1", rc.calls)
	}
	if w := do(mux, "POST", "/status/login", "localhost:8080", "http://localhost:8080", "", url.Values{"key": {""}}); w.Code != 403 {
		t.Errorf("login without a token = %d, want 403 (an empty key must never sign in)", w.Code)
	}
}

func TestPanelStates(t *testing.T) {
	now := time.Now()
	for want, si := range map[string]statusInfo{
		"ok":        {Connected: true, LastMsgAt: now.Add(-time.Hour)},
		"stall":     {Connected: true, LastMsgAt: now.Add(-7 * time.Hour)},
		"pairing":   {Connected: true, Pairing: true, LastMsgAt: now.Add(-48 * time.Hour)},
		"qr":        {HasQR: true},
		"loggedout": {HasQR: true, LoggedOutAt: now},
		"held":      {Paired: true, Held: time.Minute},
		"stopped":   {Paired: true, Stopped: true},
		"offline":   {Paired: true},
		"waiting":   {},
	} {
		if got := panelState(si, now); got != want {
			t.Errorf("panelState(%+v) = %q, want %q", si, got, want)
		}
		p, _, mux := testPanel("tok", si)
		body := do(mux, "GET", "/status", "h", "", p.cookieValue(), nil).Body.String()
		refresh := want == "qr" || want == "pairing" || want == "loggedout" || want == "waiting"
		if strings.Contains(body, `http-equiv="refresh"`) != refresh {
			t.Errorf("%s: refresh tag present = %v, want %v", want, !refresh, refresh)
		}
		canReconnect := want == "ok" || want == "stall" || want == "offline"
		if strings.Contains(body, `action="/status/reconnect"`) != canReconnect {
			t.Errorf("%s: reconnect button present = %v, want %v", want, !canReconnect, canReconnect)
		}
	}
	p, _, mux := testPanel("tok", statusInfo{Connected: true, LastMsgAt: now.Add(-7*time.Hour - time.Minute)})
	if b := do(mux, "GET", "/status", "h", "", p.cookieValue(), nil).Body.String(); !strings.Contains(b, "há 7 h") {
		t.Error("stall banner does not say how long")
	}
}

func TestPairingNow(t *testing.T) {
	defer pairedAtUnix.Store(0)
	defer lastHistoryUnix.Store(0)
	now := time.Now()
	if pairingNow(now) {
		t.Error("never paired in this run")
	}
	pairedAtUnix.Store(now.Add(-10 * time.Minute).Unix())
	if pairingNow(now) {
		t.Error("paired long ago, no history arriving")
	}
	lastHistoryUnix.Store(now.Add(-30 * time.Second).Unix())
	if !pairingNow(now) {
		t.Error("history still arriving")
	}
}

func TestForceReconnect(t *testing.T) {
	h := newSupervisorHarness()
	h.session.paired, h.session.connected = true, true
	if _, err := h.forceReconnect(); err != nil || h.session.connects != 1 || !h.session.connected {
		t.Fatalf("first forced reconnect: err %v, connects %d", err, h.session.connects)
	}
	if wait, err := h.forceReconnect(); err != errReconnectCooldown || wait <= 0 || h.session.connects != 1 {
		t.Errorf("inside the cooldown: wait %v err %v connects %d", wait, err, h.session.connects)
	}
	h.clock = h.clock.Add(forceReconnectCooldown + time.Second)
	h.handle(&events.TemporaryBan{Expire: time.Hour})
	if _, err := h.forceReconnect(); err != errReconnectHeld || h.session.connects != 1 {
		t.Errorf("while held: err %v connects %d", err, h.session.connects)
	}
	unpaired := newSupervisorHarness()
	unpaired.session.paired = false
	if _, err := unpaired.forceReconnect(); err != errReconnectStopped || unpaired.session.connects != 0 {
		t.Errorf("unpaired: err %v", err)
	}
}

func TestDiagnoseConnector(t *testing.T) {
	var status int
	var body string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/oauth-protected-resource/mcp" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.WriteHeader(status)
		w.Write([]byte(strings.ReplaceAll(body, "SELF", "https://"+r.Host)))
	}))
	defer srv.Close()
	for name, c := range map[string]struct {
		status int
		body   string
		ok     bool
	}{
		"connector up":   {200, `{"resource":"SELF/mcp"}`, true},
		"oauth off":      {404, `not found`, false},
		"tunnel error":   {502, `bad gateway`, false},
		"not json":       {200, `<html>parked domain</html>`, false},
		"someone else":   {200, `{"resource":"https://other.example/mcp"}`, false},
		"empty resource": {200, `{}`, false},
	} {
		status, body = c.status, c.body
		if res := diagnoseConnector(context.Background(), srv.Client(), srv.URL); res.OK != c.ok || (!c.ok && res.Detail == "") {
			t.Errorf("%s: %+v", name, res)
		}
	}
	if res := diagnoseConnector(context.Background(), srv.Client(), ""); res.OK || !strings.Contains(res.Detail, "MCP_PUBLIC_URL") {
		t.Errorf("no URL: %+v", res)
	}
	// The default client does not trust the test certificate: a broken certificate is named as such.
	if res := diagnoseConnector(context.Background(), &http.Client{}, srv.URL); res.OK || !strings.Contains(res.Detail, "certificado") {
		t.Errorf("bad certificate: %+v", res)
	}
	srv.Close()
	if res := diagnoseConnector(context.Background(), srv.Client(), srv.URL); res.OK || !strings.Contains(res.Detail, "não respondeu") {
		t.Errorf("server down: %+v", res)
	}
}

func TestStampAPIAccess(t *testing.T) {
	defer lastAPIUnix.Store(0)
	h := stampAPIAccess(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/status", nil))
	if lastAPIUnix.Load() != 0 {
		t.Error("/status counted as API access")
	}
	// Behind the bearer check: a refused request must not count.
	guarded := requireBearerToken("tok", h)
	guarded.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/chats", nil))
	if lastAPIUnix.Load() != 0 {
		t.Error("unauthorized request counted as API access")
	}
	r := httptest.NewRequest("GET", "/api/chats", nil)
	r.Header.Set("Authorization", "Bearer tok")
	guarded.ServeHTTP(httptest.NewRecorder(), r)
	if lastAPIUnix.Load() == 0 {
		t.Error("authorized request not counted")
	}
}
