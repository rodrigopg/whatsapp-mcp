# Contrato: POST /api/group_participants

> Novo endpoint HTTP loopback (bind 127.0.0.1, sem auth — ADR-001).

## Request

```json
POST /api/group_participants
Content-Type: application/json

{
  "group_jid": "120363021234567890@g.us",
  "participants": ["556291788888", "5562988887777@s.whatsapp.net"],
  "action": "add"
}
```

- `group_jid` (string, obrigatório): JID de grupo, sufixo `@g.us` obrigatório.
- `participants` (array string, obrigatório, ≥1): número internacional sem `+` OU JID completo. Normalização via `normalizePhone` + `types.ParseJID` (molde `create_group`).
- `action` (string, obrigatório): `add` | `remove` | `promote` | `demote`. Whitelist validada antes de chamar a lib.

## Response 200

```json
{
  "success": true,
  "message": "add applied to 2 participant(s)",
  "participants": [
    {"jid": "556291788888@s.whatsapp.net", "is_admin": false, "error": 0},
    {"jid": "5562988887777@s.whatsapp.net", "is_admin": false, "error": 403, "add_request": true}
  ]
}
```

- Status por participante (RN-02): `error: 0` = aplicado; `error != 0` = código WhatsApp de recusa (ex.: 403 privacidade, 409 já membro); `add_request: true` = virou convite pendente.
- `success: true` significa "chamada aceita pelo WhatsApp", não "todos aplicados" — inspecionar array.

## Erros

| Status | Condição |
|--------|----------|
| 400 | JSON inválido; `group_jid` não-`@g.us`; `participants` vazio; participante não parseável; `action` fora da whitelist |
| 405 | Método ≠ POST |
| 500 | Erro whatsmeow na chamada inteira (ex.: bridge não é membro do grupo) |
| 503 | Client desconectado |

## Idempotência / timeout

- `add` de quem já é membro / `remove` de quem não é: WhatsApp responde erro por participante — chamada é segura de repetir.
- Timeout: contexto default do handler (sem timeout custom; consistente com handlers existentes).

## Revisão 1 (pós ship-gate, 2026-07-09 — devil's advocate)

Endurecimento do contrato (furos #2 e #5 do devil):
- Participante como JID: só servers `s.whatsapp.net` (default) e `lid` (hidden user) aceitos; outro server → **400**. Item vazio → **400** (antes: pulado em silêncio).
- Handlers usam `r.Context()` (cancelamento do HTTP propaga à chamada whatsmeow).
