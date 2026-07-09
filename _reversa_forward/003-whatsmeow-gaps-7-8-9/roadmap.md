# Roadmap: Gaps whatsmeow #7–#9 — participantes de grupo, typing e validação de número

> Identificador: `003-whatsmeow-gaps-7-8-9`
> Data: `2026-07-09`
> Requirements: `_reversa_forward/003-whatsmeow-gaps-7-8-9/requirements.md`
> Confidência: 🟢 CONFIRMADO, 🟡 INFERIDO, 🔴 LACUNA

## 1. Resumo da abordagem

Replicar o molde da feature 002 (Tier B): três handlers Go **nomeados** no padrão `handleX(client *whatsmeow.Client) http.HandlerFunc` (main.go:905/949/988 — `handleReact`/`handleEdit`/`handleRevoke`), registrados em `startRESTServer` (main.go:1792–1798), com testes de parsing/validação em `main_test.go` (molde `TestHandleReact`, main_test.go:152). Do lado Python, três helpers em `whatsapp.py` (molde `react_to_message`, whatsapp.py:975) e três `@mcp.tool` em `main.py`. Nenhuma mudança de schema, nenhuma escrita em `messages.db` — as três operações são ações puras via REST (fluxo 3 de `architecture.md#Fluxos principais`). Assinaturas whatsmeow confirmadas via `go doc` (ver `investigation.md`).

## 2. Princípios aplicados

`.reversa/principles.md` não existe neste projeto. Aplicam-se as regras inegociáveis do `CLAUDE.md`:

| Princípio (CLAUDE.md) | Como a feature se relaciona | Status |
|-----------|------------------------------|--------|
| REST liga 127.0.0.1 | Novos endpoints herdam o bind existente, sem mudança | respeita |
| Recompilar binário após mudar main.go | Passo de ship no actions.md | respeita |
| Sync/escrita não pode impactar transcrições | Nenhum endpoint escreve em `messages` | respeita |
| Go 1.25+, `go test ./...` verde | Testes novos no molde da 002 | respeita |

## 3. Decisões técnicas

| ID | Decisão | Justificativa | Alternativas descartadas | Confidência |
|----|---------|----------------|--------------------------|-------------|
| D-01 | Um endpoint único `POST /api/group_participants` com campo `action` (`add`/`remove`/`promote`/`demote`) | `UpdateGroupParticipants(ctx, jid, []JID, action)` já é polimórfico por ação; 4 endpoints seriam boilerplate | 4 endpoints separados | 🟢 |
| D-02 | Mapear `action` string diretamente para `whatsmeow.ParticipantChange` (mesmos literais: "add", "remove", "promote", "demote"); validar whitelist antes de chamar a lib | Constantes da lib são strings idênticas | enum numérico próprio | 🟢 |
| D-03 | Resposta de participantes com status por item: `[]types.GroupParticipant` traz campo `Error int` por participante (0 = ok) e `AddRequest` p/ add pendente | Confirma RN-02 do requirements; API já retorna por-participante | resposta colapsada bool | 🟢 |
| D-04 | Participantes de entrada aceitos como número ou JID; normalizar com `normalizePhone` (main.go:897) + `types.ParseJID`, mesmo tratamento do `create_group` (main.go:914) | Consistência com criação de grupo existente | exigir JID completo | 🟢 |
| D-05 | `POST /api/chat_presence` com body `{chat_jid, state, media}`; `state` ∈ {`composing`,`paused`}, `media` ∈ {``,`audio`} mapeando `types.ChatPresence`/`ChatPresenceMedia` | Assinatura `SendChatPresence(ctx, jid, state, media)`; literais da lib são "composing"/"paused" e ""/"audio" | timer de auto-paused na bridge | 🟢 |
| D-06 | Sem timer de auto-paused na bridge: o WhatsApp já expira "digitando…" sozinho (~10s) e o chamador pode mandar `paused` explícito | Estado efêmero, menos complexidade; RN-03 | goroutine com timeout | 🟡 (comportamento de expiração do WhatsApp é observado, não documentado) |
| D-07 | `POST /api/is_on_whatsapp` com body `{phones: []string}` em lote; resposta ecoa `query`, `jid`, `is_in` de `types.IsOnWhatsAppResponse` | `IsOnWhatsApp(ctx, phones []string)` já é batch; RN-04 | single-phone por chamada | 🟢 |
| D-08 | Prefixar `+` nos números antes de chamar `IsOnWhatsApp` se ausente (lib exige formato internacional com `+`) | doc da lib: "including the `+` prefix"; usuários do MCP passam sem `+` (convenção de `resolve_contact`) | rejeitar sem `+` | 🟢 |
| D-09 | Handlers nomeados fora de `startRESTServer` (não closures inline) | Testabilidade — padrão introduzido pela 002 e coberto por `main_test.go` | closure inline como handlers pré-002 | 🟢 |
| D-10 | Tools MCP: `update_group_participants`, `send_chat_presence`, `check_whatsapp` — retorno dict `{success, message, ...}` | Molde das 6 tools da 002 em `main.py` | — | 🟢 |

## 4. Premissas

| Premissa | Origem (`requirements.md` seção) | Risco se errada |
|----------|----------------------------------|-----------------|
| WhatsApp expira "composing" sozinho sem `paused` explícito | RN-03 / D-06 | Chat fica "digitando" indefinido — mitigação: documentar `paused` na docstring da tool |
| `GroupParticipant.Error != 0` é o indicador confiável de recusa por participante | RN-02 / D-03 | Status individual incorreto na resposta — verificar em smoke test com grupo real |

## 5. Delta arquitetural

| Componente | Arquivo de origem no legado | Tipo de mudança | Resumo |
|------------|------------------------------|-----------------|--------|
| Bridge REST | `_reversa_sdd/architecture.md#Visão geral` (Bridge Go) | contrato-novo | +3 endpoints: `/api/group_participants`, `/api/chat_presence`, `/api/is_on_whatsapp` |
| MCP Server | `_reversa_sdd/architecture.md#Visão geral` (MCP Server) | componente-alterado | +3 helpers `whatsapp.py`, +3 tools `main.py` (16 → 25 tools contando a 002) |
| Testes Go | `_reversa_sdd/architecture.md#Cobertura de testes` | componente-alterado | +3 blocos de teste de handler em `main_test.go` |

Sem mudança em: event loop, history sync, transcrição, contratos de log, schema SQLite.

## 6. Delta no modelo de dados

- Resumo das mudanças: **nenhuma**. Nenhuma tabela, campo ou índice alterado; nenhuma escrita em `messages.db`/`whatsapp.db` pelos novos endpoints.
- Detalhe completo em: `_reversa_forward/003-whatsmeow-gaps-7-8-9/data-delta.md`

## 7. Delta de contratos externos

| Contrato | Tipo | Arquivo de detalhe |
|----------|------|--------------------|
| `POST /api/group_participants` | HTTP | `interfaces/group-participants.md` |
| `POST /api/chat_presence` | HTTP | `interfaces/chat-presence.md` |
| `POST /api/is_on_whatsapp` | HTTP | `interfaces/is-on-whatsapp.md` |

## 8. Plano de migração

n/a — sem migração de dados. Ship = recompilar binário + reiniciar bridge (systemd `Restart=always`, matar processo).

## 9. Riscos e mitigações

| Risco | Impacto | Probabilidade | Mitigação |
|-------|---------|---------------|-----------|
| `UpdateGroupParticipants` sem ser admin → erro por participante mal interpretado como sucesso | médio | médio | D-03: mapear `Error` por participante; teste manual em grupo onde o dono não é admin |
| Participante passado como LID (`@lid`) e não PN | médio | baixo | D-04: aceitar JID completo já parseado; `types.ParseJID` lida com sufixo |
| Índice jcodemunch stale durante delivery (não reflete PR #6) | baixo | alto (confirmado) | Âncoras deste plano tiradas do working tree via grep, não do índice |
| Rate-limit/ban por uso abusivo de `IsOnWhatsApp` em massa | alto | baixo | Docstring orienta lotes pequenos; sem loop automático |

## 10. Critério de pronto

- [ ] Todas as ações do `actions.md` marcadas `[X]`
- [ ] `go build` + `go test ./...` verdes; `python3 -m unittest` verde
- [ ] Binário recompilado, bridge reiniciada, smoke test curl nos 3 endpoints
- [ ] `regression-watch.md` gerado
