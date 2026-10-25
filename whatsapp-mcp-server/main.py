import asyncio
import hmac
import os
import sys
from functools import wraps
from typing import List, Dict, Any, Optional
from mcp.server.fastmcp import FastMCP
from mcp.server.transport_security import TransportSecuritySettings
from mcp.types import ToolAnnotations
from oauth import install_oauth_routes, oauth_from_env
from whatsapp import (
    search_contacts as whatsapp_search_contacts,
    list_messages as whatsapp_list_messages,
    list_chats as whatsapp_list_chats,
    list_active_chats as whatsapp_list_active_chats,
    get_chat as whatsapp_get_chat,
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
    get_contact_chats as whatsapp_get_contact_chats,
    get_last_interaction as whatsapp_get_last_interaction,
    get_message_context as whatsapp_get_message_context,
    send_message as whatsapp_send_message,
    send_file as whatsapp_send_file,
    send_audio_message as whatsapp_audio_voice_message,
    send_voice_message as whatsapp_send_voice_message,
    download_media as whatsapp_download_media,
    create_group as whatsapp_create_group,
    leave_group as whatsapp_leave_group,
    mark_chat_read as whatsapp_mark_chat_read,
    mark_chat_unread as whatsapp_mark_chat_unread,
    get_group_info as whatsapp_get_group_info,
    archive_chat as whatsapp_archive_chat,
    resolve_contact as whatsapp_resolve_contact,
    react_to_message as whatsapp_react_to_message,
    create_poll as whatsapp_create_poll,
    send_contact as whatsapp_send_contact,
    matter_call as whatsapp_matter_call,
    get_poll_votes as whatsapp_get_poll_votes,
    vote_poll as whatsapp_vote_poll,
    edit_message as whatsapp_edit_message,
    delete_message as whatsapp_delete_message,
    update_group_participants as whatsapp_update_group_participants,
    send_chat_presence as whatsapp_send_chat_presence,
    get_group_invite_link as whatsapp_get_group_invite_link,
    join_group_with_link as whatsapp_join_group_with_link,
    update_group_settings as whatsapp_update_group_settings,
    check_whatsapp as whatsapp_check_whatsapp
)

# Initialize FastMCP server. OAuth (for Claude web connectors) is opt-in: only the HTTP transport, only when
# MCP_PUBLIC_URL and MCP_OAUTH_PASSPHRASE are set; see oauth.py.
_OAUTH = oauth_from_env() if os.environ.get("MCP_TRANSPORT", "").strip().lower() == "streamable-http" else None
# Host/Origin checks off: every HTTP request already needs a bearer or OAuth token, which a rebinding page cannot
# send, and the SDK's loopback-only default would refuse the public host behind a reverse proxy.
_NO_REBINDING_CHECK = TransportSecuritySettings(enable_dns_rebinding_protection=False)
if _OAUTH:
    mcp = FastMCP("whatsapp", auth_server_provider=_OAUTH[0], auth=_OAUTH[1], transport_security=_NO_REBINDING_CHECK)
    install_oauth_routes(mcp, _OAUTH[0])
else:
    mcp = FastMCP("whatsapp", transport_security=_NO_REBINDING_CHECK)

# MCP_READONLY=true registers only the read tools; write tools are never exposed.
_STATIC_TOKEN = os.environ.get("MCP_AUTH_TOKEN", "")  # read once, at import, like MCP_READONLY
READONLY = os.environ.get("MCP_READONLY", "").strip().lower() in ("1", "true", "yes", "on")


def _threaded_tool(fn):
    @wraps(fn)
    async def run(*args, **kwargs):
        return await asyncio.to_thread(fn, *args, **kwargs)

    return run


def read_tool(fn):
    # readOnlyHint lets Claude and ChatGPT skip the "this changes something" confirmation for pure reads.
    # structured_output=False: the return hints are loose (strings, dataclasses), so the SDK must not validate against them.
    mcp.tool(annotations=ToolAnnotations(readOnlyHint=True, destructiveHint=False, openWorldHint=False),
             structured_output=False)(_threaded_tool(fn))
    return fn


def write_tool(fn):
    # download_media counts as a write: it writes files to disk.
    if not READONLY:
        mcp.tool(structured_output=False)(_threaded_tool(fn))
    return fn


@read_tool
def search_contacts(query: str) -> List[Dict[str, Any]]:
    """Search WhatsApp contacts by name or phone number.
    
    Args:
        query: Search term to match against contact names or phone numbers
    """
    contacts = whatsapp_search_contacts(query)
    return contacts

@read_tool
def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> str:
    """Get WhatsApp messages matching specified criteria with optional context.
    
    Args:
        after: Optional ISO-8601 formatted string to only return messages after this date
        before: Optional ISO-8601 formatted string to only return messages before this date
        sender_phone_number: Optional phone number to filter messages by sender
        chat_jid: Optional chat JID to filter messages by chat
        query: Optional search term to filter messages by content
        limit: Maximum number of messages to return (default 20)
        page: Page number for pagination (default 0)
        include_context: Whether to include messages before and after matches (default True)
        context_before: Number of messages to include before each match (default 1)
        context_after: Number of messages to include after each match (default 1)
    """
    messages = whatsapp_list_messages(
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after
    )
    return messages

@read_tool
def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Dict[str, Any]]:
    """Get WhatsApp chats matching specified criteria.
    
    Args:
        query: Optional search term to filter chats by name or JID
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
        include_last_message: Whether to include the last message in each chat (default True)
        sort_by: Field to sort results by, either "last_active" or "name" (default "last_active")
    """
    chats = whatsapp_list_chats(
        query=query,
        limit=limit,
        page=page,
        include_last_message=include_last_message,
        sort_by=sort_by
    )
    return chats

@read_tool
def list_active_chats(
    after: Optional[str] = None,
    before: Optional[str] = None,
    include_groups: bool = False
) -> List[Dict[str, Any]]:
    """List the conversations that had at least one message in a period, one row per conversation,
    in chronological order of the first message. Use it to answer "who did I talk to on day X?".

    Each row has jid, name, is_group, first_time, last_time, message_count, snippet (the last
    message of the period, or [image]/[audio]/... for media) and link (opens the chat; null for
    groups and for contacts whose phone is unknown, use the jid then). Your own chat, status and
    newsletters are never listed. Show the rows as a table, never as running text.

    Args:
        after: ISO-8601 start of the period, with the user's UTC offset (e.g. 2026-05-21T00:00:00-03:00)
        before: ISO-8601 end of the period, with the user's UTC offset (e.g. 2026-05-21T23:59:59-03:00)
        include_groups: Also list groups (default False: people only)
    """
    return whatsapp_list_active_chats(after=after, before=before, include_groups=include_groups)

@read_tool
def get_chat(chat_jid: str, include_last_message: bool = True) -> Dict[str, Any]:
    """Get WhatsApp chat metadata by JID.
    
    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)
    """
    chat = whatsapp_get_chat(chat_jid, include_last_message)
    return chat

@read_tool
def get_direct_chat_by_contact(sender_phone_number: str) -> Dict[str, Any]:
    """Get WhatsApp chat metadata by sender phone number.
    
    Args:
        sender_phone_number: The phone number to search for
    """
    chat = whatsapp_get_direct_chat_by_contact(sender_phone_number)
    return chat

@read_tool
def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Dict[str, Any]]:
    """Get all WhatsApp chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    chats = whatsapp_get_contact_chats(jid, limit, page)
    return chats

@read_tool
def get_last_interaction(jid: str) -> str:
    """Get most recent WhatsApp message involving the contact.
    
    Args:
        jid: The JID of the contact to search for
    """
    message = whatsapp_get_last_interaction(jid)
    return message

@read_tool
def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> Dict[str, Any]:
    """Get context around a specific WhatsApp message.
    
    Args:
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
    """
    context = whatsapp_get_message_context(message_id, before, after)
    return context

@write_tool
def send_message(
    recipient: str,
    message: str,
    reply_to_message_id: str = "",
    mentions: Optional[List[str]] = None
) -> Dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        message: The message text to send
        reply_to_message_id: Optional ID of a text message in the same chat to quote, so it is
                 sent as a native reply
        mentions: Optional user JIDs to @-mention, as returned by get_group_info or resolve_contact.
                 Write each one in message as @ plus the part before the @ of its JID
                 (e.g. "@123456789" for "123456789@lid")
    
    Returns:
        A dictionary containing success status and a status message
    """
    # Validate input
    if not recipient:
        return {
            "success": False,
            "message": "Recipient must be provided"
        }
    
    # Call the whatsapp_send_message function with the unified recipient parameter
    success, status_message = whatsapp_send_message(recipient, message, reply_to_message_id=reply_to_message_id, mentions=mentions)
    return {
        "success": success,
        "message": status_message
    }

@write_tool
def send_file(recipient: str, media_path: str) -> Dict[str, Any]:
    """Send a file such as a picture, raw audio, video or document via WhatsApp to the specified recipient. For group messages use the JID.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the media file to send (image, video, document)
    
    Returns:
        A dictionary containing success status and a status message
    """
    
    # Call the whatsapp_send_file function
    success, status_message = whatsapp_send_file(recipient, media_path)
    return {
        "success": success,
        "message": status_message
    }

@write_tool
def send_audio_message(recipient: str, media_path: str) -> Dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified recipient. For group messages use the JID. If it errors due to ffmpeg not being installed, use send_file instead.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the audio file to send (will be converted to Opus .ogg if it's not a .ogg file)
    
    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_audio_voice_message(recipient, media_path)
    return {
        "success": success,
        "message": status_message
    }

@write_tool
def send_voice_message(recipient: str, text: str, notice: str = "") -> Dict[str, Any]:
    """Convert text to speech and send it as a WhatsApp voice message. For group messages use the JID. Requires TTS_ENGINE to be configured (see README); otherwise nothing is sent and the reason is returned. Text is limited to 4096 characters.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        text: The text to speak (max 4096 characters)
        notice: Optional text message sent right before the voice note, only after the speech was generated (nothing is sent if generation fails; if the notice cannot be sent, the voice note is not sent either)

    Returns:
        A dictionary containing success status and a status message
    """
    success, status_message = whatsapp_send_voice_message(recipient, text, notice)
    return {
        "success": success,
        "message": status_message
    }

@write_tool
def download_media(message_id: str, chat_jid: str) -> Dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.
    
    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
    
    Returns:
        A dictionary containing success status, a status message, and the file path if successful
    """
    file_path, status_message = whatsapp_download_media(message_id, chat_jid)

    if file_path:
        return {
            "success": True,
            "message": status_message,
            "file_path": file_path
        }
    else:
        return {
            "success": False,
            "message": f"Failed to download media: {status_message}"
        }

@write_tool
def create_group(
    name: str,
    participants: List[str],
    is_community: bool = False,
    community_parent_jid: str = "",
) -> Dict[str, Any]:
    """Create a new WhatsApp group.

    Args:
        name: Group subject (max 25 characters)
        participants: List of phone numbers (country code, no '+') or JIDs.
                      Your own number is added automatically.
        is_community: If True, create a community parent instead of a normal group
        community_parent_jid: If set, create as a sub-group inside this community

    Returns:
        Dict with: success (bool), message (str), and on success: jid, name, participant_count.
    """
    success, message, details = whatsapp_create_group(
        name=name,
        participants=participants,
        is_community=is_community,
        community_parent_jid=community_parent_jid,
    )
    response: Dict[str, Any] = {"success": success, "message": message}
    if success and details:
        response.update(details)
    return response


@write_tool
def leave_group(jid: str) -> Dict[str, Any]:
    """Leave a WhatsApp group. Note: WhatsApp has no 'delete group' — leaving is
    the closest action.

    Args:
        jid: The group JID (must end with @g.us)

    Returns:
        Dict with success (bool) and message (str).
    """
    success, message = whatsapp_leave_group(jid)
    return {"success": success, "message": message}


@write_tool
def mark_chat_as_read(
    chat_jid: str,
    message_ids: List[str],
    sender_jid: Optional[str] = None,
    timestamp: Optional[int] = None
) -> Dict[str, Any]:
    """Mark a WhatsApp chat as read by sending read receipts for the given messages.

    Args:
        chat_jid: The JID of the chat (e.g. 5511999999999@s.whatsapp.net or group@g.us)
        message_ids: List of message IDs to mark as read
        sender_jid: Optional sender JID (required for group messages)
        timestamp: Optional Unix timestamp in seconds; defaults to now
    """
    success, message = whatsapp_mark_chat_read(
        chat_jid,
        message_ids,
        sender_jid=sender_jid or "",
        timestamp=timestamp or 0,
    )
    return {"success": success, "message": message}


@write_tool
def mark_chat_as_unread(chat_jid: str) -> Dict[str, Any]:
    """Mark a WhatsApp chat as unread (app-state sync — affects WhatsApp app badge).

    Args:
        chat_jid: The JID of the chat (e.g. 5511999999999@s.whatsapp.net or group@g.us)
    """
    success, message = whatsapp_mark_chat_unread(chat_jid)
    return {"success": success, "message": message}


@read_tool
def get_group_info(jid: str) -> Dict[str, Any]:
    """Get a WhatsApp group's name and participant list.

    Args:
        jid: The group JID (e.g. 120363012345678901@g.us)
    """
    success, message, info = whatsapp_get_group_info(jid)
    result: Dict[str, Any] = {"success": success, "message": message}
    if info:
        result.update(info)
    return result


@write_tool
def archive_chat(chat_jid: str, archive: bool = True) -> Dict[str, Any]:
    """Archive or unarchive a WhatsApp chat (app-state sync — affects WhatsApp app).

    Args:
        chat_jid: The JID of the chat (e.g. 5511999999999@s.whatsapp.net or group@g.us)
        archive: True to archive, False to unarchive
    """
    success, message = whatsapp_archive_chat(chat_jid, archive)
    return {"success": success, "message": message}


@read_tool
def resolve_contact(phone: str) -> Dict[str, Any]:
    """Resolve a phone number to its WhatsApp JIDs (regular + LID, if any).

    Args:
        phone: Phone number to resolve (e.g. 5511999999999)
    """
    success, message, jids = whatsapp_resolve_contact(phone)
    return {"success": success, "message": message, "jids": jids}


@write_tool
def create_poll(
    chat_jid: str,
    question: str,
    options: List[str],
    selectable_count: int = 1
) -> Dict[str, Any]:
    """Send a WhatsApp poll to a chat.

    Args:
        chat_jid: The JID of the chat (e.g. 5511999999999@s.whatsapp.net or group@g.us)
        question: The poll question (max 255 characters)
        options: 2 to 12 distinct options (max 100 characters each)
        selectable_count: How many options a voter may pick (1 to len(options), default 1)
    """
    success, message = whatsapp_create_poll(chat_jid, question, options, selectable_count)
    return {"success": success, "message": message}


@write_tool
def send_contact(recipient: str, name: str, phone_number: str) -> Dict[str, Any]:
    """Send a contact as a native WhatsApp contact card (the receiver saves it with one tap).

    Args:
        recipient: Who receives the card: a phone number with country code (no + or symbols) or a JID
        name: Display name of the contact being shared
        phone_number: Phone of the contact being shared, with country code (e.g. 5562999998888)
    """
    success, message = whatsapp_send_contact(recipient, name, phone_number)
    return {"success": success, "message": message}


@read_tool
def list_matters(
    as_of: str,
    state: Optional[str] = None,
    chat_jid: Optional[str] = None,
    attention_only: bool = False
) -> Dict[str, Any]:
    """List the user's open matters: pending requests and commitments found in conversations, until
    they are closed or expire (one month). Each has state (open, closed_confirmed, closed_owner,
    expired), the proof (snippet and message id), certainty, announce_pending, mentions and its
    history. With attention_only, only the matters that need the user's attention that day, each
    with a reason (next_day, own_promise_created_today, own_promise_deadline, other_deadline).
    Show the rows as a table, never as running text.

    Args:
        as_of: ISO-8601 instant WITH the user's UTC offset (e.g. 2026-10-12T07:00:00-03:00); it fixes
            the day and the timezone the rules are read in
        state: Optional filter: open, closed (either kind) or expired
        chat_jid: Optional chat to restrict to
        attention_only: Only matters that need attention on the day of as_of
    """
    return whatsapp_matter_call("/matters/list", {
        "as_of": as_of, "state": state or "", "chat_jid": chat_jid or "", "attention_only": attention_only})


@read_tool
def review_next_messages(limit: int = 100) -> Dict[str, Any]:
    """Get the messages not reviewed yet, oldest first (one-to-one chats, plus group messages that @-mention
    the user; never the user's own chat; at most one month old), with the open matters of those chats and next_cursor. Read them,
    record or close matters (see /whats:triage), and only then call mark_reviewed with next_cursor.
    This tool never moves the cursor.

    Args:
        limit: Maximum number of messages (default 100, max 500)
    """
    return whatsapp_matter_call("/review/next", {"limit": limit})


@write_tool
def record_matter(
    chat_jid: str,
    message_id: str,
    owed_by: str,
    origin: str,
    deadline: Optional[str] = None,
    certainty: str = "clear",
    matter_id: Optional[int] = None
) -> Dict[str, Any]:
    """Record an open matter, citing the message that proves it. Recording the same message again
    returns the same matter. Pass matter_id to add a mention to an existing open matter of the same
    chat instead of creating another (one subject, one matter). A refusal comes back as {"error": ...}.

    Args:
        chat_jid: A one-to-one chat, or a group when the cited message @-mentions the user (the user's own chat is refused)
        message_id: The message that proves it (must exist locally and be at most one month old)
        owed_by: Who has to act: owner (the user) or other (the other person)
        origin: request (asked something of the user), demand (chased the user), promise (someone
            promised to do something) or event (something agreed to happen)
        deadline: Optional date YYYY-MM-DD (no time); for an event, the date it happens
        certainty: clear, or uncertain when unsure it is a request/promise (it is still listed, marked)
        matter_id: Optional existing open matter of this chat to add the mention to
    """
    return whatsapp_matter_call("/matters/record", {
        "chat_jid": chat_jid, "message_id": message_id, "owed_by": owed_by, "origin": origin,
        "deadline": deadline, "certainty": certainty, "matter_id": matter_id})


@write_tool
def close_matter(matter_id: int, by: str, message_id: Optional[str] = None) -> Dict[str, Any]:
    """Close an open matter. by=confirmed needs message_id of a LATER message FROM THE OTHER PERSON
    that confirms it was done (the user's own replies, "ok" or silence never close anything); the
    user is told about it in the next summary. by=owner is for when the user says it is resolved.

    Args:
        matter_id: The matter to close
        by: confirmed or owner
        message_id: For confirmed: the other person's confirming message in that chat
    """
    return whatsapp_matter_call("/matters/close", {"matter_id": matter_id, "by": by, "message_id": message_id or ""})


@write_tool
def reopen_matter(matter_id: int, message_id: str) -> Dict[str, Any]:
    """Reopen a closed or expired matter because the conversation returned to it: same matter,
    history kept, one more month from now.

    Args:
        matter_id: The matter to reopen
        message_id: A message of that chat newer than everything the matter already knows
    """
    return whatsapp_matter_call("/matters/reopen", {"matter_id": matter_id, "message_id": message_id})


@write_tool
def mark_reviewed(up_to: str) -> Dict[str, Any]:
    """Advance the review cursor: everything up to that instant has been reviewed. It never goes
    back. Use the next_cursor of review_next_messages after handling those messages.

    Args:
        up_to: ISO-8601 instant with offset (the next_cursor)
    """
    return whatsapp_matter_call("/review/mark", {"up_to": up_to})


@read_tool
def suggest_favorites(limit: int = 10) -> Dict[str, Any]:
    """Suggest favorites from the last month of conversations: one-to-one chats with real two-way
    traffic (at least 3 messages each way), most active first, with how many came from each side.
    Never suggests groups, the user's own chat, current favorites or contacts the user already
    rejected. This only proposes: nothing becomes a favorite until the user decides each name with
    decide_favorite. Show the rows as a table.

    Args:
        limit: Maximum number of suggestions (default 10)
    """
    return whatsapp_matter_call("/favorites/suggest", {"limit": limit})


@read_tool
def list_favorites() -> Dict[str, Any]:
    """List the user's favorites: the WhatsApp Favorites list (source whatsapp) plus the
    suggestions the user accepted (source owner). An empty list is normal."""
    return whatsapp_matter_call("/favorites/list", {})


@write_tool
def decide_favorite(chat_jid: str, decision: str) -> Dict[str, Any]:
    """Record the user's decision about one contact. Only call it with a decision the user gave
    for that contact; never decide for them.

    Args:
        chat_jid: The one-to-one chat of the contact
        decision: accept (becomes a favorite), reject (never suggested again) or remove (forget the decision)
    """
    return whatsapp_matter_call("/favorites/decide", {"chat_jid": chat_jid, "decision": decision})


@write_tool
def morning_summary(as_of: str, all: bool = False) -> Dict[str, Any]:
    """Get the user's summary for a day: only what needs their action, favorites first, then who
    chased them or what they promised, then the rest, most recent first. The first call of a day
    builds and stores it (a photograph that never changes that day) and marks the matter closures
    it reports as told; later calls return the same summary. At most 10 items come back with
    `more` counting the rest, which are never dropped. Each item has name, time, snippet, link
    (null when there is none), reason, certainty (mark uncertain ones), owed_by, origin, deadline
    and group. `announcements` are matters closed by the other person's confirmation that the user
    has not been told about: show them as "closed because X confirmed", with a way to reopen
    (reopen_matter). Show a table, never running text. Never sends anything.

    Args:
        as_of: ISO-8601 instant WITH the user's UTC offset, e.g. 2026-10-12T07:00:00-03:00 (fixes the day)
        all: Return every item of the day's summary instead of the first 10
    """
    return whatsapp_matter_call("/summary", {"as_of": as_of, "all": all})


@write_tool
def create_chase_draft(matter_id: int, text: str) -> Dict[str, Any]:
    """Write a draft message chasing someone about a matter THEY owe (owed_by other, still open).
    This only saves the draft: nothing is sent. A pending draft of the same matter is rewritten.
    The user sees it in their summary and decides.

    Args:
        matter_id: The open matter the other person owes
        text: The message, short and friendly, in the language and tone of that chat (max 1000 characters)
    """
    return whatsapp_matter_call("/drafts/create", {"matter_id": matter_id, "text": text})


@read_tool
def list_chase_drafts(state: Optional[str] = None) -> Dict[str, Any]:
    """List chase drafts. state: written (pending, waiting for the user), sent or discarded; empty for all."""
    return whatsapp_matter_call("/drafts/list", {"state": state or ""})


@write_tool
def approve_chase_draft(draft_id: int, text: str) -> Dict[str, Any]:
    """SEND a chase message to the other person, in the user's name. Call this ONLY after the user
    has explicitly told you, in this conversation, to send THAT draft (as written or edited), and
    pass exactly the text they approved: that exact text is what is sent. Never call it on your own
    initiative, never for several drafts on one vague "ok", and never for a draft the user did not
    see. If the send fails the draft stays pending and the error comes back.

    Args:
        draft_id: The draft the user approved
        text: Exactly the text the user approved (the draft's text, or their edited version)
    """
    return whatsapp_matter_call("/drafts/approve", {"draft_id": draft_id, "text": text})


@write_tool
def discard_chase_draft(draft_id: int) -> Dict[str, Any]:
    """Discard a pending chase draft because the user does not want to send it. It is never sent later."""
    return whatsapp_matter_call("/drafts/discard", {"draft_id": draft_id})


@write_tool
def vote_in_poll(chat_jid: str, message_id: str, options: List[str]) -> Dict[str, Any]:
    """Cast the user's own vote in a WhatsApp poll (or retract it by passing no options). Call it only for
    a vote the user asked for. The latest vote replaces the previous one.

    Args:
        chat_jid: The chat the poll is in
        message_id: The poll's message id (create_poll returns it; list_messages shows it)
        options: The option names to vote for, exactly as in the poll (up to its limit); empty to retract
    """
    success, message = whatsapp_vote_poll(chat_jid, message_id, options)
    return {"success": success, "message": message}


@read_tool
def get_poll_votes(chat_jid: str, message_id: str) -> Dict[str, Any]:
    """Read the recorded votes of a poll.

    Only polls seen live by the bridge (sent through create_poll or received
    while it was running) are tracked; polls from history sync are not.

    Args:
        chat_jid: The JID of the chat containing the poll
        message_id: The ID of the poll message
    """
    success, message, result = whatsapp_get_poll_votes(chat_jid, message_id)
    if not success:
        return {"success": False, "message": message}
    return result


@write_tool
def react_to_message(
    chat_jid: str,
    message_id: str,
    emoji: str,
    from_me: bool = True
) -> Dict[str, Any]:
    """React to a WhatsApp message with an emoji.

    Passing an empty emoji string removes the existing reaction.

    Args:
        chat_jid: The JID of the chat (e.g. 5511999999999@s.whatsapp.net or group@g.us)
        message_id: The ID of the message to react to
        emoji: The emoji to react with, or "" to remove the reaction
        from_me: Whether the target message was sent by you (defaults True, your own
            message). Setting from_me=False works only in direct chats; in group
            chats it returns an error because the original sender's JID isn't available.
    """
    success, message = whatsapp_react_to_message(chat_jid, message_id, emoji, from_me=from_me)
    return {"success": success, "message": message}


@write_tool
def edit_message(
    chat_jid: str,
    message_id: str,
    new_text: str,
    from_me: bool = True
) -> Dict[str, Any]:
    """Edit the text of a previously sent WhatsApp message.

    Args:
        chat_jid: The JID of the chat (e.g. 5511999999999@s.whatsapp.net or group@g.us)
        message_id: The ID of the message to edit
        new_text: The new message text
        from_me: Accepted for API symmetry with react_to_message/delete_message but
            ignored — WhatsApp only allows editing your own messages, and the bridge
            never reads this param for /api/edit.
    """
    success, message = whatsapp_edit_message(chat_jid, message_id, new_text, from_me=from_me)
    return {"success": success, "message": message}


@write_tool
def delete_message(
    chat_jid: str,
    message_id: str,
    from_me: bool = True
) -> Dict[str, Any]:
    """Delete a WhatsApp message for everyone (revoke).

    Args:
        chat_jid: The JID of the chat (e.g. 5511999999999@s.whatsapp.net or group@g.us)
        message_id: The ID of the message to delete
        from_me: Whether the target message was sent by you (defaults True, your own
            message). Setting from_me=False works only in direct chats; in group
            chats it returns an error because the original sender's JID isn't available.
    """
    success, message = whatsapp_delete_message(chat_jid, message_id, from_me=from_me)
    return {"success": success, "message": message}


@write_tool
def update_group_participants(
    group_jid: str,
    participants: List[str],
    action: str
) -> Dict[str, Any]:
    """Update WhatsApp group participants (add/remove/promote/demote).

    Args:
        group_jid: The JID of the group (must end with @g.us)
        participants: List of phone numbers or JIDs to modify. International format recommended
            (e.g., 5562123456789 or 5562123456789@s.whatsapp.net)
        action: The action to perform: "add" (invite), "remove" (remove from group),
            "promote" (make admin), or "demote" (remove admin). Requires you to be a
            group admin for most actions.

    Returns:
        {
            "success": bool (call accepted by WhatsApp, not all participants applied),
            "message": str (summary),
            "participants": [ {"jid": str, "is_admin": bool, "error": int, "add_request"?: bool}, ... ]
        }

    Note: Each participant in the response has error code 0 if applied, non-0 if rejected
    (e.g. 403 privacy, 409 already member). add_request field is present when action="add"
    and the target has privacy settings blocking group invites — the invitation becomes pending.
    """
    success, message, participants = whatsapp_update_group_participants(
        group_jid, participants, action
    )
    return {
        "success": success,
        "message": message,
        "participants": participants
    }


@write_tool
def get_group_invite_link(group_jid: str, reset: bool = False) -> Dict[str, Any]:
    """Get a WhatsApp group's invite link (you must be a group admin).

    Args:
        group_jid: The group JID (must end with @g.us)
        reset: If true, revoke the current link and generate a new one
            (anyone holding the old link can no longer join).

    The link is a secret: anyone with it can join the group. Share it deliberately.
    """
    success, message, link = whatsapp_get_group_invite_link(group_jid, reset)
    result: Dict[str, Any] = {"success": success, "message": message}
    if link:
        result["link"] = link
    return result


@write_tool
def join_group_with_link(link: str) -> Dict[str, Any]:
    """Join a WhatsApp group using an invite link or bare invite code.

    Args:
        link: https://chat.whatsapp.com/<code> or just the code

    Returns the joined group's JID on success.
    """
    success, message, jid = whatsapp_join_group_with_link(link)
    result: Dict[str, Any] = {"success": success, "message": message}
    if jid:
        result["jid"] = jid
    return result


@write_tool
def update_group_settings(
    group_jid: str,
    name: Optional[str] = None,
    topic: Optional[str] = None,
    announce: Optional[bool] = None,
    locked: Optional[bool] = None,
) -> Dict[str, Any]:
    """Update a WhatsApp group's settings. Only the fields you pass are changed (requires group admin).

    Args:
        group_jid: The group JID (must end with @g.us)
        name: New group name (1-25 characters)
        topic: New group description (up to 2048 characters; empty string clears it)
        announce: true = only admins can send messages
        locked: true = only admins can edit group info
    """
    success, message = whatsapp_update_group_settings(group_jid, name, topic, announce, locked)
    return {"success": success, "message": message}


@write_tool
def send_chat_presence(
    chat_jid: str,
    state: str,
    media: str = ""
) -> Dict[str, Any]:
    """Send typing or recording indicator in a WhatsApp chat (ephemeral, no persistence).

    Args:
        chat_jid: The JID of the chat (direct or group)
        state: "composing" (typing indicator) or "paused" (stopped typing)
        media: "" (text, default) or "audio" (recording indicator)

    Guidance:
        - Send "composing" before replying to show the recipient you're typing.
        - Always send "paused" after you stop (or wait ~10 seconds for WhatsApp to auto-expire).
        - If you send "composing" for "audio", the recipient sees a recording indicator.
        - The indicator is ephemeral and not stored in the chat history.
    """
    success, message = whatsapp_send_chat_presence(chat_jid, state, media)
    return {"success": success, "message": message}


@read_tool
def check_whatsapp(phones: List[str]) -> Dict[str, Any]:
    """Check if phone numbers are registered on WhatsApp.

    Args:
        phones: List of phone numbers to check (international format, with or without +).
            Digits only after normalization (8-15 digits); max 50 numbers per call.
            Small batches recommended to avoid rate-limiting.

    Returns:
        {
            "success": bool,
            "message": str (summary),
            "results": [
                {
                    "query": str (normalized input),
                    "jid": str (WhatsApp JID if found, empty otherwise),
                    "is_in": bool (true if registered on WhatsApp),
                    "verified_name"?: str (business name if business account)
                },
                ...
            ]
        }

    Guidance:
        - Always validate new recipient numbers before calling send_message.
        - Numbers not on WhatsApp return is_in=false (no error).
        - Business accounts include a verified_name field (company or brand name).
        - Use small batches and avoid mass scanning to prevent rate-limiting or account restrictions.
    """
    success, message, results = whatsapp_check_whatsapp(phones)
    return {
        "success": success,
        "message": message,
        "results": results
    }



def check_http_config(host: str, token: str) -> None:
    """The HTTP transport never starts without a bearer token, loopback or not."""
    if not token:
        raise SystemExit(
            f"MCP_AUTH_TOKEN is required for MCP_TRANSPORT=streamable-http (MCP_HOST={host})"
        )


class BearerAuth:
    """ASGI middleware: 401 unless `Authorization: Bearer <token>` matches (constant time)."""

    def __init__(self, app, token: str):
        self.app = app
        self.token = token.encode()

    async def __call__(self, scope, receive, send):
        if scope["type"] == "lifespan":
            return await self.app(scope, receive, send)
        if scope["type"] != "http":
            # MCP over HTTP uses plain http only. Refuse anything else (websocket, ...) instead of passing it
            # through unauthenticated; a websocket gets a policy-violation close.
            if scope["type"] == "websocket":
                await send({"type": "websocket.close", "code": 1008})
            return
        auth = dict(scope["headers"]).get(b"authorization", b"")
        scheme, _, given = auth.partition(b" ")
        if scheme.lower() == b"bearer" and hmac.compare_digest(given.strip(), self.token):
            return await self.app(scope, receive, send)
        await send({"type": "http.response.start", "status": 401,
                    "headers": [(b"content-type", b"text/plain"), (b"www-authenticate", b"Bearer")]})
        await send({"type": "http.response.body", "body": b"unauthorized"})


class McpPathFix:
    """ASGI middleware: serve `/mcp` and `/mcp/` alike. The router redirects one to the other with a 307, and
    connector validators (Claude, ChatGPT) POST to the URL as typed and do not follow it."""

    def __init__(self, app, challenge: bytes = b"Bearer"):
        self.app = app
        self.challenge = challenge

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http" or scope["path"] not in ("/mcp", "/mcp/"):
            return await self.app(scope, receive, send)
        path = mcp.settings.streamable_http_path
        scope = dict(scope, path=path, raw_path=path.encode())

        async def with_challenge(message):
            # RFC 9728: a 401 must say where to find the authorization server (the SDK's answer has no header).
            if message["type"] == "http.response.start" and message["status"] == 401:
                headers = [(k, v) for k, v in message["headers"] if k.lower() != b"www-authenticate"]
                message = dict(message, headers=headers + [(b"www-authenticate", self.challenge)])
            await send(message)

        await self.app(scope, receive, with_challenge)


def build_http_app():
    """The ASGI app the HTTP transport serves. With OAuth the provider accepts the static token too; otherwise the
    plain bearer wrapper guards everything."""
    app = mcp.streamable_http_app()
    if not _OAUTH:
        return McpPathFix(BearerAuth(app, _STATIC_TOKEN))
    prm = f'{_OAUTH[0].public_url}/.well-known/oauth-protected-resource/mcp'
    return McpPathFix(app, f'Bearer resource_metadata="{prm}"'.encode())


def main() -> None:
    transport = os.environ.get("MCP_TRANSPORT", "stdio").strip().lower()
    if transport == "stdio":
        mcp.run(transport="stdio")
    elif transport == "streamable-http":
        import uvicorn

        host = os.environ.get("MCP_HOST", "127.0.0.1")
        port = int(os.environ.get("MCP_PORT", "8000"))
        check_http_config(host, _STATIC_TOKEN)
        uvicorn.run(build_http_app(), host=host, port=port)
    else:
        sys.exit(f"unsupported MCP_TRANSPORT={transport!r} (use stdio or streamable-http)")


if __name__ == "__main__":
    main()
