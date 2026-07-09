# Legacy impact — 003-whatsmeow-gaps-7-8-9

> Data: 2026-07-09 · Confidência: 🟢 CONFIRMADO / 🟡 INFERIDO / 🔴 LACUNA

O que esta entrega tocou no comportamento existente.

| Área do legado | Impacto | Confidência |
|----------------|---------|-------------|
| Bridge REST (`startRESTServer`) | +3 rotas registradas (`/api/group_participants`, `/api/chat_presence`, `/api/is_on_whatsapp`); nenhuma rota existente alterada | 🟢 CONFIRMADO (diff aditivo, reviewer auditou) |
| Pipeline de escrita (`handleMessage`/StoreMessage/messages.db) | Zero impacto — nenhum handler novo toca `messageStore`/SQLite (RN-05); risco de transcrição não acionado | 🟢 CONFIRMADO (reviewer verificou) |
| `normalizePhone` | Reutilizada por 2 funções novas (`parseGroupParticipantJIDs`, `normalizeCheckPhones`); comportamento da função inalterada | 🟢 CONFIRMADO |
| MCP Server (main.py) | 22 → 25 tools; tools existentes intactas | 🟢 CONFIRMADO |
| Modelo de segurança (ADR-001 loopback sem auth) | Inalterado, mas blast radius de processo local hostil aumenta: remove em massa de participantes de grupos onde o dono é admin; scan de números (mitigado por cap de 50/chamada). Devil registrou; token bridge↔MCP fica como follow-up fora de escopo | 🟡 INFERIDO (cenário teórico, sem exploit demonstrado) |
| Presença "digitando" | Estado efêmero enviado ao WhatsApp; expiração automática (~10s) é comportamento observado, não documentado pela lib | 🟡 INFERIDO |
| Resposta parcial de `IsOnWhatsApp` | Descoberto no smoke: servidor omite números não registrados; bridge faz backfill `is_in:false` (mergeIsOnWhatsAppResults) para cumprir RF-06 | 🟢 CONFIRMADO (smoke test real) |
| Prefixo `00` de discagem internacional | `+0055...` passa validação e retorna `is_in:false` (falso-negativo residual aceito pelo reviewer; follow-up: converter `00`→`+`) | 🟡 INFERIDO |
