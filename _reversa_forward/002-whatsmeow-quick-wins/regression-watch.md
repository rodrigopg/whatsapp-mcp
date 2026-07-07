# regression-watch.md — feature 002 whatsmeow quick-wins

Itens de vigília ligando o código novo às specs `_reversa_sdd/`. Uma futura re-extração `/reversa` (step-04-regression-check) lê isto pra atribuir 🟢/🟡/🔴 de drift.

## Watch table

| ID | Watch item | Código | Spec âncora | Risco de drift |
|----|-----------|--------|-------------|----------------|
| W2-1 | 3 endpoints REST novos (`/api/react`,`/api/edit`,`/api/revoke`) devem continuar registrados e usar `client.Build{Reaction,Edit,Revoke}` + `SendMessage` | `whatsapp-bridge/main.go` handleReact/handleEdit/handleRevoke | `_reversa_sdd/c4-components.md` (bridge REST), `architecture.md` | whatsmeow renomeia/muda assinatura de `Build*` numa atualização da lib |
| W2-2 | Guard group+`from_me=false` → 400 em react/revoke deve permanecer | `main.go:921,1004` | `_reversa_sdd/domain.md` (regras de mensagem) | Alguém "conserta" removendo o guard sem plumbar participant JID → volta o bug de key malformada |
| W2-3 | 6 tools MCP registradas e delegando à bridge | `whatsapp-mcp-server/main.py` | `_reversa_sdd/c4-components.md` (MCP tools) | Tool removida/renomeada quebra clientes MCP |
| W2-4 | **Refactor de desacoplamento**: `_resolve_phone_to_jids`/`_get_contact_name`/`search_contacts` leem via bridge, NÃO via SQLite direto | `whatsapp.py:36,56,453` | `_reversa_sdd/data-dictionary.md`, feature 001 (desacoplar schema) | Regressão pra leitura direta do `whatsmeow_lid_map`/`whatsmeow_contacts` reacopla ao schema interno |
| W2-5 | Regressão conhecida aceita: resolução LID é exact-match (sem fuzzy suffix). Se reintroduzirem fuzzy, é mudança de comportamento a registrar | `main.go` resolveContactJIDs | feature 001 | — |

## Nota de convergência com feature 001

O refactor W2-4 **avança** o objetivo da feature 001 (desacoplar o MCP server do schema interno do whatsmeow). Uma re-extração deve tratar isto como progresso, não drift: a leitura direta de `whatsmeow_lid_map` que a 001 queria eliminar foi de fato removida do lado Python nesta entrega.

## Histórico

- 2026-07-07 — entrega inicial (W1 ciclo, branch `feat/whatsmeow-quick-wins`). SHIP após 1 round de FIX (bug de key em grupo + testes de handler + docs).
