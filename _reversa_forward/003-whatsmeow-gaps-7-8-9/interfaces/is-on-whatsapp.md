# Contrato: POST /api/is_on_whatsapp

> Novo endpoint HTTP loopback (bind 127.0.0.1, sem auth — ADR-001).

## Request

```json
POST /api/is_on_whatsapp
Content-Type: application/json

{"phones": ["556291788888", "+5562988887777"]}
```

- `phones` (array string, obrigatório, ≥1): números em formato internacional. `+` opcional na entrada — a bridge prefixa `+` se ausente (D-08), pois a lib exige.

## Response 200

```json
{
  "success": true,
  "message": "2 number(s) checked",
  "results": [
    {"query": "+556291788888", "jid": "556291788888@s.whatsapp.net", "is_in": true},
    {"query": "+5562988887777", "jid": "", "is_in": false}
  ]
}
```

- Um item por número consultado, na ordem da entrada. Número sem WhatsApp: `is_in: false`, sem erro (RF-06).
- Business verificado: campo extra `verified_name` (string) quando presente.

## Erros

| Status | Condição |
|--------|----------|
| 400 | JSON inválido; `phones` vazio |
| 405 | Método ≠ POST |
| 500 | Erro whatsmeow na consulta |
| 503 | Client desconectado |

## Semântica

- Consulta pura, sem cache local (staleness). Idempotente.
- Uso responsável: lotes pequenos, sem varredura em massa (risco de rate-limit/ban — roadmap risco 4).

## Revisão 1 (pós ship-gate, 2026-07-09 — devil's advocate)

Endurecimento do contrato (furo #3 do devil):
- Cada phone é normalizado com `normalizePhone` (remove `+`, espaços, hífens) antes de prepender `+`; resultado deve casar `^\d{8,15}$` — item inválido → **400** com o item na mensagem (antes: enviado cru à lib, falso-negativo silencioso).
- Lista capada em **50** números → 400 se exceder (mitiga mass-scan/ban).
