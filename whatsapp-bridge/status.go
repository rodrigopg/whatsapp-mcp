package main

import (
	"database/sql"
	"html/template"
	"net/http"
	"time"
)

var (
	brt       = time.FixedZone("BRT", -3*3600)
	startedAt = time.Now()
)

// maskAccount keeps the last 4 digits: /status is open without the token, like /qr.
func maskAccount(user string) string {
	if len(user) <= 4 {
		return user
	}
	return "•••" + user[len(user)-4:]
}

// statusInfo is what /status shows. Built per request from live state, never cached.
type statusInfo struct {
	Connected bool
	Account   string // last digits of the paired account (maskAccount), empty when not paired
	HasQR     bool   // waiting for a scan
	Uptime    string
	Messages  int
	LastMsg   string // newest stored message, BRT; a stale value with Connected=true means the DB stopped recording
}

var statusTmpl = template.Must(template.New("status").Parse(`<!DOCTYPE html><html><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="10"><title>Bridge status</title>
<style>body{font-family:sans-serif;max-width:32rem;margin:2rem auto;padding:0 1rem}
.ok{color:#128c7e}.bad{color:#c0392b}td{padding:.2rem 1rem .2rem 0}img{border:8px solid #fff;box-shadow:0 2px 12px rgba(0,0,0,.2)}</style></head><body>
{{if .Connected}}<h2 class="ok">Connected</h2>{{else if .HasQR}}<h2 class="bad">Disconnected: scan to pair</h2>
<p>WhatsApp, Linked Devices, Link a Device.</p><img src="/qr.png" width="300" height="300" alt="QR">
{{else}}<h2 class="bad">Disconnected</h2><p>Waiting for a QR code. The bridge restarts itself after a logout; this page refreshes.</p>{{end}}
<table><tr><td>Account</td><td>{{if .Account}}{{.Account}}{{else}}not paired{{end}}</td></tr>
<tr><td>Up for</td><td>{{.Uptime}}</td></tr>
<tr><td>Messages stored</td><td>{{.Messages}}</td></tr>
<tr><td>Last message</td><td>{{if .LastMsg}}{{.LastMsg}}{{else}}none{{end}}</td></tr></table>
</body></html>`))

// lastMessage returns the newest stored message time in BRT ("" when the table is empty or unparsable).
func lastMessage(db *sql.DB) (n int, last string) {
	var raw sql.NullString
	if db.QueryRow(`SELECT COUNT(*), MAX(timestamp) FROM messages`).Scan(&n, &raw) != nil || !raw.Valid {
		return n, ""
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999"} {
		if t, err := time.Parse(layout, raw.String); err == nil {
			return n, t.In(brt).Format("02/01 15:04:05 BRT")
		}
	}
	return n, raw.String
}

func handleStatus(info func() statusInfo) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		statusTmpl.Execute(w, info())
	}
}
