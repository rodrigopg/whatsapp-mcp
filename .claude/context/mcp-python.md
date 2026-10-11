# Contexto: Servidor MCP Python

Servidor MCP (`whatsapp-mcp-server/`) — expõe tools ao Claude/Cursor via stdio, fala HTTP REST
com a bridge Go pra tudo (leitura E escrita). **Não lê SQLite local** — `whatsapp.py` não tem
`import sqlite3`, é 100% stateless. Isso permite bridge remota (VPS) + MCP local sem
compartilhar filesystem.

## Busca accent-insensitive

- `whatsapp.py`: `_strip_accents(text)` continua existindo (usado por `test_transcribe.py` e
  documentação), mas o `unaccent()` de SQL agora roda **do lado da bridge Go**
  (`sqlite3_unaccent` driver derivado, registrado via `sql.Register` em `main.go`), não mais
  registrado pelo Python.
- `list_messages` e `list_chats` continuam suportando busca accent-insensitive (`query` param),
  só que a lógica de comparação está na query SQL dentro da bridge agora.

## Resolução de contatos (LID-aware)

- Toda resolução LID↔PN roda na bridge Go agora (`resolvePhoneToJIDs`, `getContactNameFromStore`
  em `main.go`) — os antigos `_resolve_phone_to_jids`/`_get_contact_name` em Python foram
  removidos.
- `get_direct_chat_by_contact` chama `POST /api/chat/by_contact`; a bridge resolve LID
  internamente e aplica o fallback de nome (`chat.name` vazio/só-dígitos → busca
  `full_name`/`push_name` em `whatsmeow_contacts`).
- `search_contacts` chama `POST /api/contacts/search`.

## Tools expostas

search_contacts, list_messages, list_chats, list_active_chats, get_chat, get_direct_chat_by_contact, get_contact_chats, get_last_interaction, get_message_context, send_message, send_contact, send_file, send_audio_message, download_media, create_group, leave_group.

## Endpoint da bridge + auth

- `WHATSAPP_API_BASE_URL` aponta o MCP/scripts pra bridge não-default. Default `http://localhost:8080/api`.
- `WHATSAPP_API_AUTH_TOKEN` — obrigatório se a bridge exige auth (ver `bridge-go.md`, seção
  `API_AUTH_TOKEN`). Vai em todo request como `Authorization: Bearer <token>`, inclusive nas
  chamadas de escrita já existentes (`send_message`, `send_file`, etc).
- A bridge passa esse var derivado de `WHATSAPP_BRIDGE_PORT` pro sweep de transcrição, pra download bater na porta certa.

## Endpoints REST consumidos (todos POST, JSON body/response)

`/api/chats`, `/api/messages`, `/api/message_context`, `/api/contacts/search`,
`/api/contacts/chats`, `/api/contacts/last_interaction`, `/api/chat`, `/api/chat/by_contact`,
`/api/sender_name` — implementados em `main.go`, contrato documentado em
`.reversa/_reversa_forward/mcp-remote-hardening/interfaces.md`. Timestamps vêm com offset
explícito (ex. `-03:00`), não `Z` — `_parse_ts()` em `whatsapp.py` trata os dois formatos, mas
não depender de `Z` sendo enviado.

## Config no cliente

- Claude Desktop: `~/Library/Application Support/Claude/claude_desktop_config.json`
- Cursor: `~/.cursor/mcp.json`
- Roda via `uv --directory <path>/whatsapp-mcp-server run main.py`.
