# Contrato: POST /api/chat_presence

> Novo endpoint HTTP loopback (bind 127.0.0.1, sem auth — ADR-001).

## Request

```json
POST /api/chat_presence
Content-Type: application/json

{
  "chat_jid": "556291788888@s.whatsapp.net",
  "state": "composing",
  "media": ""
}
```

- `chat_jid` (string, obrigatório): JID direto ou de grupo.
- `state` (string, obrigatório): `composing` (digitando) | `paused` (parou).
- `media` (string, opcional): `""` (default, texto) | `audio` (gravando áudio). Mapeia `types.ChatPresenceMedia`.

## Response 200

```json
{"success": true, "message": "chat presence sent"}
```

## Erros

| Status | Condição |
|--------|----------|
| 400 | JSON inválido; `chat_jid` não parseável; `state` fora de {composing,paused}; `media` fora de {"",audio} |
| 405 | Método ≠ POST |
| 500 | Erro whatsmeow |
| 503 | Client desconectado |

## Semântica

- **Efêmero**: nada persistido (RN-03). Sem side effect em `messages.db`.
- WhatsApp expira `composing` sozinho (~10s observado); enviar `paused` para encerrar antes. Sem timer na bridge (D-06).
- Idempotente: repetir `composing` renova o indicador.
