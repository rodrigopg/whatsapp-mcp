# legacy-impact.md — feature 002 whatsmeow quick-wins

O que esta entrega tocou no comportamento existente. Escala: 🟢 CONFIRMADO / 🟡 INFERIDO / 🔴 LACUNA.

## Aditivo puro (sem impacto em comportamento existente)

- 🟢 **6 tools MCP novas** (`get_group_info`, `archive_chat`, `resolve_contact`, `react_to_message`, `edit_message`, `delete_message`) — registros novos em `main.py`, nenhum tool existente alterado em assinatura.
- 🟢 **3 handlers REST novos** (`/api/react`, `/api/edit`, `/api/revoke`) em `main.go` — 100% aditivo (diff sem linhas `-` fora de header). Handlers/rotas existentes intactos.
- 🟢 **Extração de handlers** react/edit/revoke em factories nomeadas (`handleReact`/`handleEdit`/`handleRevoke`) — só os 3 novos; não mexeu nos closures existentes.

## Impacto REAL em código pré-existente (refactor aceito pelo usuário — decisão consciente)

Três funções de `whatsapp.py` foram reescritas de leitura direta do SQLite (`whatsapp.db`: `whatsmeow_lid_map`, `whatsmeow_contacts`) para delegação à bridge REST. **Decisão do usuário: aceitar + documentar** (não repor fallback). Alinha com o objetivo da feature 001 (desacoplar do schema whatsmeow).

- 🟢 **`_resolve_phone_to_jids`** — antes: match exato `pn=?` + **fallback fuzzy** `pn LIKE %suffix` + JID alternativo. Agora: GET `/api/resolve_contact` → `client.Store.LIDs.GetLIDForPN` (**exact-match only, sem fuzzy**). **Regressão conhecida:** contato com PN formatado diferente do consultado não resolve mais LID via sufixo → retorna só PN-JID.
- 🟢 **`_get_contact_name`** — antes: SQLite local. Agora: bridge REST. **Regressão:** bridge down → retorna None (antes funcionava offline).
- 🟢 **`search_contacts`** (tool MCP exposta) — antes: SQLite local. Agora: GET `/api/search_contacts`. **Regressão:** bridge down → retorna `[]` (antes retornava do DB local).
- 🟢 Call-sites afetados indiretamente: `get_direct_chat_by_contact`, filtro de sender em `list_messages` — agora fazem round-trip de rede (timeout 10s) em vez de leitura SQLite. Docstrings anotadas com os deltas.

## Limitação de design introduzida (documentada, não é bug)

- 🟢 **react/revoke em grupo com `from_me=false`** → HTTP 400 explícito (participant JID do autor original indisponível na bridge). Casos que funcionam: `from_me=true` (DM+grupo), `from_me=false` (DM). Guard em `main.go:921` (react) e `main.go:1004` (revoke) + testes.
- 🟢 **`edit_message` param `from_me`** → aceito por simetria de API mas **ignorado** (`BuildEdit` hardcoda `FromMe=true`; WhatsApp só edita msg própria). Documentado na docstring.

## Sem impacto

- 🟢 **Schema do DB** — feature não toca `messages.db` nem cria/altera tabelas. Backup preventivo feito (`messages.db.bak-20260707-161053`) só por causa do restart da bridge.
