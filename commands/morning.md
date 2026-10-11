---
description: Review new WhatsApp messages and show the morning summary: only what needs your action today
---

Run the morning routine for the user. Nothing leaves this machine and no message is sent.

## 1. Review what is new

Do what `/whats:triage` does: `review_next_messages`, record or close matters following its rules, then `mark_reviewed` with `next_cursor`. Skip this step if there is nothing new.

## 2. Get the summary

Call `morning_summary` with `as_of` set to the current time **with the user's UTC offset** (for example `2026-10-12T07:00:00-03:00`). The first call of the day builds the summary and stores it; calling again returns the same one.

## 3. Show it

One table, no running text, only what needs the user's action: name, time, snippet, link (when there is one), why it is there, and mark the `uncertain` ones. Keep the order you received (favorites, then who chased them or what they promised, then the rest).

- If `more` is above zero, end with "N more": offer to show them and, if the user agrees, call `morning_summary` again with `all: true`.
- Under the table, list the `announcements` as "closed because the other person confirmed": name, snippet, and offer `reopen_matter` if the user disagrees.
- If nothing needs action, say so in one line.

## 4. Scheduling

If the user has no daily run yet, explain how to make it run every morning (for example `/schedule` or `/loop 24h /whats:morning`) and let them decide. Do not create a schedule yourself.
