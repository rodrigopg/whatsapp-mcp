package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	settingsPath      = "store/settings.json"
	defaultStallHours = 6
	unpairWord        = "DESCONECTAR"
)

// settings is what the owner may change from the panel. Everything else stays in the environment:
// the webhook, the bind address and the public URL could redirect messages if the panel key leaked.
type settings struct {
	StallHours       *int   `json:"stall_hours,omitempty"` // nil: 6; 0: never warn
	ReadOnly         bool   `json:"read_only,omitempty"`
	TranscribeOff    bool   `json:"transcribe_off,omitempty"`
	Outgoing         *bool  `json:"transcribe_outgoing,omitempty"` // nil: TRANSCRIBE_OUTGOING from the environment
	Language         string `json:"transcription_language,omitempty"`
	TranscriptionKey string `json:"transcription_api_key,omitempty"`
	PanelKeyHash     string `json:"panel_key_sha256,omitempty"` // set: this key signs in instead of API_AUTH_TOKEN
	PanelEpoch       int    `json:"panel_epoch,omitempty"`      // bumping it signs every browser out
}

func (s settings) stallAfter() time.Duration {
	if s.StallHours == nil {
		return defaultStallHours * time.Hour
	}
	return time.Duration(*s.StallHours) * time.Hour
}

func (s settings) language() string {
	if s.Language != "" {
		return s.Language
	}
	if v := strings.ToLower(strings.TrimSpace(os.Getenv("TRANSCRIPTION_LANGUAGE"))); v != "" {
		return v
	}
	return "pt"
}

func (s settings) outgoing() bool {
	if s.Outgoing != nil {
		return *s.Outgoing
	}
	return os.Getenv("TRANSCRIBE_OUTGOING") == "1"
}

// transcriptionEnv is appended to the transcriber's environment, where the last value of a key wins.
func (s settings) transcriptionEnv() []string {
	var env []string
	if s.Language != "" {
		env = append(env, "TRANSCRIPTION_LANGUAGE="+s.Language)
	}
	if s.Outgoing != nil {
		env = append(env, "TRANSCRIBE_OUTGOING="+map[bool]string{true: "1", false: "0"}[*s.Outgoing])
	}
	if s.TranscriptionKey != "" {
		env = append(env, "TRANSCRIPTION_API_KEY="+s.TranscriptionKey)
	}
	return env
}

type settingsStore struct {
	path string
	mu   sync.Mutex
	s    settings
}

// appSettings is loaded in main before anything reads it; a nil store answers with the defaults.
var appSettings *settingsStore

// loadSettings: a missing file means defaults; a broken one is an error, because starting with
// read-only mode or a changed panel key silently dropped would be worse than not starting.
func loadSettings(path string) (*settingsStore, error) {
	st := &settingsStore{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &st.s)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return st, nil
}

func (st *settingsStore) get() settings {
	if st == nil {
		return settings{}
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.s
}

// update applies change and writes the file before the new values count.
func (st *settingsStore) update(change func(*settings)) error {
	if st == nil {
		return errors.New("no settings file")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	next := st.s
	change(&next)
	data, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, st.path); err != nil {
		return err
	}
	st.s = next
	return nil
}

// readOnlyAllowed lists the /api/ routes that neither send nor change anything on WhatsApp: reads,
// media downloads (transcription needs them) and the bridge's own notes behind the morning summary.
// A route missing here is refused in read-only mode, so a new write route is blocked by default.
var readOnlyAllowed = map[string]bool{
	"/api/chats": true, "/api/chats/active": true, "/api/chat": true, "/api/chat/by_contact": true,
	"/api/messages": true, "/api/message_context": true, "/api/sender_name": true,
	"/api/contacts/search": true, "/api/contacts/chats": true, "/api/contacts/last_interaction": true,
	"/api/search_contacts": true, "/api/resolve_contact": true, "/api/is_on_whatsapp": true,
	"/api/group_info": true, "/api/group_invite": true, "/api/poll_votes": true,
	"/api/download": true, "/api/mediaretry": true, "/api/chat/backfill": true,
	"/api/summary": true, "/api/matters/list": true, "/api/matters/record": true, "/api/matters/close": true,
	"/api/matters/reopen": true, "/api/review/next": true, "/api/review/mark": true,
	"/api/favorites/suggest": true, "/api/favorites/list": true,
	"/api/drafts/list": true, "/api/drafts/create": true, "/api/drafts/discard": true,
}

// readOnlyGuard enforces the panel's read-only switch on the bridge itself, so it holds for every
// client of the API (remote connector, local MCP server, scripts) without restarting any of them.
func readOnlyGuard(st *settingsStore, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && !readOnlyAllowed[r.URL.Path] && st.get().ReadOnly {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			json.NewEncoder(w).Encode(map[string]any{"success": false,
				"message": "Read-only mode is on in the bridge panel (Settings): nothing is sent or changed on WhatsApp."})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// connectedApp is one OAuth client of the remote connector that still holds a valid token.
type connectedApp struct {
	ID, Name, Until string
}

// The remote connector (whatsapp-mcp-server/oauth.py) keeps its clients and tokens in a SQLite file
// and looks every token up per request, so deleting a client's tokens here cuts it off at once.
func openOAuthDB(path string) (*sql.DB, error) {
	return sql.Open("sqlite3", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=rw&_busy_timeout=5000")
}

func listConnectedApps(path string, now time.Time) ([]connectedApp, error) {
	db, err := openOAuthDB(path)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT c.client_id, c.body, MAX(t.expires_at) FROM clients c
		JOIN tokens t ON t.client_id = c.client_id WHERE t.expires_at > ? GROUP BY c.client_id ORDER BY 3 DESC`, now.Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var apps []connectedApp
	for rows.Next() {
		var id, body string
		var until int64
		if err := rows.Scan(&id, &body, &until); err != nil {
			return nil, err
		}
		var meta struct {
			Name      string   `json:"client_name"`
			Redirects []string `json:"redirect_uris"`
		}
		json.Unmarshal([]byte(body), &meta)
		name := meta.Name
		if name == "" && len(meta.Redirects) > 0 {
			if u, err := url.Parse(meta.Redirects[0]); err == nil {
				name = u.Hostname()
			}
		}
		if name == "chatgpt.com" {
			name = "ChatGPT"
		}
		if name == "" {
			name = "App sem nome"
		}
		apps = append(apps, connectedApp{ID: id, Name: name, Until: time.Unix(until, 0).In(brt).Format("02/01")})
	}
	return apps, rows.Err()
}

func revokeConnectedApp(path, clientID string) error {
	db, err := openOAuthDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("DELETE FROM tokens WHERE client_id = ?", clientID)
	return err
}

var (
	languageRe = regexp.MustCompile(`^([a-z]{2}|auto)$`)
	apiKeyRe   = regexp.MustCompile(`^[!-~]{8,300}$`) // printable ASCII, no spaces: it travels in an environment variable
)

func (p *panel) registerSettings(mux *http.ServeMux) {
	mux.HandleFunc("/status/settings", p.settingsPage)
	mux.HandleFunc("/status/settings/stall", p.action(func(_ http.ResponseWriter, r *http.Request) string {
		h, err := strconv.Atoi(r.FormValue("hours"))
		if err != nil || (h != 0 && h != 3 && h != 6 && h != 12) {
			return "invalid"
		}
		return p.save(func(s *settings) { s.StallHours = &h })
	}))
	mux.HandleFunc("/status/settings/ai", p.action(func(_ http.ResponseWriter, r *http.Request) string {
		on := r.FormValue("readonly") == "1"
		return p.save(func(s *settings) { s.ReadOnly = on })
	}))
	mux.HandleFunc("/status/settings/revoke", p.action(func(_ http.ResponseWriter, r *http.Request) string {
		if p.oauthDB == "" || r.FormValue("id") == "" {
			return "invalid"
		}
		if err := revokeConnectedApp(p.oauthDB, r.FormValue("id")); err != nil {
			fmt.Printf("settings: revoke connector app: %v\n", err)
			return "savefail"
		}
		return "revoked"
	}))
	mux.HandleFunc("/status/settings/audio", p.action(func(_ http.ResponseWriter, r *http.Request) string {
		lang, key, out := r.FormValue("lang"), r.FormValue("key"), r.FormValue("outgoing") == "1"
		if !languageRe.MatchString(lang) || (key != "" && !apiKeyRe.MatchString(key)) {
			return "invalid"
		}
		return p.save(func(s *settings) {
			s.TranscribeOff, s.Outgoing, s.Language = r.FormValue("on") != "1", &out, lang
			if key != "" {
				s.TranscriptionKey = key
			}
		})
	}))
	mux.HandleFunc("/status/settings/signout", p.action(func(w http.ResponseWriter, _ *http.Request) string {
		flash := p.save(func(s *settings) { s.PanelEpoch++ })
		if flash != "saved" {
			return flash
		}
		http.SetCookie(w, &http.Cookie{Name: panelCookie, Path: "/status", MaxAge: -1})
		return ""
	}))
	mux.HandleFunc("/status/settings/newkey", p.newKey)
	mux.HandleFunc("/status/settings/unpair", p.unpair)
}

func (p *panel) save(change func(*settings)) string {
	if err := p.set.update(change); err != nil {
		fmt.Printf("settings: cannot save: %v\n", err)
		return "savefail"
	}
	return "saved"
}

// newKey replaces the panel key with a random one, shown once in this answer and stored only as a hash.
func (p *panel) newKey(w http.ResponseWriter, r *http.Request) {
	if !p.allowed(w, r) {
		return
	}
	raw := make([]byte, 16)
	rand.Read(raw)
	key := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(key))
	if p.save(func(s *settings) { s.PanelKeyHash, s.PanelEpoch = hex.EncodeToString(sum[:]), s.PanelEpoch+1 }) != "saved" {
		back(w, r, "savefail")
		return
	}
	p.setCookie(w, r) // this browser stays in; every other one is out
	v := p.settingsView(r)
	v.Authed, v.NewKey = true, key
	p.renderSettings(w, v)
}

func (p *panel) unpair(w http.ResponseWriter, r *http.Request) {
	if !p.allowed(w, r) {
		return
	}
	if strings.TrimSpace(r.FormValue("confirm")) != unpairWord {
		back(w, r, "confirm")
		return
	}
	if err := p.sup.unpair(); err != nil {
		fmt.Printf("settings: unpair: %v\n", err)
		back(w, r, "unpairfail")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, `<!DOCTYPE html><html lang="pt-BR"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="10;url=/status"><title>Desconectado</title>
<body style="font:16px system-ui,sans-serif;max-width:30rem;margin:15vh auto;padding:0 16px">
<h1>WhatsApp desconectado</h1><p>A bridge está reiniciando. Esta página volta sozinha para o painel em 10 s, onde aparece o novo código QR.</p></body></html>`)
}

type settingsView struct {
	Account          string
	Authed, HasToken bool
	Flash            string
	FlashBad         bool
	Stall            int // hours; 0: never
	ReadOnly         bool
	AppsOn           bool // the bridge knows where the connector keeps its logins
	AppsFailed       bool
	Apps             []connectedApp
	Transcribe       bool
	Outgoing         bool
	Language         string
	Languages        [][2]string
	KeyField, KeySet bool
	NewKey           string
	Word             string
}

func (p *panel) settingsView(r *http.Request) settingsView {
	v := settingsView{Account: p.info().Account, Authed: p.authed(r), HasToken: p.keyed(), Word: unpairWord}
	v.Flash, v.FlashBad = flashText(r.URL.Query().Get("m"))
	if !v.Authed { // nothing below may reach an open page
		return v
	}
	s := p.set.get()
	v.Stall, v.ReadOnly = int(s.stallAfter().Hours()), s.ReadOnly
	v.Transcribe, v.Outgoing, v.Language = !s.TranscribeOff, s.outgoing(), s.language()
	v.Languages = [][2]string{{"pt", "Português"}, {"auto", "Detectar sozinho"}, {"en", "Inglês"}, {"es", "Espanhol"}}
	if !strings.Contains("pt auto en es", v.Language) {
		v.Languages = append(v.Languages, [2]string{v.Language, v.Language})
	}
	v.KeyField = strings.EqualFold(os.Getenv("TRANSCRIPTION_ENGINE"), "api")
	v.KeySet = s.TranscriptionKey != "" || os.Getenv("TRANSCRIPTION_API_KEY") != ""
	if v.AppsOn = p.oauthDB != ""; v.AppsOn {
		apps, err := listConnectedApps(p.oauthDB, p.now())
		if err != nil {
			fmt.Printf("settings: list connector apps: %v\n", err)
		}
		v.Apps, v.AppsFailed = apps, err != nil
	}
	return v
}

func (p *panel) settingsPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p.renderSettings(w, p.settingsView(r))
}

func (p *panel) renderSettings(w http.ResponseWriter, v settingsView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	panelTmpl.ExecuteTemplate(w, "settings", v)
}

var _ = template.Must(panelTmpl.New("settings").Parse(`<!DOCTYPE html><html lang="pt-BR"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Configurações · Seu WhatsApp</title>
<style>{{template "css"}}
.head{display:flex;justify-content:space-between;align-items:baseline;gap:8px;flex-wrap:wrap}
.badge{font-size:.72rem;color:var(--muted);border:1px solid var(--line);border-radius:999px;padding:1px 8px}
label.opt{display:flex;gap:10px;align-items:flex-start;font-size:.92rem}label.opt input{flex:none;margin-top:4px}label.opt small{display:block;color:var(--muted);font-size:.8rem}
label.sel{display:grid;gap:4px;font-size:.92rem;width:100%}label.sel input{width:100%}
select{font:inherit;padding:9px 10px;border-radius:10px;border:1px solid var(--line);background:var(--bg);color:var(--fg);max-width:100%}
select:focus-visible{outline:2px solid var(--brand);outline-offset:2px}
.apps{list-style:none;margin:0;padding:0;display:grid;gap:8px}.apps li{display:flex;justify-content:space-between;align-items:center;gap:8px;flex-wrap:wrap}
.apps small{display:block;color:var(--muted);font-size:.8rem}ul.plain{margin:0;padding-left:18px;font-size:.9rem;color:var(--muted)}
.danger{border-color:var(--bad)}button.bad{background:var(--bad);color:var(--card);border-color:transparent}
code.key{font:600 1rem ui-monospace,monospace;word-break:break-all;user-select:all}
form.stack{display:grid;gap:12px;justify-items:start}form.stack .field{width:100%}
</style></head><body><div class="p">
<header><b>Seu WhatsApp</b>{{if .Account}}<span class="acct">{{.Account}}</span>{{end}}</header>
{{template "nav" "settings"}}
{{if not .Authed}}<section class="card wide"><h2>Configurações</h2>
{{if .HasToken}}<p class="sub">Para mudar as configurações, entre com a chave do painel.</p>
<form class="field" method="post" action="/status/login"><input name="key" type="password" autocomplete="current-password" placeholder="Chave do painel" aria-label="Chave do painel" required><button class="primary">Entrar</button></form>
{{else}}<p class="sub">Esta bridge não tem chave configurada, então o painel só abre no próprio computador, pelo endereço localhost.</p>{{end}}</section>
{{else}}
{{if .NewKey}}<section class="card wide result ok" role="status"><b>Nova chave do painel</b><code class="key">{{.NewKey}}</code>
<p class="sub">Guarde agora: ela não aparece de novo. Os outros navegadores já saíram; este continua dentro.</p></section>{{end}}
{{if .Flash}}<p class="flash wide{{if .FlashBad}} bad{{end}}" role="status">{{.Flash}}</p>{{end}}
<div class="col">
<section class="card"><div class="head"><h2>Avisos</h2><span class="badge">vale na hora</span></div>
<form class="stack" method="post" action="/status/settings/stall"><label class="sel">Avisar quando ficar sem mensagem nova
<select name="hours">{{$s := .Stall}}{{range .StallOptions}}<option value="{{.Hours}}"{{if eq .Hours $s}} selected{{end}}>{{.Label}}</option>{{end}}</select></label>
<p class="fine">É o aviso amarelo da página inicial.</p><button>Salvar</button></form></section>
<section class="card"><div class="head"><h2>O que a IA pode fazer</h2><span class="badge">vale na hora</span></div>
<form class="stack" method="post" action="/status/settings/ai"><label class="opt"><input type="checkbox" name="readonly" value="1"{{if .ReadOnly}} checked{{end}}>
<span>Somente leitura<small>A IA lê e resume, mas não envia mensagem, não arquiva e não reage.</small></span></label><button>Salvar</button></form>
<h2>Apps conectados</h2>
{{if not .AppsOn}}<p class="sub">Para listar e desligar o Claude e o ChatGPT por aqui, a instalação precisa informar onde o conector guarda os acessos (MCP_OAUTH_DB).</p>
{{else if .AppsFailed}}<p class="sub">Não consegui ler os acessos do conector agora.</p>
{{else if not .Apps}}<p class="sub">Nenhum app conectado pela internet agora.</p>
{{else}}<ul class="apps">{{range .Apps}}<li><span>{{.Name}}<small>acesso válido até {{.Until}}</small></span>
<form method="post" action="/status/settings/revoke"><input type="hidden" name="id" value="{{.ID}}"><button>Desligar</button></form></li>{{end}}</ul>
<p class="fine">Desligar tira o acesso na hora; para voltar, o app pede a senha do conector de novo. Apps ligados direto neste computador não aparecem aqui.</p>{{end}}</section>
<section class="card"><div class="head"><h2>Áudios em texto</h2><span class="badge">vale no próximo áudio</span></div>
<form class="stack" method="post" action="/status/settings/audio">
<label class="opt"><input type="checkbox" name="on" value="1"{{if .Transcribe}} checked{{end}}><span>Transcrever áudios recebidos</span></label>
<label class="opt"><input type="checkbox" name="outgoing" value="1"{{if .Outgoing}} checked{{end}}><span>Transcrever também os que eu envio</span></label>
<label class="sel">Idioma<select name="lang">{{$l := .Language}}{{range .Languages}}<option value="{{index . 0}}"{{if eq (index . 0) $l}} selected{{end}}>{{index . 1}}</option>{{end}}</select></label>
{{if .KeyField}}<label class="sel">Chave do serviço de transcrição
<input name="key" type="password" autocomplete="off" placeholder="{{if .KeySet}}Configurada. Digite só para trocar{{else}}Cole a chave aqui{{end}}"></label>
<p class="fine">A chave nunca é mostrada.</p>{{end}}
<p class="fine">Só tem efeito se a transcrição foi ligada na instalação.</p><button>Salvar</button></form></section>
</div>
<div class="col">
<section class="card"><div class="head"><h2>Chave do painel</h2><span class="badge">vale na hora</span></div>
<p class="sub">Trocar a chave tira o acesso de todos os navegadores que já entraram, menos este.</p>
<div class="row"><form method="post" action="/status/settings/newkey"><button>Gerar nova chave</button></form>
{{if .HasToken}}<form method="post" action="/status/settings/signout"><button>Sair de todos os navegadores</button></form>{{end}}</div></section>
<section class="card"><h2>Só pela instalação</h2><p class="sub">Estas ficam fora da web de propósito: quem roubasse a chave do painel poderia desviar suas mensagens.</p>
<ul class="plain"><li>Para onde a bridge avisa mensagens novas (webhook)</li><li>Endereço e porta em que a bridge atende</li><li>Endereço público do conector</li></ul></section>
<section class="card danger"><h2>Desconectar este WhatsApp</h2><p class="sub">Desliga a bridge do seu número. As mensagens já guardadas ficam; para voltar, escaneie o QR de novo.</p>
<details><summary>Desconectar…</summary><form class="stack" method="post" action="/status/settings/unpair" style="margin-top:12px">
<label class="sel">Digite {{.Word}} para confirmar<input name="confirm" autocomplete="off" autocapitalize="characters" required></label>
<button class="bad">Desconectar agora</button></form></details></section>
</div>{{end}}
<footer><span>Horários em BRT</span>{{if and .Authed .HasToken}}<form method="post" action="/status/logout"><button class="link">Sair do painel</button></form>{{end}}</footer>
</div></body></html>`))

type stallOption struct {
	Hours int
	Label string
}

func (settingsView) StallOptions() []stallOption {
	return []stallOption{{3, "3 horas"}, {6, "6 horas"}, {12, "12 horas"}, {0, "Nunca"}}
}
