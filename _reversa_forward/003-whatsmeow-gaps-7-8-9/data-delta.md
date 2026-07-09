# Data delta — 003-whatsmeow-gaps-7-8-9

> Data: 2026-07-09
> Base: `_reversa_sdd/erd-complete.md`, `_reversa_sdd/data-dictionary.md`

## Resumo

**Nenhuma mudança no modelo de dados.**

- Tabelas novas: nenhuma
- Campos novos/removidos: nenhum
- Índices: nenhum
- Migrações: n/a

## Justificativa

Os três endpoints são **ações puras** (fluxo 3 de `architecture.md#Fluxos principais`): recebem request, chamam whatsmeow, devolvem resposta. Nenhum persiste estado:

- `group_participants` — mudança vive no servidor do WhatsApp; `group_info` (endpoint existente) reflete após a mudança.
- `chat_presence` — efêmero por definição (RN-03 do requirements).
- `is_on_whatsapp` — consulta pura, sem cache local (evita staleness de registro/desregistro de números).

Risco de regressão em transcrições (regra 4 de `domain.md#Persistência / integridade`): **zero** — nenhum caminho toca `messages`.
