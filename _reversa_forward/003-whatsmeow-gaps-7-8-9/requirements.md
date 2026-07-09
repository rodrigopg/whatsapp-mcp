# Requirements: Gaps whatsmeow #7–#9 — participantes de grupo, typing e validação de número

> Identificador: `003-whatsmeow-gaps-7-8-9`
> Data: `2026-07-09`
> Pasta da extração reversa: `_reversa_sdd/`
> Confidência: 🟢 CONFIRMADO, 🟡 INFERIDO, 🔴 LACUNA / DÚVIDA

## 1. Resumo executivo

Expor no MCP os três gaps Tier B de alto valor restantes do `whatsmeow-gap-analysis.md` após a feature 002 ter coberto #1–#6: **gerenciar participantes de grupo** (add/remove/promote/demote — apontado como "maior gap único"), **typing/presença no chat** (bot mais natural) e **validação de número no WhatsApp** (evita enviar para número inexistente). Beneficia o assistente de IA que opera o WhatsApp pessoal do dono. Padrão de entrega idêntico à feature 002: handler REST na bridge Go + helper `whatsapp.py` + tool `@mcp.tool` em `main.py`.

## 2. Contexto a partir do legado

| Fonte | Trecho relevante | Confidência |
|-------|------------------|-------------|
| `_reversa_sdd/architecture.md#Fluxos principais` | Fluxo 3 (ação pela IA): tool MCP → `whatsapp.py` POST REST → bridge → whatsmeow → WA. As três features seguem esse fluxo | 🟢 |
| `_reversa_sdd/architecture.md#Decisões estruturais` | ADR-001: REST loopback sem auth — novos endpoints herdam o mesmo modelo (bind 127.0.0.1) | 🟢 |
| `_reversa_sdd/domain.md#Grupos` | Regras 14–16 (nome ≤25 runes, só `@g.us` pode leave, próprio número auto-adicionado) — base do domínio de grupos existente | 🟢 |
| `_reversa_sdd/domain.md#Glossário` | JID formatos: `@s.whatsapp.net` (PN), `@g.us` (grupo), `@lid` (anonimizado) — participantes chegam como PN ou LID | 🟢 |
| `whatsmeow-gap-analysis.md#Tabela de gaps` | #7 `UpdateGroupParticipants`, #8 `SendChatPresence`, #9 `IsOnWhatsApp` — métodos existem na lib, sem REST handler | 🟢 |
| `_reversa_forward/002-whatsmeow-quick-wins/actions.md` | Molde de entrega Tier B: struct request + handler POST + testes de parsing/validação + helper Python + tool MCP | 🟢 |
| `_reversa_sdd/architecture.md#Cobertura de testes` | REST handlers sem teste automatizado era lacuna; feature 002 introduziu `main_test.go` com testes de handler — manter o padrão | 🟢 |

## 3. Personas e cenários de uso

| Persona | Objetivo | Cenário-chave |
|---------|----------|---------------|
| Assistente de IA (Claude via MCP) | Administrar grupos do dono | "Adiciona a Maria no grupo TBC Agro e promove o João a admin" |
| Assistente de IA (whats-assist) | Parecer natural ao responder | Envia "digitando…" antes de mandar resposta rascunhada |
| Assistente de IA | Validar destinatário antes de enviar | "Esse número 556299xxxxx tem WhatsApp?" antes de `send_message` falhar silencioso |

## 4. Regras de negócio novas ou alteradas

1. **RN-01:** Gerenciamento de participantes aceita quatro ações — `add`, `remove`, `promote`, `demote` — e aplica-se somente a JIDs de grupo (`@g.us`); JID não-grupo → erro 400 sem chamada ao WhatsApp. 🟢
   - Origem no legado: coerente com `_reversa_sdd/domain.md#Grupos` regra 15 (operações de grupo validam sufixo `@g.us`)
   - Tipo: nova
2. **RN-02:** O resultado de participantes é **por participante**: o WhatsApp pode aceitar uns e recusar outros (ex.: privacidade impede add; bridge não é admin). A resposta reporta status individual, nunca colapsa em sucesso/falha único. 🟡 (comportamento da API whatsmeow, confirmar assinatura no plan)
   - Tipo: nova
3. **RN-03:** Typing é **estado efêmero** — nada é persistido em `messages.db`; a bridge apenas repassa `composing`/`paused` (e variante de áudio) ao WhatsApp. 🟢
   - Tipo: nova
4. **RN-04:** Validação de número aceita **lote** (uma chamada, N números) porque `IsOnWhatsApp` da lib recebe slice; entrada em formato internacional com código do país (mesma convenção de `resolve_contact`). 🟡
   - Tipo: nova
5. **RN-05:** Nenhuma das três operações altera o pipeline de escrita canônica (`handleMessage`/StoreMessage) — são ações puras via REST, sem tocar em transcrições existentes. 🟢
   - Origem no legado: `_reversa_sdd/domain.md#Persistência / integridade` regra 4 (risco INSERT OR REPLACE não é acionado)
   - Tipo: nova

## 5. Requisitos Funcionais

| ID | Requisito | Prioridade | Critério de aceite | Confidência |
|----|-----------|------------|--------------------|-------------|
| RF-01 | Endpoint REST `POST /api/group_participants` — body `{group_jid, participants[], action}` chamando `UpdateGroupParticipants` | Must | curl com grupo real retorna 200 e lista de status por participante; JID não-grupo → 400 | 🟢 |
| RF-02 | Tool MCP `update_group_participants(group_jid, participants, action)` embrulhando RF-01 | Must | Tool registrada, retorna dict `{success, message, participants[]}` | 🟢 |
| RF-03 | Endpoint REST `POST /api/chat_presence` — body `{chat_jid, state}` (`composing`/`paused`, variante mídia/áudio) chamando `SendChatPresence` | Must | curl retorna 200; celular do dono mostra "digitando…" no chat alvo | 🟢 |
| RF-04 | Tool MCP `send_chat_presence(chat_jid, state, media?)` embrulhando RF-03 | Must | Tool registrada, estado inválido → erro claro | 🟢 |
| RF-05 | Endpoint REST `POST /api/is_on_whatsapp` — body `{phones[]}` chamando `IsOnWhatsApp` | Must | curl com número válido e inválido retorna array `{query, jid, is_in}` | 🟢 |
| RF-06 | Tool MCP `check_whatsapp(phones)` embrulhando RF-05 | Must | Tool registrada; número sem WhatsApp reporta `is_in: false` sem exceção | 🟢 |
| RF-07 | Testes Go de parsing/validação dos 3 handlers (request inválido → 400; client desconectado → 503), molde `main_test.go` da feature 002 | Must | `go test ./...` verde | 🟢 |
| RF-08 | Docstrings das tools orientando o modelo (ex.: typing antes de send; validar número antes de send em destinatário novo) | Should | Docstrings presentes e específicas | 🟡 |

## 6. Requisitos Não Funcionais

| Tipo | Requisito | Evidência ou justificativa | Confidência |
|------|-----------|----------------------------|-------------|
| Segurança | Endpoints novos herdam bind `127.0.0.1` sem auth (ADR-001); nenhum expõe dado além do que a sessão whatsmeow já permite | `_reversa_sdd/adrs/001-rest-api-loopback-sem-auth.md` | 🟢 |
| Robustez | Operações de participantes em lote grande devem tolerar recusa parcial sem abortar o batch (status por item, RN-02) | `_reversa_sdd/domain.md#Chat state` regra 18 mostra precedente de falha sob rajada | 🟡 |
| Compatibilidade | Go 1.25+, recompilar binário e reiniciar bridge (systemd) após mudar `main.go` | CLAUDE.md regras inegociáveis | 🟢 |
| Observabilidade | Erros por participante logados em `bridge.log` no padrão de log existente | Padrão dos handlers atuais | 🟡 |

## 7. Critérios de Aceitação

```gherkin
Cenário: Adicionar participante a grupo
  Dado a bridge conectada e um grupo em que o dono é admin
  Quando a tool update_group_participants é chamada com action "add" e um número válido
  Então a resposta traz o participante com status de sucesso e ele aparece no group_info

Cenário: Ação de participante em JID que não é grupo
  Dado a bridge conectada
  Quando update_group_participants é chamada com um JID @s.whatsapp.net
  Então a bridge responde 400 sem chamar o WhatsApp

Cenário: Typing aparece no destinatário
  Dado a bridge conectada
  Quando send_chat_presence é chamada com state "composing" para um chat direto
  Então o WhatsApp do destinatário exibe "digitando…" até o estado "paused" ou timeout do WhatsApp

Cenário: Número sem WhatsApp
  Dado a bridge conectada
  Quando check_whatsapp é chamada com um número inexistente
  Então a resposta contém is_in: false para aquele número, sem erro

Cenário: Bridge desconectada
  Dado a bridge sem sessão WhatsApp ativa
  Quando qualquer um dos 3 endpoints é chamado
  Então a resposta é 503 com mensagem clara
```

## 8. Prioridade MoSCoW

| Item | MoSCoW | Justificativa |
|------|--------|---------------|
| RF-01/RF-02 (participantes) | Must | "Maior gap único" segundo a análise; destrava administração de grupos |
| RF-05/RF-06 (is_on_whatsapp) | Must | Barato e evita falha silenciosa de envio para número inválido |
| RF-03/RF-04 (typing) | Must | Barato; requisito direto do caso de uso whats-assist |
| RF-07 (testes handlers) | Must | Padrão estabelecido na feature 002; lacuna histórica de testes REST |
| RF-08 (docstrings orientadas) | Should | Melhora uso pelo modelo, não bloqueia ship |

## 9. Esclarecimentos

> Nenhuma sessão de dúvidas registrada ainda. Rode `/reversa-clarify` quando houver `[DÚVIDA]` pendente.

## 10. Lacunas

- Nenhuma lacuna bloqueante. Dois pontos 🟡 a confirmar no `/reversa-plan` lendo a assinatura real da lib: shape do retorno de `UpdateGroupParticipants` (status por participante, RN-02) e shape do lote de `IsOnWhatsApp` (RN-04). Ambos são decisões de design, não ambiguidade de requisito.

## 11. Histórico de alterações

| Data | Alteração | Autor |
|------|-----------|-------|
| 2026-07-09 | Versão inicial gerada por `/reversa-requirements` (via W1) | reversa |
