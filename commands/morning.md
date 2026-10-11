---
description: Review new WhatsApp messages and show the morning summary: only what needs your action today
---

Run the morning routine for the user. Nothing leaves this machine and no message is sent.

## 1. Review what is new

Do what `/whats:triage` does: `review_next_messages`, record or close matters following its rules, then `mark_reviewed` with `next_cursor`. Skip this step if there is nothing new.

## 2. Write chase drafts

Call `list_matters` with `attention_only: true` (same `as_of` as below). For each matter that the other person owes (`owed_by: "other"`) and that has no pending draft in `list_chase_drafts` (state `written`), write a short, friendly message in the language and tone of that chat that asks about the snippet, and save it with `create_chase_draft`. This only saves a draft: nothing is sent.

## 3. Get the summary

Call `morning_summary` with `as_of` set to the current time **with the user's UTC offset** (for example `2026-10-12T07:00:00-03:00`). The first call of the day builds the summary and stores it; calling again returns the same one.

## 4. Show it

One table, no running text, only what needs the user's action: name, time, snippet, link (when there is one), why it is there, and mark the `uncertain` ones. Keep the order you received (favorites, then who chased them or what they promised, then the rest).

- If `more` is above zero, end with "N more": offer to show them and, if the user agrees, call `morning_summary` again with `all: true`.
- Under the table, list the `announcements` as "closed because the other person confirmed": name, snippet, and offer `reopen_matter` if the user disagrees.
- If nothing needs action, say so in one line.

## 5. Drafts waiting for the user

Show the `drafts` of the summary in a second table: draft id, name, the snippet it refers to and the text. For each one, ask the user: send as is, edit it, or discard it.

- Call `approve_chase_draft` **only** after the user explicitly said to send **that** draft, passing **exactly** the text they approved (their edit, if they edited). That is the only way anything is sent.
- If they discard it, call `discard_chase_draft`. If they say nothing, leave it: it shows up again tomorrow.
- Never approve on your own, not even a draft that looks obvious, and never on a vague "ok" for several drafts at once.

## 6. Scheduling

If the user has no daily run yet, explain how to make it run every morning (for example `/schedule` or `/loop 24h /whats:morning`) and let them decide. Do not create a schedule yourself.
