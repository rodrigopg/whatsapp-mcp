# Feature 002 — whatsmeow quick-wins no MCP server

**Ciclo:** W1 (spec-anchored-delivery) · **Modo:** legacy · **Branch:** `feat/whatsmeow-quick-wins`
**Fonte de escopo:** `whatsmeow-gap-analysis.md`

## Objetivo

Expor no MCP server features do whatsmeow já disponíveis na lib mas ausentes das tools MCP. Dois tiers:

- **Tier A** — 3 tools que só embrulham handlers REST **já existentes** na bridge (zero Go).
- **Tier B** — 3 features de mensagem que precisam de novo handler Go (`Build*`+`SendMessage`) + tool Python.

## Escopo

### Tier A (zero Go — REST handler já existe)

| Tool MCP | Helper `whatsapp.py` | REST existente | Método whatsmeow (por trás) |
|----------|----------------------|----------------|------------------------------|
| `get_group_info(jid)` | `get_group_info` | `GET/POST /api/group_info` | `GetGroupInfo` |
| `archive_chat(chat_jid, archive: bool)` | `archive_chat` | `POST /api/archive_chat` | `BuildArchive` (app-state) |
| `resolve_contact(phone)` | `resolve_contact` | `GET /api/resolve_contact` | `ResolveContactQRLink`/LID map |

### Tier B (Go handler + Python tool)

| Tool MCP | Handler REST novo | whatsmeow |
|----------|-------------------|-----------|
| `react_to_message(chat_jid, message_id, emoji, from_me)` | `POST /api/react` | `client.BuildReaction` + `SendMessage` |
| `edit_message(chat_jid, message_id, new_text, from_me)` | `POST /api/edit` | `client.BuildEdit` + `SendMessage` |
| `delete_message(chat_jid, message_id, from_me)` (revoke, delete-for-everyone) | `POST /api/revoke` | `client.BuildRevoke` + `SendMessage` |

Emoji vazio em `react_to_message` = remover reação (contrato whatsmeow: string vazia).
`from_me` distingue reagir/editar/apagar mensagem própria vs. de terceiro (afeta o participant no `BuildReaction`/`BuildRevoke`).

## Fora de escopo

Participant management, group setters, polls, presence, IsOnWhatsApp, newsletters. Ficam pra ciclos futuros (ver gap-analysis Tier B restante + C).

## Regras herdadas (CLAUDE.md)

- Go 1.25+; **recompilar binário** após mudar main.go; reiniciar bridge (systemd `whatsapp-bridge-dev1`, `sudo systemctl restart` — root em outra sessão).
- REST liga 127.0.0.1; não expor 0.0.0.0.
- Backup `messages.db` antes de restart — **feito** (`messages.db.bak-20260707-161053`).
- Testes: `go build -o whatsapp-bridge .` + `go test ./...`; `python3 -m unittest`.
- PR contra `rodrigopg/main`.

## Âncoras de spec (`_reversa_sdd/`)

- `_reversa_sdd/c4-components.md` — bridge REST + MCP tools são componentes; novas rotas/tools estendem o mesmo container.
- `_reversa_sdd/architecture.md` — padrão bridge Go ↔ MCP Python via REST local.
- `_reversa_sdd/data-dictionary.md` — sem mudança de schema (feature não toca DB).

## Critério de aceite

1. `go build` compila, `go test ./...` passa (novos handlers testados em `main_test.go`).
2. `python3 -m unittest` passa.
3. 6 tools novas registradas e funcionais contra a bridge.
4. Nenhum símbolo/handler/tool existente alterado em assinatura.
