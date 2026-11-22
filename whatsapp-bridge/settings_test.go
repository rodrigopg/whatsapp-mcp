package main

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	setHost   = "wa.example.test"
	setOrigin = "https://wa.example.test"
)

func settingsPanel(t *testing.T, token string) (*panel, *fakeReconnecter, *http.ServeMux) {
	t.Helper()
	p, rc, mux := testPanel(token, statusInfo{Connected: true, LastMsgAt: time.Now().Add(-4 * time.Hour)})
	st, err := loadSettings(filepath.Join(t.TempDir(), "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	p.set = st
	return p, rc, mux
}

func TestSettingsStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	st, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := st.get(); got.stallAfter() != 6*time.Hour || got.ReadOnly || got.TranscribeOff {
		t.Fatalf("defaults = %+v", got)
	}
	never := 0
	if err := st.update(func(s *settings) { s.StallHours, s.ReadOnly = &never, true }); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("settings file mode = %v, want 0600", fi.Mode().Perm())
	}
	again, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := again.get(); got.stallAfter() != 0 || !got.ReadOnly {
		t.Errorf("reloaded = %+v", got)
	}
	os.WriteFile(path, []byte("{broken"), 0o600)
	if _, err := loadSettings(path); err == nil {
		t.Error("a broken settings file must be an error, not silent defaults")
	}
	var none *settingsStore
	if none.get().ReadOnly || none.update(func(*settings) {}) == nil {
		t.Error("a nil store must answer with defaults and refuse to save")
	}
}

func TestSettingsUpdateKeepsOldValuesWhenTheWriteFails(t *testing.T) {
	st := &settingsStore{path: filepath.Join(t.TempDir(), "missing-dir", "settings.json")}
	if err := st.update(func(s *settings) { s.ReadOnly = true }); err == nil {
		t.Fatal("want an error")
	}
	if st.get().ReadOnly {
		t.Error("a value that was not written must not take effect")
	}
}

func TestReadOnlyGuard(t *testing.T) {
	st, _ := loadSettings(filepath.Join(t.TempDir(), "settings.json"))
	hits := 0
	h := readOnlyGuard(st, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	call := func(path string) int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, nil))
		return w.Code
	}
	writes := []string{"/api/send", "/api/react", "/api/archive_chat", "/api/drafts/approve", "/api/revoke", "/api/some_future_route"}
	for _, path := range writes {
		if call(path) != http.StatusOK {
			t.Errorf("%s refused with read-only off", path)
		}
	}
	st.update(func(s *settings) { s.ReadOnly = true })
	hits = 0
	for _, path := range writes {
		if call(path) != http.StatusForbidden {
			t.Errorf("%s let through in read-only mode", path)
		}
	}
	if hits != 0 {
		t.Errorf("%d write requests reached the handler in read-only mode", hits)
	}
	for _, path := range []string{"/api/messages", "/api/download", "/api/summary", "/status", "/healthz"} {
		if call(path) != http.StatusOK {
			t.Errorf("%s refused in read-only mode", path)
		}
	}
}

func TestSettingsActionsNeedCookieAndSameOrigin(t *testing.T) {
	p, rc, mux := settingsPanel(t, "secret-token")
	good := p.cookieValue()
	form := url.Values{"hours": {"3"}, "readonly": {"1"}, "lang": {"en"}, "confirm": {unpairWord}, "id": {"x"}}
	for _, path := range []string{"stall", "ai", "revoke", "audio", "signout", "newkey", "unpair"} {
		path = "/status/settings/" + path
		for name, c := range map[string]struct {
			method, origin, cookie string
			want                   int
		}{
			"GET":          {http.MethodGet, setOrigin, good, http.StatusMethodNotAllowed},
			"no cookie":    {http.MethodPost, setOrigin, "", http.StatusForbidden},
			"bad cookie":   {http.MethodPost, setOrigin, "nope", http.StatusForbidden},
			"other origin": {http.MethodPost, "https://evil.example", good, http.StatusForbidden},
			"no origin":    {http.MethodPost, "", good, http.StatusForbidden},
		} {
			if w := do(mux, c.method, path, setHost, c.origin, c.cookie, form); w.Code != c.want {
				t.Errorf("%s %s: status %d, want %d", path, name, w.Code, c.want)
			}
		}
	}
	if s := p.set.get(); s.StallHours != nil || s.ReadOnly || s.Language != "" || s.PanelKeyHash != "" || s.PanelEpoch != 0 || rc.unpairs != 0 {
		t.Errorf("a refused request changed something: %+v, unpairs %d", s, rc.unpairs)
	}
}

func TestSettingsPageOnlyWhenSignedIn(t *testing.T) {
	p, _, mux := settingsPanel(t, "secret-token")
	p.set.update(func(s *settings) { s.ReadOnly, s.TranscriptionKey = true, "sk-very-secret-key" })
	open := do(mux, http.MethodGet, "/status/settings", setHost, "", "", nil).Body.String()
	for _, leak := range []string{"Somente leitura", "Gerar nova chave", unpairWord, "sk-very-secret-key"} {
		if strings.Contains(open, leak) {
			t.Errorf("open settings page shows %q", leak)
		}
	}
	if !strings.Contains(open, `action="/status/login"`) {
		t.Error("open settings page has no sign-in form")
	}
	in := do(mux, http.MethodGet, "/status/settings", setHost, "", p.cookieValue(), nil).Body.String()
	for _, want := range []string{`name="readonly" value="1" checked`, "Gerar nova chave", unpairWord, `aria-current="page">Configurações`} {
		if !strings.Contains(in, want) {
			t.Errorf("signed-in settings page lacks %q", want)
		}
	}
	if strings.Contains(in, "sk-very-secret-key") {
		t.Error("the transcription key is shown")
	}
}

func TestSettingsSaveStallAndReadOnly(t *testing.T) {
	p, _, mux := settingsPanel(t, "secret-token")
	post := func(path string, form url.Values) string {
		t.Helper()
		w := do(mux, http.MethodPost, path, setHost, setOrigin, p.cookieValue(), form)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		return w.Header().Get("Location")
	}
	home := func() string {
		return do(mux, http.MethodGet, "/status", setHost, "", "", nil).Body.String()
	}
	if strings.Contains(home(), "nenhuma mensagem nova") {
		t.Fatal("4 h without messages must not warn at the default 6 h")
	}
	if loc := post("/status/settings/stall", url.Values{"hours": {"3"}}); loc != "/status/settings?m=saved" {
		t.Errorf("redirect = %q", loc)
	}
	if !strings.Contains(home(), "nenhuma mensagem nova") {
		t.Error("4 h without messages must warn at 3 h")
	}
	post("/status/settings/stall", url.Values{"hours": {"0"}})
	if strings.Contains(home(), "nenhuma mensagem nova") {
		t.Error(`"never" still warns`)
	}
	if loc := post("/status/settings/stall", url.Values{"hours": {"5"}}); loc != "/status/settings?m=invalid" {
		t.Errorf("unknown threshold accepted: %q", loc)
	}
	post("/status/settings/ai", url.Values{"readonly": {"1"}})
	if !p.set.get().ReadOnly {
		t.Error("read-only not saved")
	}
	post("/status/settings/ai", url.Values{})
	if p.set.get().ReadOnly {
		t.Error("read-only not cleared")
	}
}

func TestSettingsAudio(t *testing.T) {
	p, _, mux := settingsPanel(t, "secret-token")
	post := func(form url.Values) string {
		return do(mux, http.MethodPost, "/status/settings/audio", setHost, setOrigin, p.cookieValue(), form).Header().Get("Location")
	}
	for name, form := range map[string]url.Values{
		"language":       {"on": {"1"}, "lang": {"pt; rm"}},
		"key with space": {"on": {"1"}, "lang": {"pt"}, "key": {"abc def ghi"}},
		"key with line":  {"on": {"1"}, "lang": {"pt"}, "key": {"abcdefgh\nEVIL=1"}},
	} {
		if loc := post(form); loc != "/status/settings?m=invalid" {
			t.Errorf("bad %s accepted: %q", name, loc)
		}
	}
	if s := p.set.get(); s.Language != "" || s.TranscriptionKey != "" {
		t.Fatalf("invalid input saved: %+v", s)
	}
	post(url.Values{"on": {"1"}, "outgoing": {"1"}, "lang": {"auto"}, "key": {"sk-abcdefgh"}})
	want := []string{"TRANSCRIPTION_LANGUAGE=auto", "TRANSCRIBE_OUTGOING=1", "TRANSCRIPTION_API_KEY=sk-abcdefgh"}
	if got := p.set.get().transcriptionEnv(); !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
	post(url.Values{"lang": {"en"}}) // unchecked boxes, empty key: off, outgoing off, key kept
	s := p.set.get()
	if !s.TranscribeOff || s.outgoing() || s.TranscriptionKey != "sk-abcdefgh" || s.language() != "en" {
		t.Errorf("after switching off: %+v", s)
	}
	if got := (settings{}).transcriptionEnv(); len(got) != 0 {
		t.Errorf("untouched settings must not override the environment, got %q", got)
	}
}

func TestSettingsNewKeyAndSignOutEverywhere(t *testing.T) {
	p, _, mux := settingsPanel(t, "secret-token")
	old := p.cookieValue()
	w := do(mux, http.MethodPost, "/status/settings/newkey", setHost, setOrigin, old, nil)
	key := regexp.MustCompile(`<code class="key">([0-9a-f]{32})</code>`).FindStringSubmatch(w.Body.String())
	if w.Code != http.StatusOK || key == nil {
		t.Fatalf("new key not shown: status %d", w.Code)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("the page showing the key must not be cached")
	}
	data, _ := os.ReadFile(p.set.path)
	if strings.Contains(string(data), key[1]) {
		t.Error("the raw key is stored in the settings file")
	}
	fresh := w.Result().Cookies()
	if len(fresh) != 1 || fresh[0].Value != p.cookieValue() || fresh[0].Value == old {
		t.Fatalf("this browser did not get the new cookie: %+v", fresh)
	}
	if do(mux, http.MethodPost, "/status/reconnect", setHost, setOrigin, old, nil).Code != http.StatusForbidden {
		t.Error("a cookie from before the new key still works")
	}
	login := func(k string) bool {
		w := do(mux, http.MethodPost, "/status/login", setHost, setOrigin, "", url.Values{"key": {k}})
		return len(w.Result().Cookies()) == 1
	}
	if login("secret-token") {
		t.Error("the old key still signs in")
	}
	if !login(key[1]) {
		t.Error("the new key does not sign in")
	}
	if strings.Contains(do(mux, http.MethodGet, "/status/settings", setHost, "", p.cookieValue(), nil).Body.String(), key[1]) {
		t.Error("the key is shown again on a later visit")
	}

	before := p.cookieValue()
	w = do(mux, http.MethodPost, "/status/settings/signout", setHost, setOrigin, before, nil)
	if w.Code != http.StatusSeeOther || p.cookieValue() == before {
		t.Fatalf("sign out everywhere: status %d, cookie unchanged %v", w.Code, p.cookieValue() == before)
	}
	if do(mux, http.MethodPost, "/status/reconnect", setHost, setOrigin, before, nil).Code != http.StatusForbidden {
		t.Error("a cookie from before signing out everywhere still works")
	}
	if !login(key[1]) {
		t.Error("the key must keep working after signing out everywhere")
	}
}

func TestSettingsNewKeyLocksANoTokenPanel(t *testing.T) {
	p, _, mux := settingsPanel(t, "")
	const host, origin = "localhost:8080", "http://localhost:8080"
	w := do(mux, http.MethodPost, "/status/settings/newkey", host, origin, "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if do(mux, http.MethodPost, "/status/reconnect", host, origin, "", nil).Code != http.StatusForbidden {
		t.Error("with a generated key, loopback alone must not be enough")
	}
	if do(mux, http.MethodPost, "/status/reconnect", host, origin, p.cookieValue(), nil).Code != http.StatusSeeOther {
		t.Error("the cookie of the generated key does not work")
	}
}

func TestSettingsUnpair(t *testing.T) {
	p, rc, mux := settingsPanel(t, "secret-token")
	post := func(word string) *httptest.ResponseRecorder {
		return do(mux, http.MethodPost, "/status/settings/unpair", setHost, setOrigin, p.cookieValue(), url.Values{"confirm": {word}})
	}
	for _, word := range []string{"", "desconectar", "DESCONECTA"} {
		if w := post(word); w.Header().Get("Location") != "/status/settings?m=confirm" || rc.unpairs != 0 {
			t.Fatalf("confirm %q: unpaired without the exact word", word)
		}
	}
	rc.unpairErr = errUnpairOffline
	if w := post(unpairWord); w.Header().Get("Location") != "/status/settings?m=unpairfail" {
		t.Errorf("failed unpair: redirect %q", w.Header().Get("Location"))
	}
	rc.unpairErr = nil
	if w := post(" " + unpairWord + " "); w.Code != http.StatusOK || rc.unpairs != 2 || !strings.Contains(w.Body.String(), "desconectado") {
		t.Errorf("unpair: status %d, calls %d", w.Code, rc.unpairs)
	}
}

func TestSupervisorUnpair(t *testing.T) {
	h := newSupervisorHarness()
	if err := h.unpair(); !errors.Is(err, errUnpairOffline) || h.session.paired != true {
		t.Fatalf("offline: err %v, must refuse and keep the session", err)
	}
	h.session.connected = true
	h.session.unpairErr = errors.New("timeout")
	if err := h.unpair(); err == nil || h.isStopped() || len(h.pending) != 0 {
		t.Fatalf("WhatsApp did not take the request: err %v, must change nothing", err)
	}
	h.session.unpairErr = nil
	if err := h.unpair(); err != nil {
		t.Fatal(err)
	}
	if !h.isStopped() || len(h.pending) != 1 || len(h.exited) != 0 {
		t.Fatalf("after unpair: stopped %v, timers %d, exits %d; want stopped with the exit scheduled", h.isStopped(), len(h.pending), len(h.exited))
	}
	h.deleted = true
	h.pending[0]()
	if !slices.Equal(h.exited, []int{1}) {
		t.Errorf("exits = %v, want one restart", h.exited)
	}
}

// The schema is the one whatsapp-mcp-server/oauth.py creates.
func oauthTestDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp oauth.db")
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().Unix()
	if _, err := db.Exec(`CREATE TABLE clients (client_id TEXT PRIMARY KEY, body TEXT NOT NULL);
		CREATE TABLE tokens (hash TEXT PRIMARY KEY, kind TEXT NOT NULL, client_id TEXT NOT NULL, scopes TEXT NOT NULL, expires_at INTEGER NOT NULL);
		INSERT INTO clients VALUES ('c1', '{"client_name":"Claude","redirect_uris":["https://claude.ai/api/mcp/auth_callback"]}'),
			('c2', '{"redirect_uris":["https://chatgpt.com/connector_platform_oauth_redirect"]}'),
			('c3', '{"client_name":"Gone"}');
		INSERT INTO tokens VALUES ('a1', 'access', 'c1', '[]', ?), ('r1', 'refresh', 'c1', '[]', ?),
			('r2', 'refresh', 'c2', '[]', ?), ('old', 'refresh', 'c3', '[]', ?)`, now+3600, now+86400, now+7200, now-10); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConnectedApps(t *testing.T) {
	path := oauthTestDB(t)
	apps, err := listConnectedApps(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].Name != "Claude" || apps[1].Name != "ChatGPT" {
		t.Fatalf("apps = %+v, want Claude and ChatGPT (expired ones left out)", apps)
	}

	p, _, mux := settingsPanel(t, "secret-token")
	p.oauthDB = path
	if strings.Contains(do(mux, http.MethodGet, "/status/settings", setHost, "", "", nil).Body.String(), "Claude<") {
		t.Error("open page lists the connected apps")
	}
	if !strings.Contains(do(mux, http.MethodGet, "/status/settings", setHost, "", p.cookieValue(), nil).Body.String(), "Claude<small>") {
		t.Error("signed-in page does not list the connected apps")
	}
	w := do(mux, http.MethodPost, "/status/settings/revoke", setHost, setOrigin, p.cookieValue(), url.Values{"id": {"c1"}})
	if w.Header().Get("Location") != "/status/settings?m=revoked" {
		t.Fatalf("revoke: redirect %q", w.Header().Get("Location"))
	}
	apps, _ = listConnectedApps(path, time.Now())
	if len(apps) != 1 || apps[0].ID != "c2" {
		t.Errorf("after revoking c1: %+v", apps)
	}
	if _, err := listConnectedApps(filepath.Join(t.TempDir(), "absent.db"), time.Now()); err == nil {
		t.Error("a missing connector database must be an error, not an empty list or a new file")
	}
}
