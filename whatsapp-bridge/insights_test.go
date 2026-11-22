package main

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

func insightsDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1) // one in-memory database, not one per connection
	if _, err := db.Exec(`CREATE TABLE chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP, is_from_me BOOLEAN, media_type TEXT);` + matterSchema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

const (
	ana   = "5511900001111@s.whatsapp.net"
	bruno = "5511900002222@s.whatsapp.net"
	grupo = "120363000000000001@g.us"
)

// Sunday 22/11/2026 12:00 BRT.
var insightsNow = time.Date(2026, 11, 22, 12, 0, 0, 0, brt)

func seedInsights(t *testing.T, db *sql.DB) {
	t.Helper()
	db.Exec(`INSERT INTO chats (jid, name) VALUES (?, 'Ana Souza'), (?, ''), (?, 'Grupo da Rua')`, ana, bruno, grupo)
	n := 0
	msg := func(chat, at string, mine bool, media, content string) {
		n++
		if _, err := db.Exec(`INSERT INTO messages VALUES (?, ?, '', ?, ?, ?, ?)`, n, chat, content, at, mine, media); err != nil {
			t.Fatal(err)
		}
	}
	// Friday 20/11, three timestamp spellings the bridge has written over time.
	msg(ana, "2026-11-20 09:00:00-03:00", false, "", "oi")
	msg(ana, "2026-11-20T12:10:00Z", true, "", "oi!")                // 09:10 BRT: answered in 10 min
	msg(ana, "2026-11-20 09:30:00.123456789-03:00", false, "", "?")  //
	msg(ana, "2026-11-20 10:30:00-03:00", true, "", "sim")           // answered in 60 min
	msg(ana, "2026-11-20 10:31:00-03:00", true, "", "e mais")        // owner after owner: not an answer
	msg(bruno, "2026-11-21 23:50:00-03:00", false, "audio", "texto") // Saturday night, transcribed
	msg(bruno, "2026-11-22 02:30:00Z", false, "audio", "")           // still Saturday 23:30 in BRT
	msg(bruno, "2026-11-22 08:00:00-03:00", true, "", "ok")          // Sunday; answered after 8 h 30
	for i := 0; i < 4; i++ {
		msg(grupo, "2026-11-22 10:00:00-03:00", false, "", "bom dia") // group: counts, but no response time
	}
	msg(grupo, "2026-11-22 10:05:00-03:00", true, "", "bom dia")
	msg("status@broadcast", "2026-11-22 10:00:00-03:00", false, "", "status")
	msg("120363000000000009@newsletter", "2026-11-22 10:00:00-03:00", false, "", "canal")
	msg(ana, "2026-10-01 10:00:00-03:00", false, "", "antiga") // outside 7 and 30 days, inside 90

	matter := func(chat, owedBy, created string, closed any) {
		if _, err := db.Exec(`INSERT INTO matters (chat_jid, owed_by, origin, origin_message_id, snippet, created_at, certainty, valid_until, closed_how)
			VALUES (?, ?, 'request', 'm', ?, ?, 'clear', ?, ?)`, chat, owedBy, "Pediu o orçamento", created, "2026-12-20T00:00:00Z", closed); err != nil {
			t.Fatal(err)
		}
	}
	matter(ana, "owner", "2026-11-20T12:00:00Z", nil)     // open for 2 days
	matter(bruno, "owner", "2026-11-22T12:00:00Z", nil)   // open for 3 h
	matter(grupo, "other", "2026-11-22T12:00:00Z", nil)   // they owe the owner: not "waiting for you"
	matter(ana, "owner", "2026-11-21T12:00:00Z", "owner") // closed
}

func TestLoadInsights(t *testing.T) {
	db := insightsDB(t)
	seedInsights(t, db)
	in, err := loadInsights(db, 7, insightsNow)
	if err != nil {
		t.Fatal(err)
	}
	if in.Rec != 8 || in.Sent != 5 || in.Chats != 3 || in.Groups != 1 {
		t.Errorf("totals: rec %d sent %d chats %d groups %d", in.Rec, in.Sent, in.Chats, in.Groups)
	}
	if in.Audio != 2 || in.Transcribed != 1 {
		t.Errorf("audio %d transcribed %d", in.Audio, in.Transcribed)
	}
	if in.Reply != "1 h" { // gaps 10 min, 60 min, 8 h 30: the median
		t.Errorf("reply median = %q", in.Reply)
	}
	if len(in.Daily) != 7 || in.Daily[0].Label != "16/11" || in.Daily[6].Label != "22/11" {
		t.Fatalf("daily window wrong: %+v", in.Daily)
	}
	want := map[string]dayCount{"20/11": {Rec: 2, Sent: 3}, "21/11": {Rec: 2}, "22/11": {Rec: 4, Sent: 2}, "19/11": {}}
	for _, d := range in.Daily {
		if w, ok := want[d.Label]; ok && (d.Rec != w.Rec || d.Sent != w.Sent) {
			t.Errorf("%s = %d/%d, want %d/%d (days are BRT days)", d.Label, d.Rec, d.Sent, w.Rec, w.Sent)
		}
	}
	// Monday first: Friday is row 4, Saturday 5, Sunday 6; hours are BRT.
	if in.Heat[4][9] != 3 || in.Heat[5][23] != 2 || in.Heat[6][10] != 5 || in.Heat[6][8] != 1 {
		t.Errorf("heat wrong: fri9=%d sat23=%d sun10=%d sun8=%d", in.Heat[4][9], in.Heat[5][23], in.Heat[6][10], in.Heat[6][8])
	}
	if len(in.Top) != 3 || in.Top[0].Name != "Grupo da Rua" || in.Top[0].Count != 5 || in.Top[0].Pct != 100 || in.Top[1].Name != "Ana Souza" {
		t.Errorf("top chats wrong: %+v", in.Top)
	}
	if in.Top[2].Name != "•••2222" { // no name stored: never the full number
		t.Errorf("unnamed chat shown as %q", in.Top[2].Name)
	}
	if len(in.Waiting) != 2 || in.WaitingOld != 1 || in.Waiting[0].Name != "•••2222" || in.Waiting[0].Since != "há 3 h" || in.Waiting[1].Since != "há 2 dias" {
		t.Errorf("waiting wrong: old %d %+v", in.WaitingOld, in.Waiting)
	}

	if in90, _ := loadInsights(db, 90, insightsNow); in90.Rec != 9 || len(in90.Daily) != 90 {
		t.Errorf("90 days: rec %d, %d days", in90.Rec, len(in90.Daily))
	}
	empty, err := loadInsights(insightsDB(t), 30, insightsNow)
	if err != nil || empty.Rec+empty.Sent != 0 || empty.Reply != "" || len(empty.Daily) != 30 || len(empty.Top) != 0 {
		t.Errorf("empty database: %v %+v", err, empty)
	}
}

func TestInsightsChart(t *testing.T) {
	bars, grid := buildChart([]dayCount{{Label: "21/11", Rec: 30, Sent: 12}, {Label: "22/11"}})
	if len(grid) != 3 || grid[0].Value != 0 || grid[1].Value != 25 || grid[2].Value != 50 || grid[2].Y != "6.0" || grid[0].Y != "152.0" {
		t.Errorf("grid wrong: %+v", grid)
	}
	// 42 of 50 on a 146px plot: the stack tops out at 6 + 146*(1-0.84) = 29.4, received starts at 64.4.
	if b := bars[0]; b.SentY != "29.4" || b.RecY != "64.4" || b.RecH != "87.6" || b.Tip != "21/11: 30 recebidas, 12 enviadas" || b.Label != "21/11" {
		t.Errorf("bar wrong: %+v", b)
	}
	if b := bars[1]; b.RecH != "0.0" || b.SentH != "0.0" {
		t.Errorf("empty day draws something: %+v", b)
	}
	var heat [7][24]int
	heat[0][9], heat[0][10], heat[6][23] = 100, 26, 1
	rows := buildHeat(heat)
	if rows[0].Cells[9].Level != 4 || rows[0].Cells[10].Level != 2 || rows[6].Cells[23].Level != 1 || rows[1].Cells[0].Level != 0 || rows[6].Day != "Dom" {
		t.Errorf("heat levels wrong")
	}
	if buildHeat([7][24]int{})[0].Cells[0].Level != 0 {
		t.Error("empty heat map must not divide by zero or light up")
	}
}

func TestInsightsPageOnlyWhenSignedIn(t *testing.T) {
	db := insightsDB(t)
	seedInsights(t, db)
	p, _, mux := testPanel("secret-token", statusInfo{Connected: true, Account: "•••4821"})
	p.db, p.now = db, func() time.Time { return insightsNow }

	open := do(mux, "GET", "/status/insights?d=7", "wa.example.test", "", "", nil).Body.String()
	for _, leak := range []string{"Ana Souza", "Grupo da Rua", "Pediu o orçamento", "2222", "recebidas", "<svg"} {
		if strings.Contains(open, leak) {
			t.Errorf("open insights page leaks %q", leak)
		}
	}
	if !strings.Contains(open, `action="/status/login"`) {
		t.Error("open insights page has no sign-in form")
	}

	in := do(mux, "GET", "/status/insights?d=7", "wa.example.test", "", p.cookieValue(), nil).Body.String()
	for _, want := range []string{"Ana Souza", "Pediu o orçamento", "8 recebidas · 5 enviadas", "<svg", `aria-current="true">7 dias`, "20/11: 2 recebidas, 3 enviadas", "1 (50%)", "há 2 dias"} {
		if !strings.Contains(in, want) {
			t.Errorf("signed-in insights page lacks %q", want)
		}
	}
	if strings.Contains(in, "5511900001111") || strings.Contains(in, "5511900002222") {
		t.Error("insights page shows a full phone number")
	}

	hidden := do(mux, "GET", "/status/insights?d=7&hide=1", "wa.example.test", "", p.cookieValue(), nil).Body.String()
	for _, leak := range []string{"Ana Souza", "Grupo da Rua", "Pediu o orçamento", "2222"} {
		if strings.Contains(hidden, leak) {
			t.Errorf("hide=1 still shows %q", leak)
		}
	}
	if !strings.Contains(hidden, "Conversa 1") || !strings.Contains(hidden, "?d=30&amp;hide=1") {
		t.Error("hide=1 page lacks the placeholders or drops hide from the period links")
	}

	if b := do(mux, "GET", "/status/insights?d=9999", "wa.example.test", "", p.cookieValue(), nil).Body.String(); !strings.Contains(b, `aria-current="true">30 dias`) {
		t.Error("an unknown period must fall back to 30 days")
	}
	if w := do(mux, "POST", "/status/insights", "wa.example.test", "https://wa.example.test", p.cookieValue(), nil); w.Code != 405 {
		t.Errorf("POST = %d, want 405", w.Code)
	}
	if b := do(mux, "GET", "/status", "wa.example.test", "", "", nil).Body.String(); !strings.Contains(b, `href="/status/insights"`) {
		t.Error("home page has no link to insights")
	}
}
