package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	pairingQuiet     = 2 * time.Minute // history sync counts as finished after this long without a chunk
	panelCookie      = "panel"
	panelCookieTTL   = 30 * 24 * time.Hour
	loginMaxFails    = 10
	loginMaxClients  = 1024     // addresses with wrong keys remembered at once
	panelMaxBody     = 64 << 10 // a panel form is a few short fields
	loginWindow      = 15 * time.Minute
	diagnoseTimeout  = 5 * time.Second
	diagnoseInterval = 10 * time.Second
)

// Stamped from the event loop and the HTTP middleware, read by /status.
var (
	pairedAtUnix    atomic.Int64 // QR scanned in this run
	lastHistoryUnix atomic.Int64 // newest history sync chunk
	lastAPIUnix     atomic.Int64 // newest authenticated /api/ request
)

func unixTime(v *atomic.Int64) time.Time {
	if n := v.Load(); n > 0 {
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// pairingNow: the QR was scanned in this run and history arrived (or pairing happened) a moment ago.
func pairingNow(now time.Time) bool {
	paired, hist := unixTime(&pairedAtUnix), unixTime(&lastHistoryUnix)
	if paired.IsZero() {
		return false
	}
	if hist.After(paired) {
		paired = hist
	}
	return now.Sub(paired) < pairingQuiet
}

// stampAPIAccess records when /api/ was last used; it sits behind the bearer check.
func stampAPIAccess(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			lastAPIUnix.Store(time.Now().Unix())
		}
		next.ServeHTTP(w, r)
	})
}

// The marker survives the restart that follows a logout, so the next run can say why a QR is back.
func markLoggedOut()  { os.WriteFile(loggedOutMarker(), nil, 0o600) }
func clearLoggedOut() { os.Remove(loggedOutMarker()) }
func loggedOutAt() time.Time {
	if fi, err := os.Stat(loggedOutMarker()); err == nil {
		return fi.ModTime()
	}
	return time.Time{}
}

// panelState names the one banner /status shows. stall is how long without a new message turns
// "connected" amber; 0 never does.
func panelState(si statusInfo, now time.Time, stall time.Duration) string {
	switch {
	case si.Connected && si.Pairing:
		return "pairing"
	case si.Connected && stall > 0 && !si.LastMsgAt.IsZero() && now.Sub(si.LastMsgAt) >= stall:
		return "stall"
	case si.Connected:
		return "ok"
	case !si.LoggedOutAt.IsZero():
		return "loggedout"
	case si.HasQR:
		return "qr"
	case si.Stopped:
		return "stopped"
	case si.Held > 0:
		return "held"
	case si.Paired:
		return "offline"
	}
	return "waiting"
}

type diagResult struct {
	OK     bool
	Detail string // plain-words cause when not OK
	Took   string
	At     string
}

// diagnoseConnector fetches the connector's OAuth metadata through its public address. A pass
// proves DNS, the certificate, the tunnel or proxy and the MCP server with OAuth on, as seen from
// this host; it does not prove the AI vendor's network reaches it, nor the connector passphrase.
func diagnoseConnector(ctx context.Context, client *http.Client, publicURL string) diagResult {
	start := time.Now()
	res := diagResult{At: start.In(brt).Format("15:04 BRT")}
	if publicURL == "" {
		res.Detail = "O endereço público do conector não está configurado (MCP_PUBLIC_URL)."
		return res
	}
	ctx, cancel := context.WithTimeout(ctx, diagnoseTimeout)
	defer cancel()
	// #nosec G704 -- the URL is the operator-set MCP_PUBLIC_URL, never request input
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, publicURL+"/.well-known/oauth-protected-resource/mcp", nil)
	if err != nil {
		res.Detail = "O endereço público do conector é inválido."
		return res
	}
	// #nosec G704 -- same operator-set URL
	resp, err := client.Do(req)
	res.Took = fmt.Sprintf("%.1f s", time.Since(start).Seconds())
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &dnsErr):
		res.Detail = "O endereço não existe na internet (DNS). Confira o domínio."
	case errors.As(err, &certErr):
		res.Detail = "O certificado de segurança (HTTPS) do endereço é inválido ou venceu."
	case err != nil:
		res.Detail = "O endereço público não respondeu. Causa provável: o túnel HTTPS ou o servidor do conector está parado."
	default:
		defer resp.Body.Close()
		var meta struct {
			Resource string `json:"resource"`
		}
		switch {
		case resp.StatusCode == http.StatusNotFound:
			res.Detail = "O endereço responde, mas o conector não está com o login (OAuth) ligado."
		case resp.StatusCode != http.StatusOK:
			res.Detail = fmt.Sprintf("O endereço respondeu com erro %d. O servidor do conector pode estar parado atrás do túnel.", resp.StatusCode)
		case json.NewDecoder(resp.Body).Decode(&meta) != nil || !strings.HasPrefix(meta.Resource, publicURL):
			res.Detail = "Quem respondeu nesse endereço não é o conector deste WhatsApp."
		default:
			res.OK = true
		}
	}
	return res
}

type reconnecter interface {
	forceReconnect() (wait time.Duration, err error)
	unpair() error
}

// panel is /status and its actions. The page is open and read-only; the connector link and every
// action need the panel key (API_AUTH_TOKEN, or the one generated in Settings), or a loopback Host
// when neither exists.
type panel struct {
	token     string
	publicURL string         // MCP_PUBLIC_URL without a trailing slash; "" when the remote connector is not set up
	oauthDB   string         // MCP_OAUTH_DB: the remote connector's logins; "" hides the connected-apps list
	set       *settingsStore // nil in tests that do not touch settings: defaults, nothing saved
	info      func() statusInfo
	sup       reconnecter
	client    *http.Client
	db        *sql.DB // messages.db, for /status/insights
	now       func() time.Time

	mu     sync.Mutex
	fails  map[string]loginFails // wrong keys per client (clientKey), within loginWindow
	diag   *diagResult
	diagAt time.Time
}

// loginFails counts one client's wrong keys since from.
type loginFails struct {
	n    int
	from time.Time
}

// clientKey names who is typing keys: the peer address, or its /64 for IPv6 (one network holds all
// of those). When the peer is this machine itself (a reverse proxy or tunnel in front of the
// bridge), the last X-Forwarded-For entry is used: that is the one the proxy added, whatever the
// visitor sent before it. The header is ignored from any other peer, which could write anything
// there to dodge the count. A proxy on another host, or one that does not forward the address,
// leaves every visitor sharing one count.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	if addr.Unmap().IsLoopback() {
		hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
		if fwd, err := netip.ParseAddr(strings.TrimSpace(hops[len(hops)-1])); err == nil {
			addr = fwd
		}
	}
	if addr = addr.Unmap().WithZone(""); addr.Is6() {
		if p, err := addr.Prefix(64); err == nil {
			return p.String()
		}
	}
	return addr.String()
}

func newPanel(token, publicURL string, info func() statusInfo, sup reconnecter) *panel {
	return &panel{token: token, publicURL: strings.TrimRight(publicURL, "/"), info: info, sup: sup,
		client: &http.Client{}, now: time.Now}
}

func (p *panel) register(mux *http.ServeMux) {
	mux.HandleFunc("/status", p.page)
	mux.HandleFunc("/status/insights", p.insightsPage)
	p.registerSettings(mux)
	mux.HandleFunc("/status/login", p.login)
	mux.HandleFunc("/status/logout", p.action(func(w http.ResponseWriter, r *http.Request) string {
		// #nosec G124 -- clears the cookie
		http.SetCookie(w, &http.Cookie{Name: panelCookie, Path: "/status", MaxAge: -1})
		return ""
	}))
	mux.HandleFunc("/status/reconnect", p.action(func(http.ResponseWriter, *http.Request) string { return p.reconnect() }))
	mux.HandleFunc("/status/diagnose", p.action(func(_ http.ResponseWriter, r *http.Request) string {
		p.mu.Lock()
		tooSoon := p.now().Sub(p.diagAt) < diagnoseInterval
		if !tooSoon {
			p.diagAt = p.now()
		}
		p.mu.Unlock()
		if !tooSoon {
			res := diagnoseConnector(r.Context(), p.client, p.publicURL)
			p.mu.Lock()
			p.diag = &res
			p.mu.Unlock()
		}
		return ""
	}))
}

// keyed: a panel key exists, so the panel asks for it.
func (p *panel) keyed() bool { return p.token != "" || p.set.get().PanelKeyHash != "" }

// keyOK checks a typed key: the one generated in Settings when there is one, else API_AUTH_TOKEN.
func (p *panel) keyOK(given string) bool {
	if h := p.set.get().PanelKeyHash; h != "" {
		sum := sha256.Sum256([]byte(given))
		return subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(h)) == 1
	}
	return subtle.ConstantTimeCompare([]byte(given), []byte(p.token)) == 1
}

// cookieValue changes with the key and with the epoch, so a new key or "sign out everywhere"
// invalidates every cookie handed out before.
func (p *panel) cookieValue() string {
	s := p.set.get()
	key, msg := p.token, "panel"
	if s.PanelKeyHash != "" {
		key = s.PanelKeyHash
	}
	if s.PanelEpoch > 0 {
		msg += strconv.Itoa(s.PanelEpoch)
	}
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(msg))
	return hex.EncodeToString(mac.Sum(nil))
}

func (p *panel) setCookie(w http.ResponseWriter, r *http.Request) {
	// #nosec G124 -- Secure is set whenever the request came over HTTPS; the panel also serves plain loopback
	http.SetCookie(w, &http.Cookie{Name: panelCookie, Value: p.cookieValue(), Path: "/status",
		MaxAge: int(panelCookieTTL.Seconds()), HttpOnly: true, SameSite: http.SameSiteStrictMode,
		Secure: r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"})
}

func loopbackHost(hostport string) bool {
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || host == "[::1]"
}

// authed: the panel cookie, or (no key at all) a loopback Host, which a DNS-rebinding page cannot fake.
func (p *panel) authed(r *http.Request) bool {
	if !p.keyed() {
		return loopbackHost(r.Host)
	}
	c, err := r.Cookie(panelCookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(p.cookieValue())) == 1
}

// sameOrigin is the CSRF check: a browser always sends Origin (or Referer) on a form POST.
func sameOrigin(r *http.Request) bool {
	from := r.Header.Get("Origin")
	if from == "" {
		from = r.Header.Get("Referer")
	}
	u, err := url.Parse(from)
	return err == nil && u.Host != "" && u.Host == r.Host
}

// allowed is the gate of every state-changing route: POST, same origin, signed in.
func (p *panel) allowed(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelMaxBody)
	if !sameOrigin(r) || !p.authed(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}

// action wraps a state-changing route: allowed, then back to the page it came from.
func (p *panel) action(do func(http.ResponseWriter, *http.Request) (flash string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if p.allowed(w, r) {
			back(w, r, do(w, r))
		}
	}
}

func back(w http.ResponseWriter, r *http.Request, flash string) {
	target := "/status"
	if strings.HasPrefix(r.URL.Path, "/status/settings/") {
		target = "/status/settings"
	}
	if flash != "" {
		target += "?m=" + flash
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

func (p *panel) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !sameOrigin(r) || !p.keyed() {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, panelMaxBody)
	key, who := r.FormValue("key"), clientKey(r)
	p.mu.Lock()
	now := p.now()
	for k, f := range p.fails {
		if now.Sub(f.from) > loginWindow {
			delete(p.fails, k)
		}
	}
	if _, known := p.fails[who]; !known && len(p.fails) >= loginMaxClients {
		who = "" // ponytail: past the cap new addresses share one count; per-network buckets if this ever matters
	}
	f := p.fails[who]
	locked := f.n >= loginMaxFails
	ok := !locked && p.keyOK(key)
	if !locked && !ok {
		if f.n == 0 {
			f.from = now
		}
		f.n++
		if p.fails == nil {
			p.fails = make(map[string]loginFails)
		}
		p.fails[who] = f
	}
	p.mu.Unlock()
	switch {
	case locked:
		http.Error(w, "Muitas tentativas erradas. Tente de novo em 15 minutos.", http.StatusTooManyRequests)
	case !ok:
		back(w, r, "key")
	default:
		p.setCookie(w, r)
		back(w, r, "")
	}
}

func (p *panel) reconnect() string {
	wait, err := p.sup.forceReconnect()
	switch {
	case err == nil:
		return "rc"
	case errors.Is(err, errReconnectCooldown):
		return fmt.Sprintf("wait%d", int(wait.Seconds())+1)
	case errors.Is(err, errReconnectHeld), errors.Is(err, errReconnectStopped):
		return "held"
	}
	return "fail"
}

type panelView struct {
	statusInfo
	State        string
	StallHours   int
	HoldMinutes  int
	LoggedOut    string
	Authed       bool
	HasToken     bool
	ConnectorURL string
	Flash        string
	FlashBad     bool
	Diag         *diagResult
	LastAPI      string
}

func (v panelView) Refresh() bool {
	return v.State == "qr" || v.State == "pairing" || v.State == "loggedout" || v.State == "waiting"
}
func (v panelView) Online() bool { return v.State == "ok" || v.State == "stall" }
func (v panelView) CanReconnect() bool {
	return v.State == "ok" || v.State == "stall" || v.State == "offline"
}

func flashText(code string) (text string, bad bool) {
	switch {
	case code == "rc":
		return "Reconectado.", false
	case code == "saved":
		return "Salvo.", false
	case code == "revoked":
		return "App desligado. Para voltar, ele vai pedir a senha do conector.", false
	case code == "savefail":
		return "Não deu para salvar agora. Nada mudou.", true
	case code == "invalid":
		return "Valor inválido. Nada mudou.", true
	case code == "confirm":
		return "Digite a palavra exatamente para confirmar.", true
	case code == "unpairfail":
		return "Não deu para desconectar: a bridge precisa estar conectada ao WhatsApp. Reconecte e tente de novo.", true
	case code == "key":
		return "Chave errada. Confira e tente de novo.", true
	case code == "held":
		return "O WhatsApp pediu para esperar; reconectar agora não é possível.", true
	case code == "fail":
		return "Não deu para reconectar agora. A bridge continua tentando sozinha.", true
	case strings.HasPrefix(code, "wait"):
		var s int
		if _, err := fmt.Sscanf(code, "wait%d", &s); err == nil && s > 0 && s <= 120 {
			return fmt.Sprintf("Reconectou há pouco. Tente de novo em %d s.", s), true
		}
	}
	return "", false
}

func (p *panel) page(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	si := p.info()
	now := p.now()
	v := panelView{statusInfo: si, State: panelState(si, now, p.set.get().stallAfter()), HasToken: p.keyed(), Authed: p.authed(r),
		HoldMinutes: int(si.Held.Minutes()) + 1}
	v.Flash, v.FlashBad = flashText(r.URL.Query().Get("m"))
	if !si.LastMsgAt.IsZero() {
		v.StallHours = int(now.Sub(si.LastMsgAt).Hours())
	}
	if !si.LoggedOutAt.IsZero() {
		v.LoggedOut = si.LoggedOutAt.In(brt).Format("02/01 às 15:04")
	}
	if v.Authed { // nothing below may reach an open page
		if p.publicURL != "" {
			v.ConnectorURL = p.publicURL + "/mcp"
		}
		if t := unixTime(&lastAPIUnix); !t.IsZero() {
			v.LastAPI = t.In(brt).Format("02/01 15:04 BRT")
		}
		p.mu.Lock()
		v.Diag = p.diag
		p.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	panelTmpl.Execute(w, v)
}

var panelTmpl = template.Must(template.New("panel").Parse(`<!DOCTYPE html><html lang="pt-BR"><head>
<meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
{{if .Refresh}}<meta http-equiv="refresh" content="10">{{end}}<title>Seu WhatsApp</title>
<style>{{template "css"}}</style></head><body><div class="p">
<header><b>Seu WhatsApp</b>{{if .Account}}<span class="acct">{{.Account}}</span>{{end}}</header>
{{template "nav" "home"}}
<div class="col">
{{if .Flash}}<p class="flash{{if .FlashBad}} bad{{end}}" role="status">{{.Flash}}</p>{{end}}
{{if eq .State "ok"}}<section class="card banner t-ok"><span class="pill">Conectado</span><p class="big">Tudo certo</p><p class="sub">Seu WhatsApp está ligado e guardando mensagens.</p>
{{else if eq .State "stall"}}<section class="card banner t-warn"><span class="pill">Atenção</span><p class="big">Conectado, mas nenhuma mensagem nova há {{.StallHours}} h</p><p class="sub">Pode ser normal se ninguém te escreveu. Se você sabe que chegou mensagem, reconecte.</p>
{{else if eq .State "pairing"}}<section class="card banner t-info"><span class="pill">Quase lá</span><p class="big">Conectado! Baixando seu histórico</p><p class="sub">Pode levar alguns minutos. {{.Messages}} mensagens guardadas até agora.</p>
{{else if eq .State "qr"}}<section class="card banner t-info"><span class="pill">Aguardando</span><p class="big">Escaneie para conectar</p><p class="sub">Abra o WhatsApp no celular e aponte a câmera para o código.</p>
{{else if eq .State "loggedout"}}<section class="card banner t-bad"><span class="pill">Desconectado</span><p class="big">O WhatsApp desligou este aparelho em {{.LoggedOut}}</p><p class="sub">Acontece quando você remove o aparelho no celular ou fica muito tempo sem abrir o WhatsApp. Escaneie de novo para voltar.</p>
{{else if eq .State "held"}}<section class="card banner t-bad"><span class="pill">Pausado</span><p class="big">O WhatsApp pausou este aparelho por segurança</p><p class="sub">Insistir pode piorar. A bridge tenta sozinha em {{.HoldMinutes}} min. Suas mensagens antigas continuam disponíveis.</p>
{{else if eq .State "stopped"}}<section class="card banner t-bad"><span class="pill">Parado</span><p class="big">O WhatsApp recusou esta versão da bridge</p><p class="sub">É preciso atualizar a bridge. Suas mensagens antigas continuam disponíveis.</p>
{{else if eq .State "offline"}}<section class="card banner t-bad"><span class="pill">Sem conexão</span><p class="big">Sem conexão com o WhatsApp</p><p class="sub">A bridge está tentando de novo sozinha.</p>
{{else}}<section class="card banner t-info"><span class="pill">Aguardando</span><p class="big">Preparando o código QR</p><p class="sub">Esta página se atualiza sozinha.</p>{{end}}
{{if .HasQR}}<div class="qr"><img src="/qr.png" width="220" height="220" alt="Código QR para conectar o WhatsApp">
<ol><li>No celular, abra o WhatsApp</li><li>Toque em <b>Aparelhos conectados</b></li><li>Toque em <b>Conectar um aparelho</b> e aponte para o código</li></ol></div>{{end}}
{{if .Refresh}}<p class="fine">Esta página se atualiza sozinha a cada 10 s.</p>{{end}}
{{if .CanReconnect}}{{if .Authed}}<form class="row" method="post" action="/status/reconnect"><button{{if eq .State "stall"}} class="hl"{{end}}>Reconectar</button><span class="hint">Não apaga nada e não pede QR de novo.</span></form>
{{else if .HasToken}}<p class="hint">Precisa reconectar? Entre com a chave do painel.</p>{{end}}{{end}}
</section>
<section class="card"><h2>Resumo</h2><dl>
<dt>Conta</dt><dd>{{if .Account}}{{.Account}}{{else}}não conectada{{end}}</dd>
<dt>Ligada há</dt><dd>{{.Uptime}}</dd>
<dt>Mensagens guardadas</dt><dd>{{.Messages}}</dd>
<dt>Última mensagem</dt><dd>{{if .LastMsg}}{{.LastMsg}}{{else}}nenhuma{{end}}</dd></dl></section>
</div>
<div class="col">
{{if not .Online}}<section class="card"><h2>Ligar ao Claude ou ChatGPT</h2><p class="sub">Primeiro conecte o WhatsApp. Depois aparece aqui o link para colar no Claude ou no ChatGPT.</p></section>
{{else if not .Authed}}<section class="card"><h2>Ligar ao Claude ou ChatGPT</h2>
{{if .HasToken}}<p class="sub">Para ver o link do conector, reconectar e testar, entre com a chave do painel. Ela aparece no fim da instalação.</p>
<form class="field" method="post" action="/status/login"><input id="key" name="key" type="password" autocomplete="current-password" placeholder="Chave do painel" aria-label="Chave do painel" required><button class="primary">Entrar</button></form>
{{else}}<p class="sub">Esta bridge não tem chave configurada, então o painel só abre no próprio computador, pelo endereço localhost.</p>{{end}}</section>
{{else}}<section class="card"><h2>Ligar ao Claude ou ChatGPT</h2>
{{if .ConnectorURL}}<p class="sub">O mesmo link serve para os dois.</p>
<div class="field"><input id="mcpurl" readonly value="{{.ConnectorURL}}" aria-label="Link do conector" onfocus="this.select()"><button class="primary" type="button" id="copy">Copiar</button></div>
<details open><summary>Onde colar no Claude</summary><ol><li>Abra <b>Personalizar → Conectores</b></li><li>Toque em <b>Adicionar conector personalizado</b></li><li>Cole o link e confirme</li><li>Na janela que abrir, digite a <b>senha do conector</b></li></ol></details>
<details><summary>Onde colar no ChatGPT</summary><ol><li>Abra <b>Configurações → Apps e conectores → Configurações avançadas</b> e ligue o <b>modo desenvolvedor</b> (depende do seu plano)</li><li>Toque em <b>Criar</b></li><li>Cole o link e escolha <b>OAuth</b></li><li>Na janela que abrir, digite a <b>senha do conector</b></li></ol></details>
<p class="fine">Os nomes dos menus podem mudar. A senha do conector é a que você escolheu na instalação; este painel não mostra nem guarda essa senha.</p>
{{else}}<p class="sub">O conector remoto não está configurado nesta instalação. Veja no README como ligar o acesso pelo Claude e ChatGPT na web (MCP_PUBLIC_URL).</p>{{end}}</section>
<section class="card"><h2>O Claude me alcança?</h2><p class="sub">Confere se o endereço do conector responde pelo caminho público, o mesmo que o Claude e o ChatGPT usam.</p>
<form class="row" method="post" action="/status/diagnose"><button>{{if .Diag}}Testar de novo{{else}}Testar agora{{end}}</button></form>
{{with .Diag}}{{if .OK}}<div class="result ok"><b>Sim, o caminho até você está aberto</b><p>Endereço público respondeu em {{.Took}}: DNS, certificado, túnel e conector no ar.</p>
{{else}}<div class="result bad"><b>Não, o Claude não consegue chegar até você</b><p>{{.Detail}}</p><p class="fine">O WhatsApp continua guardando suas mensagens; nada se perde enquanto isso.</p>{{end}}
<p class="fine">Testado a partir deste servidor às {{.At}}. Não confere a senha do conector.</p></div>{{end}}
<dl><dt>Último acesso à API</dt><dd>{{if .LastAPI}}{{.LastAPI}}{{else}}nenhum desde que a bridge ligou{{end}}</dd></dl>
<p class="fine">Inclui a transcrição de áudios, que usa a mesma chave.</p></section>{{end}}
</div>
<footer><span>Horários em BRT</span>{{if and .Authed .HasToken}}<form method="post" action="/status/logout"><button class="link">Sair do painel</button></form>{{end}}</footer>
</div>
<script>var c=document.getElementById("copy");if(c)c.onclick=function(){var i=document.getElementById("mcpurl"),ok=function(){c.textContent="Copiado"};
(navigator.clipboard?navigator.clipboard.writeText(i.value):Promise.reject()).then(ok,function(){i.focus();i.select();try{document.execCommand("copy");ok()}catch(e){}})};</script>
</body></html>
{{define "nav"}}<nav aria-label="Páginas"><a href="/status"{{if eq . "home"}} aria-current="page"{{end}}>Início</a><a href="/status/insights"{{if eq . "insights"}} aria-current="page"{{end}}>Insights</a><a href="/status/settings"{{if eq . "settings"}} aria-current="page"{{end}}>Configurações</a></nav>{{end}}
{{define "css"}}:root{--bg:#f6f7f6;--card:#fff;--fg:#1a2421;--muted:#5f6d69;--line:#e0e6e3;--brand:#0d6b5f;--brand-ink:#fff;
--ok:#17803d;--ok-bg:#e6f4ea;--warn:#9a5b00;--warn-bg:#fdf2dc;--bad:#b42318;--bad-bg:#fdecea;--info:#245ea8;--info-bg:#e7effa}
@media (prefers-color-scheme:dark){:root{color-scheme:dark;--bg:#101614;--card:#19211f;--fg:#e3eae7;--muted:#97a6a1;--line:#2b3633;--brand:#55c4b1;--brand-ink:#05201b;
--ok:#5fd28a;--ok-bg:#12291b;--warn:#f2b24c;--warn-bg:#2d220f;--bad:#ff8a7e;--bad-bg:#331614;--info:#86b4f0;--info-bg:#132238}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
.p{max-width:1040px;margin:0 auto;padding:18px 16px 32px;display:grid;gap:14px}
@media (min-width:860px){.p{grid-template-columns:minmax(0,1fr) minmax(0,1fr);align-items:start;gap:20px;padding:32px 40px 48px}.p>header,.p>footer{grid-column:1/-1}}
header,footer{display:flex;justify-content:space-between;align-items:center;gap:8px;flex-wrap:wrap}
header b{font-size:1.1rem}footer{font-size:.8rem;color:var(--muted)}
.acct{font:500 .78rem ui-monospace,monospace;color:var(--muted);border:1px solid var(--line);padding:3px 8px;border-radius:999px}
.col{display:grid;gap:14px;min-width:0}
.card{background:var(--card);border:1px solid var(--line);border-radius:14px;padding:16px;display:grid;gap:12px;min-width:0}
h2{margin:0;font-size:1.05rem}p{margin:0}
.banner{border-left:6px solid var(--tone);background:linear-gradient(90deg,var(--tone-bg),var(--card) 70%)}
.pill{justify-self:start;font-weight:600;font-size:.72rem;letter-spacing:.06em;text-transform:uppercase;color:var(--tone);background:var(--tone-bg);border:1px solid var(--tone);padding:2px 9px;border-radius:999px}
.t-ok{--tone:var(--ok);--tone-bg:var(--ok-bg)}.t-warn{--tone:var(--warn);--tone-bg:var(--warn-bg)}.t-bad{--tone:var(--bad);--tone-bg:var(--bad-bg)}.t-info{--tone:var(--info);--tone-bg:var(--info-bg)}
.big{font-size:1.3rem;font-weight:600;line-height:1.25}.sub,.hint,.fine{color:var(--muted)}.sub{font-size:.92rem}.hint{font-size:.84rem}.fine{font-size:.78rem}
dl{display:grid;grid-template-columns:auto 1fr;gap:6px 16px;margin:0;font-size:.9rem}dt{color:var(--muted)}dd{margin:0;text-align:right;font-variant-numeric:tabular-nums}
button{font:600 .92rem inherit;font-family:inherit;border-radius:10px;padding:10px 14px;border:1px solid var(--line);background:var(--card);color:var(--fg);cursor:pointer}
button.primary{background:var(--brand);color:var(--brand-ink);border-color:transparent}button.hl{background:var(--warn);color:var(--card);border-color:transparent}
button.link{border:0;background:none;padding:0;color:var(--brand);text-decoration:underline;font-size:.82rem}
button:focus-visible,input:focus-visible,summary:focus-visible{outline:2px solid var(--brand);outline-offset:2px}
.row{display:flex;flex-wrap:wrap;gap:8px;align-items:center}.field{display:flex;gap:8px;min-width:0}
input{flex:1;min-width:0;font:.85rem ui-monospace,monospace;padding:10px 12px;border-radius:10px;border:1px solid var(--line);background:var(--bg);color:var(--fg)}
.qr{display:grid;justify-items:center;gap:10px}.qr img{width:220px;height:220px;max-width:100%;border:10px solid #fff;border-radius:8px;box-shadow:0 2px 14px rgba(0,0,0,.15)}
ol{margin:0;padding-left:1.2rem;font-size:.9rem}summary{cursor:pointer;font-weight:600;font-size:.9rem}details{border-top:1px solid var(--line);padding-top:8px}
.flash{border-radius:10px;padding:10px 12px;font-weight:600;font-size:.9rem;background:var(--ok-bg);color:var(--ok)}.flash.bad{background:var(--bad-bg);color:var(--bad)}
.result{border-radius:10px;padding:12px;display:grid;gap:8px;font-size:.9rem}.result.ok{background:var(--ok-bg)}.result.bad{background:var(--bad-bg)}
.result b{font-size:.98rem}.result.ok b{color:var(--ok)}.result.bad b{color:var(--bad)}
nav{display:flex;gap:4px;border-bottom:1px solid var(--line)}
nav a{font-weight:600;font-size:.9rem;color:var(--muted);text-decoration:none;padding:8px 12px;border-bottom:3px solid transparent}
nav a[aria-current]{color:var(--fg);border-bottom-color:var(--brand)}a:focus-visible{outline:2px solid var(--brand);outline-offset:2px}
@media (min-width:860px){.p>nav,.p>.wide{grid-column:1/-1}}
{{end}}`))
