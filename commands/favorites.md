---
description: Suggest your favorite contacts from recent conversations and record your decision for each one
---

Help the user build the list of favorites that comes first in their summaries. A favorite is the WhatsApp Favorites list; if it is empty (or incomplete), suggest names from how the user actually talks. You only propose: every name becomes a favorite by the user's own decision. Nothing leaves this machine and no message is sent.

## 1. Look at what exists

Call `list_favorites` (an empty list is normal) and show it.

## 2. Suggest

Call `suggest_favorites`. It returns one-to-one chats of the last month with real two-way traffic (someone who only writes to the user, like a shop or a promotion, is never proposed), with how many messages came from each side. Show one table: name, messages from them, messages from the user.

## 3. Decide, one name at a time

Ask the user about each suggested name: keep as favorite, not a favorite, or skip for now. Record only what they answer with `decide_favorite`:

- `accept`: becomes a favorite.
- `reject`: never suggested again.
- `remove`: forget an earlier decision.

Do not decide for the user, not even for names that look obvious. A name they skip is not recorded.

## 4. Finish

Call `list_favorites` and show the result. Tell them they can also mark favorites in WhatsApp itself: that list is read by the bridge when WhatsApp syncs it.
