---
description: Review new WhatsApp messages and keep the list of open matters (pending requests and commitments) up to date
---

Review the messages that were not reviewed yet and keep the open matters up to date. You recognize; the bridge stores and applies the fixed rules (one-month validity, when something needs attention). Nothing leaves this machine through this command: use only the tools below, never send a message.

An **open matter** is something agreed or asked in a one-to-one conversation that still has to happen. The owner is the user (messages with `is_from_me` true).

## 1. Fetch

Call `review_next_messages` (limit 100). It returns the messages, the open matters of those chats and `next_cursor`. Nothing moves until you call `mark_reviewed`.

## 2. Decide, message by message

Register a matter with `record_matter` (always citing that message) when it is:

- **request**: someone asks the owner for something ("me manda o contrato").
- **demand**: someone chases the owner about something owed ("e o orçamento?").
- **promise**: someone, owner included, commits to do something, with or without a date ("te mando até sexta", "vou ver e te falo").
- **event**: something agreed to happen ("o montador vem terça", "semana que vem a gente almoça").

Set `owed_by` to who has to act: `owner` or `other`.

Messages from groups only reach you when they @-mention the user, and they are addressed to the user like any other request. In a group, a matter is closed by a later message from someone else in the group confirming it.

Do **not** register: plain information ("o orçamento saiu hoje"), a conditional promise ("qualquer coisa eu te aviso"), pleasantries, promotions and automatic messages, unless they contain a concrete request to the owner.

- **Deadline**: turn "até sexta" into a date (`YYYY-MM-DD`, relative to the message's own date); no time. For an event, the date it happens. No date mentioned: leave it empty.
- **Same subject**: look at the `open_matters` of that chat first. If the message is about one of them, pass its `matter_id` (a mention) instead of creating another.
- **Unsure**: register it with `certainty: "uncertain"`. The owner prefers being reminded of too much over missing something.

## 3. Closing and reopening

- Close with `close_matter` `by: "confirmed"` only when a **later message from the other person** says it was done ("deu certo", "recebi, obrigado"), citing that message.
- **Never close** because of silence, because the owner replied, or because of a courtesy reply ("ok", "blz", "vou ver", "já to indo"): those clear the unread dot but the matter is still open. `by: "owner"` is only for when the owner tells you it is resolved.
- If the conversation returns to a closed or expired matter ("e aquele contrato?"), call `reopen_matter` with that message.
- If you are not sure it was confirmed, leave it open.

## 4. Finish

After handling the messages, call `mark_reviewed` with `next_cursor`. If a call failed midway, mark only up to the last message you handled. If `next_cursor` is empty there was nothing new.

## 5. Report

One table, no running text: new matters (who owes, origin, snippet, deadline, uncertain or not), matters closed by confirmation (the owner is told about these), reopened ones, and a count of messages reviewed. The owner reads this in the morning: only what needs their action, with the chat as the link.
