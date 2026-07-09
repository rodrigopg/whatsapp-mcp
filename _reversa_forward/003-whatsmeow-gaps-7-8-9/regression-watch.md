# regression-watch.md — feature 003 whatsmeow gaps #7–#9

Itens de vigília ligando o código novo às specs `_reversa_sdd/`. Uma futura re-extração `/reversa` (step-04-regression-check) lê isto pra atribuir 🟢/🟡/🔴 de drift.

## Watch table

| ID | Watch item | Código | Spec âncora | Risco de drift |
|----|-----------|--------|-------------|----------------|
| W3-1 | 3 endpoints REST (`/api/group_participants`, `/api/chat_presence`, `/api/is_on_whatsapp`) registrados e usando `UpdateGroupParticipants`/`SendChatPresence`/`IsOnWhatsApp` | `whatsapp-bridge/main.go` handleGroupParticipants/handleChatPresence/handleIsOnWhatsApp | `_reversa_sdd/c4-components.md` (bridge REST), `architecture.md#Fluxos` (fluxo 3) | whatsmeow muda assinatura numa atualização da lib |
| W3-2 | Whitelist de `action` (add/remove/promote/demote) e de server de participante (default + lid) deve permanecer — item inválido → 400 antes de chamar o WhatsApp | `main.go` parseGroupParticipantJIDs | `_reversa_sdd/domain.md#Grupos` (validação de sufixo JID) | "Conserto" removendo a validação deixa passar server lixo/item vazio |
| W3-3 | `normalizeCheckPhones`: normalização + regex `^\d{8,15}$` + **cap de 50 números** por chamada deve permanecer (anti mass-scan/ban) | `main.go` normalizeCheckPhones | requirements RNF robustez; devil finding #3 | Remoção do cap reabre vetor de ban da conta |
| W3-4 | `mergeIsOnWhatsAppResults`: backfill `is_in:false` para números omitidos pelo servidor — 1 item por número de entrada, na ordem | `main.go` mergeIsOnWhatsAppResults | RF-06 (`requirements.md`); descoberto em smoke real (servidor omite não-registrados) | Loop direto sobre a resposta da lib volta a "sumir" com números |
| W3-5 | Nenhum dos 3 handlers escreve em `messages.db`/`whatsapp.db` (ações puras) | `main.go` (3 handlers) | `_reversa_sdd/domain.md#Persistência` regra 4 (INSERT OR REPLACE vs transcrições) | Alguém adicionar persistência de presença/participantes aciona o risco de transcrição |
| W3-6 | Handlers usam `r.Context()` (não `context.Background()`) — cancelamento HTTP propaga | `main.go` (3 handlers) | devil finding #5 | Regressão pra Background() re-desacopla cancelamento |
| W3-7 | 3 tools MCP (`update_group_participants`, `send_chat_presence`, `check_whatsapp`) registradas e delegando à bridge; docstrings orientam paused-após-composing, validar-antes-de-enviar, status por participante | `whatsapp-mcp-server/main.py`, `whatsapp.py` | `_reversa_sdd/c4-components.md` (MCP tools); RF-08 | Tool removida/renomeada quebra clientes MCP |
| W3-8 | Funções puras `parseGroupParticipantJIDs`/`normalizeCheckPhones`/`mergeIsOnWhatsAppResults` mantêm unit tests diretos (anti falso-verde) | `whatsapp-bridge/main_test.go` | devil finding #7 | Inline de volta nos handlers mata a cobertura real |

## Limitações conhecidas (aceitas no ship)

- Prefixo `00` internacional não é convertido para `+` → falso-negativo residual em `check_whatsapp` (follow-up sugerido pelo reviewer).
- Testes de handler cobrem guards (400/405/503) + funções puras; caminho 200 com client real segue sem teste automatizado (mesmo padrão da feature 002).
- Blast radius de processo local hostil aumentou (remove em massa, scan capado em 50); token bridge↔MCP é follow-up de segurança fora deste escopo.

## Histórico

- 2026-07-09 — entrega inicial (W1 ciclo, working tree main). SHIP após 2 rounds de fix: round 1 = 4 furos do devil (validação/cap phones, testes de funções puras, server whitelist, r.Context); round 2 = backfill de números omitidos pelo servidor (descoberto em smoke real). Reviewer: SHIP final sem crítico/aviso.
