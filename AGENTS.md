# WhatsApp MCP Server

MCP server pra WhatsApp pessoal. Bridge Go/whatsmeow (`whatsapp-bridge/`) + servidor MCP Python (`whatsapp-mcp-server/`), SQLite local, transcrição de áudio opt-in.

## Carregar contexto antes de trabalhar

| Área | Arquivo |
|------|---------|
| Bridge Go (whatsmeow, REST, history sync, mídia, segurança) | `.claude/context/bridge-go.md` |
| Servidor MCP Python (tools, busca, LID) | `.claude/context/mcp-python.md` |
| Transcrição (whisper local/API, backfill, recovery, sweep) | `.claude/context/transcription.md` |
| Deploy/operação (VPS systemd, launchd local, build, install.sh, DNS, git) | `.claude/context/deploy.md` |

Tarefa multi-área: leia os arquivos relevantes em paralelo.

## Regras inegociáveis

- Go **1.25+** (casa com go.mod); recompilar binário após mudar main.go — `go run` é só dev. CGO impede cross-compile do macOS pra Linux/VPS — compilar direto na VPS (tem Go+gcc lá).
- REST API liga **127.0.0.1** por padrão; `BIND_ADDR=<ip>` pra expor além de loopback exige `API_AUTH_TOKEN` setado (a bridge recusa subir sem token nesse caso) — nunca bind não-loopback sem token, mesmo em rede privada (Tailscale/VPN não é substituto de auth).
- `safeMediaPath` **rejeita** componentes com separadores/`..` — não sanitizar silenciosamente.
- Re-parear/re-sync: `StoreMessage` preserva `content` existente via `COALESCE(NULLIF(...))` (não sobrescreve transcrição com string vazia do sync) — mas faça backup de `messages.db`/`whatsapp.db` antes de qualquer re-pareamento de qualquer forma, é operação real de produção.
- Transcrição é **opt-in**; sem engine, sweep deve ser no-op (não marcar áudios).
- `transcription.env` **nunca** commitado (gitignored).
- Repo próprio (independente do projeto original): `origin` = `rodrigopg/whatsapp-mcp`.

## Checklist antes de abrir PR

- [ ] `go build -o whatsapp-bridge .` compila; `go test ./...` passa.
- [ ] `python3 -m unittest test_transcribe -v` passa.
- [ ] Mudou main.go? Binário recompilado e bridge reiniciada — local: `launchctl kickstart -k`; VPS: `go build` na própria VPS (CGO não cross-compila) + `systemctl restart whatsapp-bridge-*`.
- [ ] Sem path pessoal/secret vazando (transcription.env, API keys).
- [ ] Mudou comportamento de sync/escrita? Conferir impacto em transcrições existentes.
- [ ] README/install.sh coerentes se mudou onboarding (versão Go, env vars, troubleshooting).
- [ ] PR contra `rodrigopg/main`.

## Higiene de PRs paralelos

- Teste Go novo vai num `<feature>_test.go` novo, nunca no fim do `main_test.go`.
- Teste Python novo vai num `test_<feature>.py` novo (descoberto automaticamente; nunca listar módulos por nome).
- Teste e2e novo = método novo na sua seção numerada; tool MCP nova = uma linha por nome em `MCP_TOOLS` (ordem alfabética, vírgula no fim).
- Linhas da tabela de tools do README e do `whatsmeow-gap-analysis.md` são linhas únicas.
- PR sempre contra `main`; PR com base em outra branch é fechado pelo GitHub quando essa branch é apagada.
- Depois de squash merge, outros PRs podem conflitar: rebase via cherry-pick do seu commit em cima de `origin/main`, mantendo os dois lados quando ambos só adicionam.

## Comandos essenciais

```bash
# build + test
cd whatsapp-bridge && go build -o whatsapp-bridge . && go test ./...
cd whatsapp-mcp-server && python3 -m unittest test_transcribe -v

# serviço (macOS launchd)
launchctl print gui/$(id -u)/com.whatsapp-mcp.bridge          # status
launchctl kickstart -k gui/$(id -u)/com.whatsapp-mcp.bridge    # reiniciar
tail -f whatsapp-bridge/bridge.log

# transcrição manual (set -a propaga pro subprocesso; source sozinho não)
cd whatsapp-mcp-server && set -a && source ../whatsapp-bridge/transcription.env && set +a
python3 transcribe.py            # backfill
WHATSAPP_BRIDGE_LOG=../whatsapp-bridge/bridge.log python3 recover_audios.py
```
