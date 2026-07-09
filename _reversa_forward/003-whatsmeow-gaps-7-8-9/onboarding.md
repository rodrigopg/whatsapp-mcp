# Onboarding — testar 003-whatsmeow-gaps-7-8-9

> Para um humano validando a feature pela primeira vez nesta VM Linux (bridge systemd, porta 8081).

## Pré-requisitos

1. Bridge compilada e rodando: `pgrep -fa whatsapp-bridge` (systemd religa sozinho após `kill`).
2. Sessão WhatsApp pareada (sem QR pendente em `http://127.0.0.1:8081/qr`).
3. Um grupo de teste onde o número pareado é **admin** (para add/remove/promote/demote).

## Teste 1 — validação de número

```bash
curl -s -X POST http://127.0.0.1:8081/api/is_on_whatsapp \
  -H 'Content-Type: application/json' \
  -d '{"phones": ["556291788888", "5562000000000"]}' | jq
```

Esperado: array com `is_in: true` para o primeiro (número do dono) e `is_in: false` para o inexistente.

## Teste 2 — typing

```bash
# num chat direto qualquer (use seu segundo número pra observar)
curl -s -X POST http://127.0.0.1:8081/api/chat_presence \
  -H 'Content-Type: application/json' \
  -d '{"chat_jid": "5562XXXXXXXX@s.whatsapp.net", "state": "composing"}' | jq
```

Esperado: `success: true` e "digitando…" visível no aparelho do destinatário; some sozinho em ~10s ou após enviar `{"state": "paused"}`.

## Teste 3 — participantes de grupo

```bash
GROUP="1203XXXXXXXX@g.us"   # grupo de teste onde você é admin
NUM="5562YYYYYYYY"          # número com WhatsApp (valide no Teste 1 antes)

curl -s -X POST http://127.0.0.1:8081/api/group_participants \
  -H 'Content-Type: application/json' \
  -d "{\"group_jid\": \"$GROUP\", \"participants\": [\"$NUM\"], \"action\": \"add\"}" | jq
# conferir: status por participante; depois validar presença via
curl -s "http://127.0.0.1:8081/api/group_info?jid=$GROUP" | jq '.participants'
# repetir com action promote → demote → remove
```

Casos negativos: `action: "kick"` → 400; `group_jid` `@s.whatsapp.net` → 400; participante com privacidade restrita → item com `error != 0` (add vira convite pendente em `add_request`).

## Teste 4 — via MCP

No Claude conectado ao MCP whatsapp: pedir "verifica se 5562... tem WhatsApp", "manda um digitando pro fulano", "adiciona fulano no grupo X". Tools: `check_whatsapp`, `send_chat_presence`, `update_group_participants`.

## Teste 5 — regressão

```bash
cd whatsapp-bridge && go test ./...
cd ../whatsapp-mcp-server && python3 -m unittest test_transcribe -v
```
