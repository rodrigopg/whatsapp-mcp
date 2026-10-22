# Contexto: Deploy / Operação

## Bridge remota (bridge e MCP em máquinas diferentes)

Desde 2026-07-30 a bridge suporta rodar numa máquina diferente do MCP server (ex: bridge numa
VPS/home server, MCP local no laptop) — o server ficou 100% stateless (ver `mcp-python.md`),
então isso passou a ser suportado sem inventar nada além do que já existia:

- **Bind**: por padrão `127.0.0.1` (só a própria máquina). Pra expor pra outra rede, `BIND_ADDR=<ip>`
  (endereço de uma VPN/rede privada — Tailscale, WireGuard, etc — nunca `0.0.0.0` direto pra
  internet pública).
- **Auth obrigatória** quando não-loopback: `API_AUTH_TOKEN` — a bridge **recusa subir** se
  `BIND_ADDR` não é loopback e o token está vazio (fail-closed). Toda `/api/*` exige
  `Authorization: Bearer <token>`; `/qr`/`/qr.png` continuam sem auth (é o próprio fluxo de
  pareamento). MCP server lê o token equivalente via `WHATSAPP_API_AUTH_TOKEN`.
- **Múltiplas contas na mesma VPS**: rodar duas (ou mais) instâncias da bridge é só questão de
  diretório + porta (`WHATSAPP_BRIDGE_PORT`) + token diferentes por instância — cada uma seu
  próprio `store/`. Compartilhado entre instâncias do mesmo usuário: o guard de bridge ativa
  do `install.sh` (`pgrep`), os nomes de serviço que ele gera e o lock de transcrição
  (`wa_transcribe.lock` no TMPDIR) — com várias instâncias, registrar os serviços à mão.
- **Rede privada é sua escolha** (Tailscale, WireGuard, SSH tunnel, VPN da nuvem que você usa) —
  a bridge só precisa que `BIND_ADDR` aponte pra uma interface alcançável pelo MCP client.
  Cuidado com ordem de boot: se a bridge sobe **antes** da VPN/interface estar pronta, o bind
  falha silenciosamente (`bind: cannot assign requested address`, só logado, não fatal) e o
  processo fica "active" no systemd/launchd **sem porta nenhuma aberta** — checar `ss -tln`
  além do status do serviço. Se usar systemd, adicionar `After=`/`Wants=` pro serviço da sua VPN.

## Host Linux (VM de dev) vs macOS

- Doc da seção launchd descreve o setup **macOS**. VMs Linux não têm launchd — a bridge roda
  como **systemd system-service** lá (ex: `/etc/systemd/system/whatsapp-bridge-dev1.service`,
  `Restart=always`, `RestartSec=5`, `WHATSAPP_BRIDGE_PORT` explícito, ExecStart = binário direto).
- **Reiniciar sem sudo** (se não tiver root no host): `systemctl restart` exige root; workaround é
  `kill <pid>` do processo `whatsapp-bridge` — systemd com `Restart=always` religa sozinho em
  ~5s. Confirmar com `pgrep -f '/whatsapp-bridge$'`. `systemctl --user` falha nesse caso (unit é
  system-scope, não user).
- Engine de transcrição `local` (whisper.cpp) só funciona se o binário/modelo existirem *nesse*
  host — paths absolutos de outra máquina (ex: macOS) não existem lá. Nesse caso usar engine
  `api` (Groq/OpenAI) via `transcription.env`/`transcription.systemd.env` (EnvironmentFile do
  systemd, gitignored, contém a API key).
- Porta não é sempre 8080 — `transcribe.py`/scripts de backfill usam esse default, então rodar
  manual num host com porta diferente precisa `WHATSAPP_API_BASE_URL=http://localhost:<porta>/api`
  explícito.

## launchd (serviço persistente — macOS)

- Agent: `~/Library/LaunchAgents/com.whatsapp-mcp.bridge.plist`. **`RunAtLoad=true` + `KeepAlive=true`** (arranca no login, reinicia se cair, ThrottleInterval 10s).
- Roda do clone real `/Users/rodrigo/git/whatsapp-mcp/whatsapp-bridge` (NÃO de `~/.whatsapp-mcp`). Executa `start-bridge.sh`.
- **launchd NÃO herda `export` do shell** → env de transcrição vem de `transcription.env` (sourced por start-bridge.sh). O plist gerado manda stdout/stderr pra `<INSTALL_DIR>/bridge.log`. `WHATSAPP_BRIDGE_LOG` a bridge não lê: é do `recover_audios.py`, que lê esse arquivo.
- Controle:
  - Parar: `launchctl bootout gui/$(id -u)/com.whatsapp-mcp.bridge`
  - Iniciar: `launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/com.whatsapp-mcp.bridge.plist`
  - Status: `launchctl print gui/$(id -u)/com.whatsapp-mcp.bridge`

## start-bridge.sh

- `cd` próprio dir, source `transcription.env` opcional, `WHATSAPP_BRIDGE_PORT=<porta do install.sh, default 8080> exec ./whatsapp-bridge`.
- `transcription.env` é **gitignored** (path pessoal + prompt).

## Build

- `cd whatsapp-bridge && go build -o whatsapp-bridge .` — recompilar após mudar main.go (binário stale não tem fixes).
- `go run main.go` = dev (binário temp em /var/folders, morre ao fechar terminal). Loga no terminal, NÃO em arquivo → recover_audios não funciona com go run.
- Matar órfãos de porta: `lsof -ti TCP:${WHATSAPP_BRIDGE_PORT:-8080} | xargs kill`. `go run` spawna child `main` que sobrevive ao kill do pai.

## install.sh (para outros)

- Gate Go 1.25+ (casa com go.mod). Clona em `~/.whatsapp-mcp`, compila, escreve config Claude/Cursor, cria start-bridge.sh + plist (RunAtLoad/KeepAlive=false por padrão lá).
- One-line: `curl -fsSL https://raw.githubusercontent.com/rodrigopg/whatsapp-mcp/main/install.sh | bash`.
- Flags: `--service` cria unit systemd **user** `whatsapp-bridge` (Linux) / launchd KeepAlive (macOS); `--codex` registra o MCP no `~/.codex/config.toml`.

## Plugin Claude Code

- Repo também é distribuído como plugin: `.claude-plugin/plugin.json` + `commands/setup.md`, via marketplace `rodrigopg/claude-plugins` (`/plugin install whatsapp-mcp@rodrigopg`).
- Coexistência com bridge já rodando como serviço system-scope (ex: setup de VM Linux acima): `install.sh --service` cria unit systemd **user**; se a bridge já roda como serviço **system** numa porta própria, o guard do install.sh detecta a bridge ativa e pula o setup de serviço (não duplica).

## QR / auth

- Local (install.sh default): `http://localhost:8080/qr` no browser (PNG raw em `/qr.png`). macOS salva `/tmp/whatsapp-qr.png` e abre no Preview. QR renova a cada ~20s.
- Bridge remota: `/qr`/`/qr.png` ficam **sempre sem auth** mesmo com `API_AUTH_TOKEN` setado (é o próprio fluxo de pareamento) — acessar via túnel SSH (`ssh -L 8080:<bind-addr>:<porta> <host>`) ou direto pela rede privada se sua máquina já estiver nela.
- **Re-parear:** QR só é emitido no startup quando `client.Store.ID == nil` (sem sessão em `whatsapp.db`). Logout remoto apaga a sessão mas NÃO re-entra no loop de QR em runtime — `/qr` fica preso em "connected". Precisa reiniciar o processo (`kill <pid>`; systemd/launchd com restart automático religa sozinho). Logout pelo celular já zera `whatsmeow_device`; aí o bridge sobe limpo e gera QR.
- History sync ao parear é limitado (poucas msgs/conversas recentes), não histórico completo — normal do multi-device. Backup `messages.db` antes (INSERT OR REPLACE pode sobrescrever).

## DNS GitHub (rede do Rodrigo)

- `api.github.com` resolve globalmente pra `4.228.31.149` (Azure) — **inalcançável desta rede** (timeout). IPs legados `140.82.x` (range oficial GitHub) roteiam OK.
- Fix: pin em `/etc/hosts` (bloco demarcado `claude-code: GitHub pin`): api→140.82.112.6, github→140.82.112.3, codeload→140.82.112.9. Remover bloco reverte. Bloqueio é da rede/ISP, não DNS local.

## Git

- Remote: `origin` = `rodrigopg/whatsapp-mcp`. main rastreia `origin/main`.
