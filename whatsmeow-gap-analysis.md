# whatsmeow → MCP: análise de features ausentes

**Data:** 2026-07-07
**whatsmeow:** `v0.0.0-20260516102357-8d3700152a69`
**Camadas:** whatsmeow (lib Go) → bridge REST (`main.go`) → tools MCP (`main.py`)

## Estado atual (o que já existe)

**25 tools MCP:** `search_contacts`, `list_messages`, `list_chats`, `get_chat`, `get_direct_chat_by_contact`, `get_contact_chats`, `get_last_interaction`, `get_message_context`, `send_message`, `send_file`, `send_audio_message`, `download_media`, `create_group`, `leave_group`, `mark_chat_as_read`, `mark_chat_as_unread`, `get_group_info`, `archive_chat`, `resolve_contact`, `react_to_message`, `edit_message`, `delete_message`, `update_group_participants`, `send_chat_presence`, `check_whatsapp`.

**19 REST handlers:** `/api/send`, `/api/download`, `/api/mediaretry`, `/api/create_group`, `/api/group_info`, `/api/leave_group`, `/api/mark_chat_read`, `/api/mark_chat_unread`, `/api/archive_chat`, `/api/resolve_contact`, `/api/search_contacts`, `/api/react`, `/api/edit`, `/api/revoke`, `/api/group_participants`, `/api/chat_presence`, `/api/is_on_whatsapp` (+ `/qr`, `/qr.png`).

## Legenda de esforço

| Tier | Significado | Trabalho |
|------|-------------|----------|
| **A** | REST handler **já existe** na bridge; falta só wrapper Python | ~1 helper em `whatsapp.py` + `@mcp.tool` em `main.py`. **Zero Go.** |
| **B** | Método whatsmeow existe; sem REST handler | Handler Go (`main.go`) + helper + tool Python |
| **C** | Pesado/nicho; API existe mas custo alto ou pouco uso pessoal | Handler Go não-trivial + modelagem |

---

## Tabela de gaps (priorizada)

| # | Feature | whatsmeow | REST hoje | MCP hoje | Tier | Valor |
|---|---------|-----------|-----------|----------|------|-------|
| 1 | **Group info** (metadados, participantes) | `GetGroupInfo` | ✅ `/api/group_info` | ❌ | **A** | Alto |
| 2 | **Arquivar/desarquivar chat** | `SendAppState` | ✅ `/api/archive_chat` | ❌ | **A** | Médio |
| 3 | **Resolver contato** (QR/link → JID) | `ResolveContactQRLink` | ✅ `/api/resolve_contact` | ❌ | **A** | Médio |
| 4 | **Reagir a mensagem** (emoji) | `BuildReaction`+`SendMessage` | ❌ | ❌ | **B** | Alto |
| 5 | **Editar mensagem** | `BuildEdit`+`SendMessage` | ❌ | ❌ | **B** | Alto |
| 6 | **Apagar p/ todos** (revoke) | `BuildRevoke`/`RevokeMessage` | ❌ | ❌ | **B** | Alto |
| 7 | **Gerenciar participantes** (add/remove/promote/demote) | `UpdateGroupParticipants` | ✅ | ✅ | **B** | **Alto — maior gap único** |
| 8 | **Typing/presença no chat** | `SendChatPresence` | ✅ | ✅ | **B** | Alto (bot mais natural) |
| 9 | **Está no WhatsApp?** (valida número) | `IsOnWhatsApp` | ✅ | ✅ | **B** | Alto |
| 10 | **Info de usuário** (status, pic, devices) | `GetUserInfo`/`GetProfilePictureInfo` | ❌ | ❌ | **B** | Médio |
| 11 | **Enquete** (criar + ler votos) | `BuildPollCreation`/`DecryptPollVote` | ❌ | ❌ | **B** | Médio |
| 12 | **Link de convite do grupo** (get/join) | `GetGroupInviteLink`/`JoinGroupWithLink` | ❌ | ❌ | **B** | Médio |
| 13 | **Setters de grupo** (nome/tópico/foto/anúncio/locked) | `SetGroupName`/`SetGroupTopic`/`SetGroupPhoto`/`SetGroupAnnounce`/`SetGroupLocked` | ❌ | ❌ | **B** | Médio |
| 14 | **Read receipt explícito** (marcar lida via app-state ✅; receipt real ❌) | `MarkRead` | parcial | parcial | **B** | Médio |
| 15 | **Presença global** (online/offline) | `SendPresence`/`SubscribePresence` | ❌ | ❌ | **B** | Baixo |
| 16 | **Bloquear/desbloquear** | `UpdateBlocklist`/`GetBlocklist` | ❌ | ❌ | **B** | Baixo |
| 17 | **Business profile** | `GetBusinessProfile` | ❌ | ❌ | **B** | Baixo |
| 18 | **Mensagens efêmeras** (timer) | `SetDisappearingTimer` | ❌ | ❌ | **C** | Baixo |
| 19 | **Newsletters/Canais** (follow/read/react) | `FollowNewsletter`, `GetNewsletterMessages`, … | ❌ | ❌ | **C** | Baixo (nicho) |
| 20 | **Proxy / privacy settings** | `SetProxy`, `GetPrivacySettings` | ❌ | ❌ | **C** | Baixo |

> Excluído como plumbing (não é "feature"): `Connect/Disconnect/ResetConnection`, `Upload*/Download*` (uso interno), `Encrypt*/Decrypt*`, `Build*` internos, `AddEventHandler*`, `Set*HTTPClient`, `DangerousInternals`, `PairPhone`, `Logout`.

---

## Quick wins recomendados

### Fazer já — Tier A (zero Go, ~15min cada)
Handler REST pronto; padrão de wrapper idêntico a `download_media` (tool → helper `whatsapp.py` → POST bridge):

1. **`get_group_info(jid)`** → POST `/api/group_info`
2. **`archive_chat(chat_jid, archived)`** → POST `/api/archive_chat`
3. **`resolve_contact(...)`** → POST `/api/resolve_contact`

Estranho o handler existir sem tool — provavelmente meio-caminho de PRs anteriores (#4, #5). Fechar essa lacuna primeiro.

### Sprint seguinte — Tier B alto valor (Go + Python)
Ordem por valor/esforço:

1. **Reagir** (#4) — `BuildReaction` é one-liner; handler `/api/react` trivial.
2. **Apagar p/ todos** (#6) — `BuildRevoke`, mesmo padrão.
3. **Editar** (#5) — `BuildEdit`, idem.
4. **Gerenciar participantes** (#7) — `UpdateGroupParticipants(jid, []jids, action)`; **maior valor** pra grupos.
5. **Typing** (#8) + **IsOnWhatsApp** (#9) — baratos, deixam bot mais natural/robusto.

Todos #4–#6 compartilham o mesmo shape: `Build*` + `SendMessage` + handler POST curto. Dá pra fazer os três num PR só.

### Adiar — Tier C
Newsletters, proxy, disappearing timer, privacy: API existe mas pouco uso pessoal. Só sob demanda.
