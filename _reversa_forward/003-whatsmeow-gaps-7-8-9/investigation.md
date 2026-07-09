# Investigation — 003-whatsmeow-gaps-7-8-9

> Data: 2026-07-09

## Assinaturas whatsmeow (confirmadas via `go doc`, versão do go.mod `v0.0.0-20260516102357`)

```go
func (cli *Client) UpdateGroupParticipants(ctx context.Context, jid types.JID,
    participantChanges []types.JID, action ParticipantChange) ([]types.GroupParticipant, error)
// ParticipantChange: "add" | "remove" | "promote" | "demote" (constantes string)

func (cli *Client) SendChatPresence(ctx context.Context, jid types.JID,
    state types.ChatPresence, media types.ChatPresenceMedia) error
// ChatPresence: "composing" | "paused"; ChatPresenceMedia: "" (texto) | "audio"

func (cli *Client) IsOnWhatsApp(ctx context.Context, phones []string) ([]types.IsOnWhatsAppResponse, error)
// phones em formato internacional COM prefixo `+`
// IsOnWhatsAppResponse{Query string; JID types.JID; IsIn bool; VerifiedName *VerifiedName}
```

`types.GroupParticipant` relevante: `JID`, `PhoneNumber`, `IsAdmin`, `IsSuperAdmin`, `Error int` (código por participante quando a ação falha para ele), `AddRequest` (add pendente de aprovação).

## Moldes no código atual (working tree, pós-PR #6)

| Molde | Local | Uso |
|-------|-------|-----|
| Handler nomeado testável | `handleReact` main.go:905, `handleEdit` :949, `handleRevoke` :988 | Shape dos 3 novos handlers |
| Registro de rota | main.go:1792–1798 | Onde registrar os 3 novos |
| Parse número→JID | `createWhatsAppGroup` main.go:914 + `normalizePhone` :897 | Normalização de participantes |
| Teste de handler | `TestHandleReact` main_test.go:152 | Testes de 400/503/parse |
| Helper Python | `react_to_message` whatsapp.py:975 | POST + retorno `(bool, str)` |
| Tool MCP | tools da 002 em main.py | dict `{success, message, ...}` |

⚠️ Índice jcodemunch está stale (não contém handlers do PR #6) — âncoras acima vieram de grep no working tree. Rodar `register_edit`/reindex antes de confiar no índice para blast radius fino.

## Alternativas avaliadas

1. **4 endpoints separados para participantes** — descartado: a lib já é polimórfica por `action`; um endpoint com whitelist é menor e espelha a API.
2. **Timer de auto-`paused` na bridge** — descartado: WhatsApp expira composing sozinho; timer adiciona goroutine + estado sem necessidade comprovada. Documentar `paused` explícito na docstring.
3. **Persistir eventos de presença** — descartado: RN-03, estado efêmero, fora do modelo de dados.
4. **`IsOnWhatsApp` single-phone** — descartado: lib é batch nativa; single seria N chamadas de rede.

## Fontes externas

- whatsmeow godoc: https://pkg.go.dev/go.mau.fi/whatsmeow (Client.UpdateGroupParticipants, SendChatPresence, IsOnWhatsApp)
- Análise de gaps: `whatsmeow-gap-analysis.md` (raiz do repo), itens #7, #8, #9
