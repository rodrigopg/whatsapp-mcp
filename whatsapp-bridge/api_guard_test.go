package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func guardCode(h http.Handler, method, path, host string, headers ...string) int {
	r := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	r.Host = host
	for i := 0; i+1 < len(headers); i += 2 {
		r.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec.Code
}

func TestAPIGuardNoToken(t *testing.T) {
	hits := 0
	h := apiGuard("", apiHosts(""), http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	for _, c := range []struct {
		name, path, host string
		headers          []string
		want             int
	}{
		{"local client, 127.0.0.1", "/api/send", "127.0.0.1:8080", nil, 200},
		{"local client, localhost", "/api/send", "localhost:8080", nil, 200},
		{"local client, ipv6", "/api/send", "[::1]:8080", nil, 200},
		{"host is case-insensitive", "/api/send", "LOCALHOST:8080", nil, 200},
		{"cross-origin page", "/api/send", "127.0.0.1:8080", []string{"Origin", "https://example.com", "Content-Type", "text/plain"}, 403},
		{"same-origin page still carries Origin", "/api/send", "127.0.0.1:8080", []string{"Origin", "http://127.0.0.1:8080"}, 403},
		{"opaque origin", "/api/send", "127.0.0.1:8080", []string{"Origin", "null"}, 403},
		{"cross-site request without Origin", "/api/chats", "127.0.0.1:8080", []string{"Sec-Fetch-Site", "cross-site"}, 403},
		{"typed in the address bar", "/api/chats", "127.0.0.1:8080", []string{"Sec-Fetch-Site", "none"}, 200},
		{"foreign Host (DNS rebinding)", "/api/messages", "example.com:8080", nil, 403},
		{"foreign Host that starts like a local one", "/api/messages", "localhost.example.com:8080", nil, 403},
		{"empty Host", "/api/messages", "", nil, 403},
		{"LAN address is not a no-token host", "/api/messages", "192.168.1.10:8080", nil, 403},
		{"another published port (container)", "/api/messages", "localhost:9999", nil, 200},
		{"outside /api/ is not guarded here", "/status", "example.com:8080", []string{"Origin", "https://example.com"}, 200},
	} {
		if got := guardCode(h, http.MethodPost, c.path, c.host, c.headers...); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
	if hits != 7 {
		t.Errorf("handler reached %d times, want 7", hits)
	}
}

func TestAPIGuardNoTokenExtraHosts(t *testing.T) {
	h := apiGuard("", apiHosts("bridge.internal, Proxy.example:8443"), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	for host, want := range map[string]int{
		"bridge.internal:8080": 200, "proxy.example": 200, "PROXY.example:8443": 200, "localhost:8080": 200,
		"other.example:8080": 403,
	} {
		if got := guardCode(h, http.MethodPost, "/api/send", host); got != want {
			t.Errorf("%s: got %d, want %d", host, got, want)
		}
	}
}

// With a token the bearer is the whole check: a page cannot attach it, so the bridge may be
// reached by any name (LAN address behind BIND_ADDR=0.0.0.0, DNS name, another container).
func TestAPIGuardWithToken(t *testing.T) {
	h := apiGuard("tok", apiHosts(""), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	auth := []string{"Authorization", "Bearer tok"}
	for _, c := range []struct {
		name, host string
		headers    []string
		want       int
	}{
		{"loopback", "127.0.0.1:8082", auth, 200},
		{"LAN address", "192.168.1.10:8082", auth, 200},
		{"DNS name", "bridge.example.com", auth, 200},
		{"container name", "bridge:8080", auth, 200},
		{"Origin is fine once the token is there", "100.64.0.1:8082", append([]string{"Origin", "https://example.com"}, auth...), 200},
		{"no token", "127.0.0.1:8082", nil, 401},
		{"no token, any host", "bridge.example.com", nil, 401},
		{"wrong token", "127.0.0.1:8082", []string{"Authorization", "Bearer nope"}, 401},
		{"token prefix", "127.0.0.1:8082", []string{"Authorization", "Bearer to"}, 401},
		{"token without scheme", "127.0.0.1:8082", []string{"Authorization", "tok"}, 401},
	} {
		if got := guardCode(h, http.MethodPost, "/api/send", c.host, c.headers...); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// The Python MCP server and transcribe.py use python-requests against the base URL the bridge
// itself hands them (http://<BIND_ADDR>:<port>/api, default http://localhost:8080/api): a real
// HTTP round trip with that client's headers must pass, with and without a token.
func TestAPIGuardLetsPythonClientsThrough(t *testing.T) {
	for _, token := range []string{"", "tok"} {
		var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
		srv := httptest.NewServer(apiGuard(token, apiHosts(""), h))
		_, port, _ := strings.Cut(srv.Listener.Addr().String(), ":")
		for _, base := range []string{"http://127.0.0.1:" + port, "http://localhost:" + port} {
			req, _ := http.NewRequest(http.MethodPost, base+"/api/chats", strings.NewReader(`{"limit":1}`))
			req.Header.Set("User-Agent", "python-requests/2.32.3")
			req.Header.Set("Accept", "*/*")
			req.Header.Set("Content-Type", "application/json")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Errorf("token=%q %s: got %d", token, base, resp.StatusCode)
			}
		}
		srv.Close()
	}
}
