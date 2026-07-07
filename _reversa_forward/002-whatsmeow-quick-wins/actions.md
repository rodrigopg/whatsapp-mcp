# actions.md — feature 002 whatsmeow quick-wins

Contrato executável. Checkbox `[X]` ao concluir (append-safe). `∥` = paralelizável.

## Grupo Tier A — Python only (zero Go) ∥ entre si

- [X] **A1** `whatsapp.py::get_group_info(jid) -> Tuple[bool, str, Optional[dict]]` — POST/GET `/api/group_info`. Molde: `mark_chat_read` (whatsapp.py:865). Confirmar método do handler (`/api/group_info`) — GET com querystring ou POST body; ler main.go:1435.
- [X] **A2** `whatsapp.py::archive_chat(chat_jid, archive: bool) -> Tuple[bool, str]` — POST `/api/archive_chat` body `{chat_jid, archive}`. Handler exige `archive` não-nulo (main.go:1595).
- [X] **A3** `whatsapp.py::resolve_contact(phone) -> Tuple[bool, str, List[str]]` — GET `/api/resolve_contact` (main.go:1628, é GET). Retorna JIDs.
- [X] **A4** `main.py` — 3 tools MCP `@mcp.tool` (`get_group_info`, `archive_chat`, `resolve_contact`) embrulhando A1–A3. Molde: `mark_chat_as_read` (main.py:300) → dict `{success, message, ...}`.

## Grupo Tier B — Go handler + Python. Handlers ∥ entre si; Python depende do Go

### Go (whatsapp-bridge/main.go) — 3 handlers + 3 request structs
- [X] **B1** struct `ReactRequest{ChatJID, MessageID, Emoji string; FromMe bool}` + `POST /api/react`. Parse JID, `client.BuildReaction(chatJID, senderJID, msgID, emoji)`, `client.SendMessage`. senderJID = próprio se FromMe, senão o remetente (para msg de terceiro). Molde estrutural: handler `mark_chat_unread` (main.go:1548) + send (main.go:482).
- [X] **B2** struct `EditRequest{ChatJID, MessageID, NewText string; FromMe bool}` + `POST /api/edit`. `client.BuildEdit(chatJID, msgID, newMsg)` onde newMsg = `&waProto.Message{Conversation: proto.String(newText)}`. SendMessage.
- [X] **B3** struct `RevokeRequest{ChatJID, MessageID string; FromMe bool}` + `POST /api/revoke`. `client.BuildRevoke(chatJID, senderJID, msgID)`. SendMessage. senderJID conforme FromMe.
- [X] **B4** Reutilizar `MarkChatResponse` (ou criar `ActionResponse{Success bool; Message string}`) pro corpo de resposta. Não duplicar tipo se já existe equivalente.

### Testes Go (whatsapp-bridge/main_test.go)
- [X] **B5** Testes de parsing/validação dos 3 novos handlers (request inválido → 400; client desconectado → 503). Molde: testes existentes de mark_chat.

### Python (depende de B1–B3 compilados)
- [X] **B6** `whatsapp.py` — helpers `react_to_message`, `edit_message`, `delete_message` (POST aos 3 endpoints). Molde: `mark_chat_read`.
- [X] **B7** `main.py` — 3 tools MCP correspondentes com docstrings claras (emoji vazio = remove reação; delete = apaga p/ todos).

## Integração / ship
- [X] **B8** `go build -o whatsapp-bridge .` + `go test ./...` verde.
- [X] **B9** `python3 -m unittest discover` verde (ou o comando de teste do server).
- [X] **B10** Recompilar binário + reiniciar bridge (systemd). Smoke test: `curl` num endpoint novo (client conectado).
