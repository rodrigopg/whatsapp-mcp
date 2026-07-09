# Actions: Gaps whatsmeow #7–#9 — participantes de grupo, typing e validação de número

> Identificador: `003-whatsmeow-gaps-7-8-9`
> Data: `2026-07-09`
> Roadmap: `_reversa_forward/003-whatsmeow-gaps-7-8-9/roadmap.md`

## Resumo

| Métrica | Valor |
|---------|-------|
| Total de ações | 10 |
| Paralelizáveis (`[//]`) | 3 |
| Maior cadeia de dependência | 6 (T001→T002→T003→T004→T007→T009) |

Nota de sequenciamento: T001–T003 são logicamente independentes mas compartilham `main.go` — encadeadas para evitar conflito de edição. T005 (Python) roda em paralelo com todo o trilho Go, guiada pelos contratos em `interfaces/`.

## Fase 1, Preparação

n/a — sem scaffolding, sem migração (data-delta.md: nenhuma mudança de dados).

## Fase 2, Testes

| ID | Descrição | Dependências | Paralelismo | Arquivo alvo | Confidência | Status |
|----|-----------|--------------|-------------|--------------|-------------|--------|
| T004 | `[//]` Testes dos 3 handlers no molde `TestHandleReact` (main_test.go:152): JSON inválido→400, método≠POST→405, JID/`action`/`state` inválidos→400, client desconectado→503; whitelist de `action` e de `state`/`media` coberta | T003 | `[//]` | `whatsapp-bridge/main_test.go` | 🟢 | `[X]` |

## Fase 3, Núcleo

| ID | Descrição | Dependências | Paralelismo | Arquivo alvo | Confidência | Status |
|----|-----------|--------------|-------------|--------------|-------------|--------|
| T001 | Handler `handleGroupParticipants(client) http.HandlerFunc` + structs `GroupParticipantsRequest/Response` + registro `POST /api/group_participants` (junto de main.go:1792). Whitelist `action`∈{add,remove,promote,demote}→`whatsmeow.ParticipantChange`; `group_jid` exige `@g.us`; participantes normalizados via `normalizePhone`+`types.ParseJID` (molde createWhatsAppGroup main.go:914); resposta por participante com `error`/`is_admin`/`add_request` (contrato `interfaces/group-participants.md`) | - | - | `whatsapp-bridge/main.go` | 🟢 | `[X]` |
| T002 | Handler `handleChatPresence(client)` + structs + registro `POST /api/chat_presence`. `state`∈{composing,paused}→`types.ChatPresence`, `media`∈{"",audio}→`types.ChatPresenceMedia`; sem persistência, sem timer (D-06; contrato `interfaces/chat-presence.md`) | T001 | - | `whatsapp-bridge/main.go` | 🟢 | `[X]` |
| T003 | Handler `handleIsOnWhatsApp(client)` + structs + registro `POST /api/is_on_whatsapp`. Prefixa `+` se ausente (D-08); `phones` vazio→400; resposta `results[]` com `query`/`jid`/`is_in` (+`verified_name` quando business; contrato `interfaces/is-on-whatsapp.md`) | T002 | - | `whatsapp-bridge/main.go` | 🟢 | `[X]` |
| T005 | `[//]` Helpers Python `update_group_participants`, `send_chat_presence`, `check_whatsapp` — POST aos 3 endpoints, retorno `(bool, str, payload)` conforme contratos `interfaces/`; molde `react_to_message` (whatsapp.py:975) | - | `[//]` | `whatsapp-mcp-server/whatsapp.py` | 🟢 | `[X]` |

## Fase 4, Integração

| ID | Descrição | Dependências | Paralelismo | Arquivo alvo | Confidência | Status |
|----|-----------|--------------|-------------|--------------|-------------|--------|
| T006 | 3 tools `@mcp.tool` em main.py embrulhando T005, retorno dict `{success, message, ...}`; docstrings orientadas (RF-08): `paused` explícito após composing, validar número antes de send, status por participante | T005 | - | `whatsapp-mcp-server/main.py` | 🟢 | `[X]` |
| T007 | `cd whatsapp-bridge && go build -o whatsapp-bridge . && go test ./...` verde | T004 | - | `whatsapp-bridge/` | 🟢 | `[X]` |
| T008 | `cd whatsapp-mcp-server && python3 -m unittest test_transcribe -v` verde (regressão) + import sanity dos novos helpers/tools | T006 | - | `whatsapp-mcp-server/` | 🟢 | `[X]` |
| T009 | Reiniciar bridge (`kill $(pgrep -f '/whatsapp-bridge$')`, systemd religa) + smoke curl nos 3 endpoints com client conectado (roteiro `onboarding.md`) | T007, T008 | - | `whatsapp-bridge/` | 🟢 | `[X]` |

## Fase 5, Polimento

| ID | Descrição | Dependências | Paralelismo | Arquivo alvo | Confidência | Status |
|----|-----------|--------------|-------------|--------------|-------------|--------|
| T010 | `[//]` Atualizar README (lista de tools MCP) e `whatsmeow-gap-analysis.md` (seção "Estado atual": marcar #7–#9 cobertos) | T006 | `[//]` | `README.md` | 🟢 | `[X]` |
