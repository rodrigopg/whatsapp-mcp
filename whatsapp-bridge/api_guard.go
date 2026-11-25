package main

import (
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
)

// maxAPIBody caps /api/ request bodies. They are JSON with text, JIDs and file paths, never file
// bytes, so 1 MiB is far above any real request; without a cap one request could make the bridge
// buffer an arbitrarily large body while decoding it.
const maxAPIBody = 1 << 20

// apiHosts lists the host names an /api/ request may carry in its Host header when no token is
// set: loopback (the only bind address allowed without a token), plus API_ALLOWED_HOSTS (comma
// separated names, no port) for a bridge reached through another name, e.g. a local reverse proxy.
// The port is not compared: it is whatever the client connected to (a container may publish
// another one), and it is the name that tells a foreign page from a local client.
func apiHosts(extra string) map[string]bool {
	hosts := map[string]bool{"127.0.0.1": true, "localhost": true, "::1": true}
	for _, h := range strings.Split(extra, ",") {
		if h = hostName(h); h != "" {
			hosts[h] = true
		}
	}
	return hosts
}

// hostName is a Host header (or a configured name) without port or brackets, lower case.
func hostName(hostport string) string {
	host := strings.TrimSpace(hostport)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.ToLower(strings.Trim(host, "[]"))
}

// apiGuard is the single gate in front of every /api/ route, for every caller.
//
// With a token: the bearer token is required, compared in constant time. A web page cannot attach
// it to a request, so nothing else is checked and the bridge may be reached by any name.
//
// With no token (loopback bind only):
//   - the Host header must be one of hosts, so a page that re-points its own name at the bridge
//     (DNS rebinding) is refused;
//   - a request that a browser made on behalf of a page (it carries Origin, or Sec-Fetch-Site
//     other than none/same-origin) is refused: the local API is for programs, and no legitimate
//     client of it is a web page.
//
// /qr, /status and /healthz are not handled here (the panel has its own checks in panel.go).
func apiGuard(token string, hosts map[string]bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.ContentLength > maxAPIBody {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxAPIBody)
		if token != "" {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		if !hosts[hostName(r.Host)] {
			http.Error(w, "Forbidden: unexpected Host header (see API_ALLOWED_HOSTS)", http.StatusForbidden)
			return
		}
		site := r.Header.Get("Sec-Fetch-Site")
		if r.Header.Get("Origin") != "" || (site != "" && site != "none" && site != "same-origin") {
			http.Error(w, "Forbidden: browser requests are not accepted without API_AUTH_TOKEN", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
