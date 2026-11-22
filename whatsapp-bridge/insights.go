package main

import (
	"database/sql"
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Not conversations: status updates and channels.
const insightChats = `chat_jid NOT LIKE '%@broadcast' AND chat_jid NOT LIKE '%@newsletter'`

type dayCount struct {
	Label     string // DD/MM, BRT
	Rec, Sent int
}

type topChat struct {
	Name  string
	Count int
	Pct   int // bar length against the busiest chat
}

type waitingItem struct {
	Name, Snippet, Since string
}

// insights is everything /status/insights shows, computed from messages.db on each request.
type insights struct {
	Rec, Sent, Chats, Groups int
	Reply                    string // median time to answer in direct chats; "" when there is no pair to measure
	Daily                    []dayCount
	Heat                     [7][24]int // Monday first, BRT hour
	Top                      []topChat
	Waiting                  []waitingItem
	WaitingOld               int // open for more than a day
	Audio, Transcribed       int
}

// ponytail: every query filters on julianday(timestamp), a full scan of messages per page view.
// Fine for one owner and a few hundred thousand rows; add a stored epoch column if it gets slow.
func loadInsights(db *sql.DB, days int, now time.Time) (insights, error) {
	var in insights
	now = now.In(brt)
	first := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, brt).AddDate(0, 0, -(days - 1))
	where := ` FROM messages WHERE ` + insightChats + ` AND julianday(timestamp) >= julianday(?)`
	cut := first.UTC().Format(time.RFC3339)

	if err := db.QueryRow(`SELECT COALESCE(SUM(is_from_me = 0), 0), COALESCE(SUM(is_from_me = 1), 0), COUNT(DISTINCT chat_jid),
		COUNT(DISTINCT CASE WHEN chat_jid LIKE '%@g.us' THEN chat_jid END),
		COALESCE(SUM(media_type = 'audio' AND is_from_me = 0), 0),
		COALESCE(SUM(media_type = 'audio' AND is_from_me = 0 AND COALESCE(content, '') <> ''), 0)`+where, cut).
		Scan(&in.Rec, &in.Sent, &in.Chats, &in.Groups, &in.Audio, &in.Transcribed); err != nil {
		return in, err
	}

	byDay := map[string]dayCount{}
	rows, err := db.Query(`SELECT strftime('%d/%m', timestamp, '-3 hours'), CAST(strftime('%w', timestamp, '-3 hours') AS INTEGER),
		CAST(strftime('%H', timestamp, '-3 hours') AS INTEGER), SUM(is_from_me = 0), SUM(is_from_me = 1)`+where+` GROUP BY 1, 2, 3`, cut)
	if err != nil {
		return in, err
	}
	defer rows.Close()
	for rows.Next() {
		var day sql.NullString
		var wd, hour, rec, sent int
		if err := rows.Scan(&day, &wd, &hour, &rec, &sent); err != nil {
			return in, err
		}
		if !day.Valid { // a timestamp SQLite cannot parse
			continue
		}
		d := byDay[day.String]
		d.Rec, d.Sent = d.Rec+rec, d.Sent+sent
		byDay[day.String] = d
		in.Heat[(wd+6)%7][hour] += rec + sent // %w starts on Sunday
	}
	for d := first; !d.After(now); d = d.AddDate(0, 0, 1) {
		c := byDay[d.Format("02/01")]
		c.Label = d.Format("02/01")
		in.Daily = append(in.Daily, c)
	}

	top, err := db.Query(`SELECT m.chat_jid, COALESCE(c.name, ''), COUNT(*)`+strings.Replace(where, "FROM messages", "FROM messages m LEFT JOIN chats c ON c.jid = m.chat_jid", 1)+
		` GROUP BY m.chat_jid ORDER BY 3 DESC, m.chat_jid LIMIT 5`, cut)
	if err != nil {
		return in, err
	}
	defer top.Close()
	for top.Next() {
		var jid, name string
		var n int
		if err := top.Scan(&jid, &name, &n); err != nil {
			return in, err
		}
		in.Top = append(in.Top, topChat{Name: chatLabel(jid, name), Count: n})
	}
	for i := range in.Top {
		in.Top[i].Pct = max(in.Top[i].Count*100/in.Top[0].Count, 1)
	}

	// Seconds between someone's message and the owner's next one in the same direct chat.
	gaps, err := db.Query(`SELECT (ts - prev_ts) * 86400 FROM (
		SELECT is_from_me, julianday(timestamp) AS ts, LAG(is_from_me) OVER w AS prev_me, LAG(julianday(timestamp)) OVER w AS prev_ts
		FROM messages WHERE chat_jid LIKE '%@s.whatsapp.net' AND julianday(timestamp) >= julianday(?)
		WINDOW w AS (PARTITION BY chat_jid ORDER BY julianday(timestamp))) WHERE is_from_me = 1 AND prev_me = 0`, cut)
	if err != nil {
		return in, err
	}
	defer gaps.Close()
	var secs []float64
	for gaps.Next() {
		var s float64
		if err := gaps.Scan(&s); err != nil {
			return in, err
		}
		secs = append(secs, s)
	}
	if len(secs) > 0 {
		sort.Float64s(secs)
		in.Reply = shortDuration((time.Duration(secs[len(secs)/2] * float64(time.Second))).Round(time.Second))
	}

	matters, err := loadMatters(db, "")
	if err != nil {
		return in, err
	}
	for i := len(matters) - 1; i >= 0; i-- { // newest first
		m := matters[i]
		if m.OwedBy != "owner" || matterState(m, now) != "open" {
			continue
		}
		if now.Sub(m.CreatedAt) > 24*time.Hour {
			in.WaitingOld++
		}
		var name string
		db.QueryRow(`SELECT COALESCE(name, '') FROM chats WHERE jid = ?`, m.ChatJID).Scan(&name)
		in.Waiting = append(in.Waiting, waitingItem{Name: chatLabel(m.ChatJID, name), Snippet: m.Snippet, Since: "há " + shortDuration(now.Sub(m.CreatedAt))})
	}
	return in, nil
}

// chatLabel never shows a full phone number: a chat without a name gets its last digits.
func chatLabel(jid, name string) string {
	user, _, _ := strings.Cut(jid, "@")
	if name == "" || name == jid || name == user {
		return maskAccount(user)
	}
	return name
}

func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h", int(d.Hours()))
	}
	return fmt.Sprintf("%d dias", int(d.Hours()/24))
}

type chartBar struct {
	X, W, HitX, HitW         string
	RecY, RecH, SentY, SentH string
	Tip, Label               string
}

type chartGrid struct {
	Y, TextY string
	Value    int
}

type heatRow struct {
	Day   string
	Cells []heatCell
}

type heatCell struct {
	Level int
	Tip   string
}

type insightsView struct {
	Account          string
	Authed, HasToken bool
	Days             int
	Hide             bool
	Periods          []int
	Failed           bool
	insights
	Bars    []chartBar
	Grid    []chartGrid
	HeatMap []heatRow
}

func (v insightsView) Total() int           { return v.Rec + v.Sent }
func (v insightsView) Empty() bool          { return v.Total() == 0 }
func (v insightsView) LastWeek() []dayCount { return v.Daily[max(len(v.Daily)-7, 0):] }
func (v insightsView) TranscribedPct() int {
	if v.Audio == 0 {
		return 0
	}
	return v.Transcribed * 100 / v.Audio
}

// Chart box: 600 x 170, plot from x=30 to 600 and y=6 to 152.
const chartLeft, chartRight, chartTop, chartBottom = 30.0, 600.0, 6.0, 152.0

func chartScale(daily []dayCount) (peak int) {
	for _, d := range daily {
		peak = max(peak, d.Rec+d.Sent)
	}
	step := 10
	if peak > 100 {
		step = 50
	}
	return max((peak+step-1)/step*step, 10) // even, so the middle gridline is a whole number
}

func buildChart(daily []dayCount) (bars []chartBar, grid []chartGrid) {
	peak := chartScale(daily)
	y := func(v int) float64 { return chartTop + (chartBottom-chartTop)*(1-float64(v)/float64(peak)) }
	f := func(v float64) string { return fmt.Sprintf("%.1f", v) }
	for _, v := range []int{0, peak / 2, peak} {
		grid = append(grid, chartGrid{Y: f(y(v)), TextY: f(y(v) + 3), Value: v})
	}
	slot := (chartRight - chartLeft) / float64(len(daily))
	every := (len(daily) + 5) / 6 // about six date labels
	for i, d := range daily {
		x := chartLeft + float64(i)*slot
		b := chartBar{X: f(x + 1), W: f(max(slot-2, 1)), HitX: f(x), HitW: f(slot),
			RecY: f(y(d.Rec)), RecH: f(y(0) - y(d.Rec)),
			SentY: f(y(d.Rec + d.Sent)), SentH: f(max(y(d.Rec)-y(d.Rec+d.Sent)-2, 0)), // 2px gap between the two series
			Tip: fmt.Sprintf("%s: %d recebidas, %d enviadas", d.Label, d.Rec, d.Sent)}
		if i%every == 0 {
			b.Label = d.Label
		}
		bars = append(bars, b)
	}
	return
}

func buildHeat(heat [7][24]int) []heatRow {
	peak := 0
	for _, row := range heat {
		for _, n := range row {
			peak = max(peak, n)
		}
	}
	out := make([]heatRow, 7)
	for i, day := range []string{"Seg", "Ter", "Qua", "Qui", "Sex", "Sáb", "Dom"} {
		out[i].Day = day
		for h, n := range heat[i] {
			level := 0
			if n > 0 {
				level = min(max((n*4+peak-1)/peak, 1), 4) // quarters of the busiest hour
			}
			out[i].Cells = append(out[i].Cells, heatCell{Level: level, Tip: fmt.Sprintf("%s %02dh: %d mensagens", day, h, n)})
		}
	}
	return out
}

func (p *panel) insightsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	v := insightsView{Account: p.info().Account, Authed: p.authed(r), HasToken: p.keyed(), Days: 30, Periods: []int{7, 30, 90},
		Hide: r.URL.Query().Get("hide") == "1"}
	if d := r.URL.Query().Get("d"); d == "7" || d == "90" {
		fmt.Sscan(d, &v.Days)
	}
	if v.Authed && p.db != nil { // nothing about the conversations may reach an open page
		in, err := loadInsights(p.db, v.Days, p.now())
		if err != nil {
			fmt.Printf("insights: %v\n", err)
			v.Failed = true
		}
		if v.Hide {
			for i := range in.Top {
				in.Top[i].Name = fmt.Sprintf("Conversa %d", i+1)
			}
			for i := range in.Waiting {
				in.Waiting[i].Name, in.Waiting[i].Snippet = fmt.Sprintf("Conversa %d", i+1), "(texto oculto)"
			}
		}
		v.insights = in
		if len(in.Daily) > 0 {
			v.Bars, v.Grid = buildChart(in.Daily)
		}
		v.HeatMap = buildHeat(in.Heat)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	panelTmpl.ExecuteTemplate(w, "insights", v)
}

var _ = template.Must(panelTmpl.New("insights").Parse(`<!DOCTYPE html><html lang="pt-BR"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Insights · Seu WhatsApp</title>
<style>{{template "css"}}
:root{--c1:#2a78d6;--c2:#eb6834;--h0:#e8eef5;--h1:#b7d3f6;--h2:#6da7ec;--h3:#2a78d6;--h4:#104281}
@media (prefers-color-scheme:dark){:root{--c1:#3987e5;--c2:#d95926;--h0:#1d2a36;--h1:#0d366b;--h2:#256abf;--h3:#5598e7;--h4:#b7d3f6}}
.wide{display:grid;gap:14px;min-width:0}.bar{display:flex;justify-content:space-between;align-items:center;flex-wrap:wrap;gap:8px}
.seg{display:inline-flex;gap:2px;border:1px solid var(--line);border-radius:8px;padding:2px;background:var(--card)}
.seg a{font-size:.8rem;font-weight:500;color:var(--fg);text-decoration:none;padding:4px 10px;border-radius:6px}
.seg a[aria-current]{background:var(--brand);color:var(--brand-ink)}.toggle{font-size:.84rem;color:var(--brand)}
.kpis{display:grid;grid-template-columns:repeat(2,minmax(0,1fr));gap:10px}@media (min-width:860px){.kpis{grid-template-columns:repeat(4,minmax(0,1fr))}}
.kpi{background:var(--card);border:1px solid var(--line);border-radius:12px;padding:12px;display:grid;gap:2px;min-width:0}
.kpi b{font-size:1.5rem;line-height:1.1;font-variant-numeric:tabular-nums}.kpi span{font-size:.8rem;color:var(--muted)}.kpi small{font-size:.74rem;color:var(--muted)}
svg{width:100%;height:auto;display:block}svg text{font-size:10px;fill:var(--muted)}.grid{stroke:var(--line)}.hit{fill:transparent}g:hover .hit{fill:var(--line);opacity:.6}
.legend{display:flex;gap:14px;flex-wrap:wrap;font-size:.8rem;color:var(--muted)}.legend i{display:inline-block;width:10px;height:10px;border-radius:3px;margin-right:5px}
.heat{display:grid;grid-template-columns:auto repeat(24,minmax(0,1fr));gap:2px;align-items:center;font-size:.7rem;color:var(--muted)}
.heat i{aspect-ratio:1;border-radius:2px;min-width:0}.heat .hl{grid-column:span 6}
.h0{background:var(--h0)}.h1{background:var(--h1)}.h2{background:var(--h2)}.h3{background:var(--h3)}.h4{background:var(--h4)}
.bars{display:grid;gap:8px;margin:0;padding:0;list-style:none}.bars li{display:grid;grid-template-columns:minmax(0,9.5rem) 1fr auto;gap:10px;align-items:center;font-size:.86rem}
.bars span:first-child{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}.bars i{display:block;height:10px;background:var(--c1);border-radius:0 4px 4px 0}
.bars span:last-child{font-variant-numeric:tabular-nums;color:var(--muted)}
.wait{display:grid;gap:8px;margin:0;padding:0;list-style:none;font-size:.88rem}.wait li{display:flex;justify-content:space-between;gap:10px;border-bottom:1px solid var(--line);padding-bottom:8px}
.wait li:last-child{border:0;padding:0}.wait small{color:var(--muted);display:block;overflow-wrap:anywhere}.wait li>span:last-child{white-space:nowrap}
</style></head><body><div class="p">
<header><b>Seu WhatsApp</b>{{if .Account}}<span class="acct">{{.Account}}</span>{{end}}</header>
{{template "nav" "insights"}}
{{if not .Authed}}<div class="wide"><section class="card"><h2>Insights</h2>
{{if .HasToken}}<p class="sub">Os números das suas conversas só aparecem para quem entrou com a chave do painel.</p>
<form class="field" method="post" action="/status/login"><input id="key" name="key" type="password" autocomplete="current-password" placeholder="Chave do painel" aria-label="Chave do painel" required><button class="primary">Entrar</button></form>
{{else}}<p class="sub">Esta bridge não tem chave configurada, então o painel só abre no próprio computador, pelo endereço localhost.</p>{{end}}</section></div>
{{else}}
<div class="wide"><div class="bar"><div class="seg" role="group" aria-label="Período">{{range .Periods}}<a href="?d={{.}}{{if $.Hide}}&amp;hide=1{{end}}"{{if eq . $.Days}} aria-current="true"{{end}}>{{.}} dias</a>{{end}}</div>
<a class="toggle" href="?d={{.Days}}{{if not .Hide}}&amp;hide=1{{end}}">{{if .Hide}}Mostrar nomes{{else}}Ocultar nomes{{end}}</a></div>
{{if .Failed}}<p class="flash bad" role="status">Não deu para calcular todos os números agora. Tente de novo em instantes.</p>{{end}}
<div class="kpis">
<div class="kpi"><span>Mensagens</span><b>{{.Total}}</b><small>{{.Rec}} recebidas · {{.Sent}} enviadas</small></div>
<div class="kpi"><span>Conversas ativas</span><b>{{.Chats}}</b><small>{{.Groups}} grupos</small></div>
<div class="kpi"><span>Você responde em</span><b>{{if .Reply}}{{.Reply}}{{else}}–{{end}}</b><small>mediana, conversas diretas</small></div>
<div class="kpi"><span>Esperando você</span><b>{{len .Waiting}}</b><small>{{.WaitingOld}} há mais de um dia</small></div>
</div>
{{if .Empty}}<section class="card"><p class="sub">Ainda não há mensagens guardadas neste período.</p></section>{{else}}
<section class="card"><div class="bar"><h2>Mensagens por dia</h2><div class="legend"><span><i style="background:var(--c1)"></i>Recebidas</span><span><i style="background:var(--c2)"></i>Enviadas</span></div></div>
<svg viewBox="0 0 600 170" role="img" aria-label="Mensagens recebidas e enviadas por dia">
{{range .Grid}}<line class="grid" x1="30" x2="600" y1="{{.Y}}" y2="{{.Y}}"/><text x="25" y="{{.TextY}}" text-anchor="end">{{.Value}}</text>{{end}}
{{range .Bars}}<g><title>{{.Tip}}</title><rect class="hit" x="{{.HitX}}" y="6" width="{{.HitW}}" height="146"/><rect x="{{.X}}" y="{{.RecY}}" width="{{.W}}" height="{{.RecH}}" fill="var(--c1)"/><rect x="{{.X}}" y="{{.SentY}}" width="{{.W}}" height="{{.SentH}}" rx="2" fill="var(--c2)"/>{{if .Label}}<text x="{{.X}}" y="166">{{.Label}}</text>{{end}}</g>{{end}}
</svg>
<details><summary>Ver como tabela (últimos 7 dias)</summary><dl>{{range .LastWeek}}<dt>{{.Label}}</dt><dd>{{.Rec}} recebidas · {{.Sent}} enviadas</dd>{{end}}</dl></details></section>{{end}}
</div>
{{if not .Empty}}<section class="card"><h2>Quando você mais conversa</h2>
<div class="heat">{{range .HeatMap}}<span>{{.Day}}</span>{{range .Cells}}<i class="h{{.Level}}" title="{{.Tip}}"></i>{{end}}{{end}}<span></span><span class="hl">0h</span><span class="hl">6h</span><span class="hl">12h</span><span class="hl">18h</span></div>
<div class="legend"><span>menos <i class="h0"></i><i class="h1"></i><i class="h2"></i><i class="h3"></i><i class="h4"></i>mais</span></div></section>
<section class="card"><h2>Conversas mais movimentadas</h2><ol class="bars">{{range .Top}}<li title="{{.Name}}: {{.Count}} mensagens"><span>{{.Name}}</span><span><i style="width:{{.Pct}}%"></i></span><span>{{.Count}}</span></li>{{end}}</ol></section>{{end}}
<section class="card"><h2>Esperando sua resposta</h2>{{if .Waiting}}<ul class="wait">{{range .Waiting}}<li><span>{{.Name}}<small>{{.Snippet}}</small></span><span class="hint">{{.Since}}</span></li>{{end}}</ul>
{{else}}<p class="sub">Nada pendente com você.</p>{{end}}<p class="fine">Vem das pendências que a IA registra para o resumo da manhã.</p></section>
<section class="card"><h2>Áudios recebidos</h2><dl><dt>Recebidos</dt><dd>{{.Audio}}</dd><dt>Transcritos</dt><dd>{{.Transcribed}}{{if .Audio}} ({{.TranscribedPct}}%){{end}}</dd></dl></section>
<p class="fine wide">Tudo é calculado neste servidor a partir das mensagens guardadas. Nenhum número sai daqui.</p>
{{end}}
<footer><span>Horários em BRT</span>{{if and .Authed .HasToken}}<form method="post" action="/status/logout"><button class="link">Sair do painel</button></form>{{end}}</footer>
</div></body></html>`))
