import logging
import unicodedata
from datetime import datetime
from dataclasses import dataclass
from typing import Optional, List, Tuple, Dict, Any
import os
import os.path
import requests
import json
import audio

# This server runs over MCP's stdio transport, where stdout carries JSON-RPC
# framing — a stray print() either corrupts that stream or is silently
# discarded, so it's not a usable diagnostic channel. logging defaults to
# stderr, which stdio-transport MCP servers can safely use for diagnostics.
logger = logging.getLogger(__name__)

WHATSAPP_API_BASE_URL = os.environ.get("WHATSAPP_API_BASE_URL", "http://localhost:8080/api")
WHATSAPP_API_AUTH_TOKEN = os.environ.get("WHATSAPP_API_AUTH_TOKEN", "")

REQUEST_TIMEOUT = 30

# Cache of sender_jid -> resolved name, avoids one HTTP round trip per message
# when formatting a batch of messages (format_message is called per-message).
_sender_name_cache: Dict[str, str] = {}


def _auth_headers() -> Dict[str, str]:
    if WHATSAPP_API_AUTH_TOKEN:
        return {"Authorization": f"Bearer {WHATSAPP_API_AUTH_TOKEN}"}
    return {}


def _api_request(method: str, path: str, timeout: int = REQUEST_TIMEOUT, **kwargs) -> requests.Response:
    """GET/POST to the bridge with auth header + timeout, raising requests.RequestException
    on 401 with an actionable message. Used by the Tuple[bool, str, ...]-returning action
    functions below, which need the raw Response (they parse .json() and build their own
    return tuple) rather than _api_post's Optional[dict] shape."""
    url = f"{WHATSAPP_API_BASE_URL}{path}"
    response = requests.request(method, url, headers=_auth_headers(), timeout=timeout, **kwargs)
    if response.status_code == 401:
        raise requests.RequestException(
            f"HTTP 401 - check WHATSAPP_API_AUTH_TOKEN matches the bridge's API_AUTH_TOKEN ({response.text})"
        )
    return response


def _api_post(path: str, payload: dict, timeout: int = REQUEST_TIMEOUT) -> Optional[dict]:
    """POST to the bridge REST API. Returns the parsed JSON dict on HTTP 200,
    or None on any failure (non-200 status, timeout, connection error, bad JSON).
    Never raises — mirrors the old `except sqlite3.Error` behavior."""
    try:
        url = f"{WHATSAPP_API_BASE_URL}{path}"
        response = requests.post(url, json=payload, headers=_auth_headers(), timeout=timeout)
        if response.status_code == 401:
            logger.warning(
                "API auth error on %s: HTTP 401 - check WHATSAPP_API_AUTH_TOKEN "
                "matches the bridge's API_AUTH_TOKEN (%s)",
                path, response.text,
            )
            return None
        if response.status_code != 200:
            logger.warning("API error on %s: HTTP %s - %s", path, response.status_code, response.text)
            return None
        return response.json()
    except requests.RequestException as e:
        logger.warning("Request error on %s: %s", path, e)
        return None
    except json.JSONDecodeError:
        logger.warning("Error parsing response as JSON from %s", path)
        return None
    except Exception as e:
        logger.warning("Unexpected error on %s: %s", path, e)
        return None


def _strip_accents(text: Optional[str]) -> Optional[str]:
    """Lowercase and remove diacritics so searches match regardless of accents."""
    if text is None:
        return None
    decomposed = unicodedata.normalize('NFD', text)
    return ''.join(c for c in decomposed if unicodedata.category(c) != 'Mn').lower()


def _normalize_phone(phone: str) -> str:
    return phone.replace('+', '').replace(' ', '').replace('-', '')


def _parse_ts(value: Optional[str]) -> Optional[datetime]:
    """Parse an RFC3339/ISO-8601 timestamp coming from the bridge JSON.
    Returns None for falsy input or unparseable strings (never raises)."""
    if not value:
        return None
    try:
        return datetime.fromisoformat(value.replace('Z', '+00:00'))
    except (ValueError, TypeError):
        return None


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: Optional[str] = None
    media_type: Optional[str] = None

@dataclass
class Chat:
    jid: str
    name: Optional[str]
    last_message_time: Optional[datetime]
    last_message: Optional[str] = None
    last_sender: Optional[str] = None
    last_is_from_me: Optional[bool] = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")

@dataclass
class Contact:
    phone_number: str
    name: Optional[str]
    jid: str

@dataclass
class MessageContext:
    message: Message
    before: List[Message]
    after: List[Message]


def _message_from_dict(d: dict) -> Message:
    return Message(
        timestamp=_parse_ts(d.get("timestamp")),
        sender=d.get("sender"),
        chat_name=d.get("chat_name"),
        content=d.get("content"),
        is_from_me=d.get("is_from_me", False),
        chat_jid=d.get("chat_jid"),
        id=d.get("id"),
        media_type=d.get("media_type"),
    )


def _chat_from_dict(d: dict) -> Chat:
    return Chat(
        jid=d.get("jid"),
        name=d.get("name"),
        last_message_time=_parse_ts(d.get("last_message_time")),
        last_message=d.get("last_message"),
        last_sender=d.get("last_sender"),
        last_is_from_me=d.get("last_is_from_me"),
    )


def get_sender_name(sender_jid: str) -> str:
    if sender_jid in _sender_name_cache:
        return _sender_name_cache[sender_jid]

    result = _api_post("/sender_name", {"sender_jid": sender_jid})
    if result is None:
        # Transport failure: don't cache, so a later retry can still succeed.
        return sender_jid

    name = result.get("name", sender_jid)
    _sender_name_cache[sender_jid] = name
    return name

def format_message(message: Message, show_chat_info: bool = True) -> None:
    """Print a single message with consistent formatting."""
    output = ""
    ts_str = f"{message.timestamp:%Y-%m-%d %H:%M:%S}" if message.timestamp else "unknown time"

    if show_chat_info and message.chat_name:
        output += f"[{ts_str}] Chat: {message.chat_name} "
    else:
        output += f"[{ts_str}] "

    content_prefix = ""
    if hasattr(message, 'media_type') and message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "

    try:
        sender_name = get_sender_name(message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        logger.warning("Error formatting message %s: %s", message.id, e)
    return output

def format_messages_list(messages: List[Message], show_chat_info: bool = True) -> None:
    output = ""
    if not messages:
        output += "No messages to display."
        return output

    for message in messages:
        output += format_message(message, show_chat_info)
    return output

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
) -> List[Message]:
    """Get messages matching the specified criteria with optional context."""
    # Validate date filters up front (same contract as before: invalid dates raise
    # a ValueError that propagates to the caller, it is NOT swallowed like transport errors).
    if after:
        try:
            datetime.fromisoformat(after)
        except ValueError:
            raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")

    if before:
        try:
            datetime.fromisoformat(before)
        except ValueError:
            raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")

    payload = {
        "after": after,
        "before": before,
        "sender_phone_number": _normalize_phone(sender_phone_number) if sender_phone_number else None,
        "chat_jid": chat_jid,
        "query": query,
        "limit": limit,
        "page": page,
    }

    result = _api_post("/messages", payload)
    if result is None:
        return []

    messages = [_message_from_dict(m) for m in result.get("messages", [])]

    if include_context and messages:
        messages_with_context = []
        context_failures = []
        for msg in messages:
            try:
                context = get_message_context(msg.id, context_before, context_after)
                messages_with_context.extend(context.before)
                messages_with_context.append(context.message)
                messages_with_context.extend(context.after)
            except Exception as e:
                # A single failed context lookup shouldn't sink the whole batch -
                # fall back to the bare message. But unlike a genuine "not found"
                # (which get_message_context itself distinguishes), any failure here
                # silently degrades results the caller can't otherwise detect, so
                # surface it in the response text instead of only a stdout print
                # (this server runs over stdio, where stdout is the JSON-RPC channel,
                # not a readable log).
                context_failures.append(f"{msg.id} ({e})")
                messages_with_context.append(msg)

        output = format_messages_list(messages_with_context, show_chat_info=True)
        if context_failures:
            output += (
                f"\n[Warning: context lookup failed for {len(context_failures)} message(s), "
                f"showing them without surrounding context: {'; '.join(context_failures)}]"
            )
        return output

    return format_messages_list(messages, show_chat_info=True)


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> MessageContext:
    """Get context around a specific message."""
    result = _api_post("/message_context", {
        "message_id": message_id,
        "before": before,
        "after": after,
    })

    if result is None:
        # Transport/server failure - mirror old `except ... raise` behavior for
        # the DB-error path by not silently fabricating a context.
        raise ValueError(f"Could not fetch context for message ID {message_id}")

    msg_data = result.get("message")
    if not msg_data:
        raise ValueError(f"Message with ID {message_id} not found")

    target_message = _message_from_dict(msg_data)
    before_messages = [_message_from_dict(m) for m in result.get("before", [])]
    after_messages = [_message_from_dict(m) for m in result.get("after", [])]

    return MessageContext(
        message=target_message,
        before=before_messages,
        after=after_messages
    )


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get chats matching the specified criteria."""
    result = _api_post("/chats", {
        "query": query,
        "limit": limit,
        "page": page,
        "include_last_message": include_last_message,
        "sort_by": sort_by,
    })

    if result is None:
        return []

    return [_chat_from_dict(c) for c in result.get("chats", [])]


def search_contacts(query: str) -> List[Contact]:
    """Search contacts by name or phone number."""
    result = _api_post("/contacts/search", {"query": query})

    if result is None:
        return []

    return [
        Contact(
            phone_number=c.get("phone_number"),
            name=c.get("name"),
            jid=c.get("jid"),
        )
        for c in result.get("contacts", [])
    ]


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact.

    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    result = _api_post("/contacts/chats", {
        "jid": jid,
        "limit": limit,
        "page": page,
    })

    if result is None:
        return []

    return [_chat_from_dict(c) for c in result.get("chats", [])]


def get_last_interaction(jid: str) -> str:
    """Get most recent message involving the contact."""
    result = _api_post("/contacts/last_interaction", {"jid": jid})

    if result is None:
        return None

    # interfaces.md documents this endpoint two ways (`formatted` string vs raw
    # `message` object); accept either so a mismatch with the bridge implementation
    # doesn't silently break this call.
    if "formatted" in result and result.get("formatted") is not None:
        return result["formatted"]

    msg_data = result.get("message")
    if not msg_data:
        return None

    message = _message_from_dict(msg_data)
    return format_message(message)


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    result = _api_post("/chat", {
        "chat_jid": chat_jid,
        "include_last_message": include_last_message,
    })

    if result is None:
        return None

    chat_data = result.get("chat")
    if not chat_data:
        return None

    return _chat_from_dict(chat_data)


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get chat metadata by sender phone number (handles LID contacts)."""
    result = _api_post("/chat/by_contact", {"sender_phone_number": _normalize_phone(sender_phone_number)})

    if result is None:
        return None

    chat_data = result.get("chat")
    if not chat_data:
        return None

    return _chat_from_dict(chat_data)

def send_message(recipient: str, message: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "message": message,
        }

        response = requests.post(url, json=payload, headers=_auth_headers())

        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_file(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        if not media_path:
            return False, "Media path must be provided"

        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path
        }

        response = requests.post(url, json=payload, headers=_auth_headers())

        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def send_audio_message(recipient: str, media_path: str) -> Tuple[bool, str]:
    try:
        # Validate input
        if not recipient:
            return False, "Recipient must be provided"

        if not media_path:
            return False, "Media path must be provided"

        if not os.path.isfile(media_path):
            return False, f"Media file not found: {media_path}"

        if not media_path.endswith(".ogg"):
            try:
                media_path = audio.convert_to_opus_ogg_temp(media_path)
            except Exception as e:
                return False, f"Error converting file to opus ogg. You likely need to install ffmpeg: {str(e)}"

        url = f"{WHATSAPP_API_BASE_URL}/send"
        payload = {
            "recipient": recipient,
            "media_path": media_path
        }

        response = requests.post(url, json=payload, headers=_auth_headers())

        # Check if the request was successful
        if response.status_code == 200:
            result = response.json()
            return result.get("success", False), result.get("message", "Unknown response")
        else:
            return False, f"Error: HTTP {response.status_code} - {response.text}"

    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        return False, f"Error parsing response: {response.text}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"

def download_media(message_id: str, chat_jid: str) -> Tuple[Optional[str], str]:
    """Download media from a message and return the local file path.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        A tuple of (file path or None, status message). The message carries the
        reason on failure: the bridge already reports one, and collapsing every
        failure mode into a bare None left the caller unable to tell an expired
        media from a bad auth token from a bridge that is simply down.
    """
    try:
        url = f"{WHATSAPP_API_BASE_URL}/download"
        payload = {
            "message_id": message_id,
            "chat_jid": chat_jid
        }

        response = requests.post(url, json=payload, headers=_auth_headers())

        if response.status_code == 200:
            result = response.json()
            if result.get("success", False):
                path = result.get("path")
                logger.info("Media downloaded successfully: %s", path)
                return path, "Media downloaded successfully"
            else:
                reason = result.get('message', 'Unknown error')
                logger.warning("Download failed: %s", reason)
                return None, reason
        elif response.status_code == 401:
            reason = (
                "HTTP 401 - check WHATSAPP_API_AUTH_TOKEN matches the bridge's "
                f"API_AUTH_TOKEN ({response.text})"
            )
            logger.warning("Download auth error: %s", reason)
            return None, reason
        else:
            reason = f"HTTP {response.status_code} - {response.text}"
            logger.warning("Download error: %s", reason)
            return None, reason

    except requests.RequestException as e:
        logger.warning("Download request error: %s", e)
        return None, f"Request error: {str(e)}"
    except json.JSONDecodeError:
        logger.warning("Error parsing download response: %s", response.text)
        return None, f"Error parsing response: {response.text}"
    except Exception as e:
        logger.warning("Unexpected download error: %s", e)
        return None, f"Unexpected error: {str(e)}"


def create_group(
    name: str,
    participants: List[str],
    is_community: bool = False,
    community_parent_jid: str = "",
) -> Tuple[bool, str, Optional[dict]]:
    try:
        if not name or not name.strip():
            return False, "Group name is required", None
        if not participants:
            return False, "At least one participant is required", None
        url = f"{WHATSAPP_API_BASE_URL}/create_group"
        payload = {
            "name": name,
            "participants": participants,
            "is_community": is_community,
            "community_parent_jid": community_parent_jid,
        }
        response = requests.post(url, json=payload, headers=_auth_headers())
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}", None
        success = bool(result.get("success", False))
        message = result.get("message", "Unknown response")
        details = {
            "jid": result.get("jid"),
            "name": result.get("name"),
            "participant_count": result.get("participant_count"),
        } if success else None
        return success, message, details
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}", None
    except Exception as e:
        return False, f"Unexpected error: {str(e)}", None


def leave_group(jid: str) -> Tuple[bool, str]:
    try:
        if not jid or not jid.strip():
            return False, "Group JID is required"
        response = _api_request("POST", "/leave_group", json={"jid": jid})
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}"
        return bool(result.get("success", False)), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def mark_chat_read(chat_jid: str, message_ids: List[str], sender_jid: str = "", timestamp: int = 0) -> Tuple[bool, str]:
    try:
        if not chat_jid or not chat_jid.strip():
            return False, "chat_jid is required"
        payload: Dict[str, Any] = {"chat_jid": chat_jid, "message_ids": message_ids}
        if sender_jid:
            payload["sender_jid"] = sender_jid
        if timestamp:
            payload["timestamp"] = timestamp
        response = _api_request("POST", "/mark_chat_read", json=payload)
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}"
        return bool(result.get("success", False)), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def mark_chat_unread(chat_jid: str) -> Tuple[bool, str]:
    try:
        if not chat_jid or not chat_jid.strip():
            return False, "chat_jid is required"
        response = _api_request("POST", "/mark_chat_unread", json={"chat_jid": chat_jid})
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}"
        return bool(result.get("success", False)), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def get_group_info(jid: str) -> Tuple[bool, str, Optional[dict]]:
    try:
        if not jid or not jid.strip():
            return False, "Group JID is required", None
        response = _api_request("GET", "/group_info", params={"jid": jid})
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}", None
        success = bool(result.get("success", False))
        if not success:
            return False, result.get("message", "Unknown response"), None
        info = {
            "name": result.get("name", ""),
            "participants": result.get("participants", []),
        }
        return True, "Group info retrieved", info
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}", None
    except Exception as e:
        return False, f"Unexpected error: {str(e)}", None


def archive_chat(chat_jid: str, archive: bool) -> Tuple[bool, str]:
    try:
        if not chat_jid or not chat_jid.strip():
            return False, "chat_jid is required"
        response = _api_request("POST", "/archive_chat", json={"chat_jid": chat_jid, "archive": archive})
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}"
        return bool(result.get("success", False)), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def resolve_contact(phone: str) -> Tuple[bool, str, List[str]]:
    try:
        if not phone or not phone.strip():
            return False, "phone is required", []
        response = _api_request("GET", "/resolve_contact", params={"phone": phone})
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}", []
        success = bool(result.get("success", False))
        jids = result.get("jids", []) or []
        return success, result.get("message", "Contact resolved" if success else "Unknown response"), jids
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}", []
    except Exception as e:
        return False, f"Unexpected error: {str(e)}", []


def react_to_message(chat_jid: str, message_id: str, emoji: str, from_me: bool = True) -> Tuple[bool, str]:
    """React to a message with an emoji ("" removes the reaction).

    from_me defaults True (your own message). from_me=False works only in direct
    chats; in group chats the bridge returns an error because the original
    sender's JID isn't available.
    """
    try:
        if not chat_jid or not chat_jid.strip():
            return False, "chat_jid is required"
        if not message_id or not message_id.strip():
            return False, "message_id is required"
        payload = {
            "chat_jid": chat_jid,
            "message_id": message_id,
            "emoji": emoji,
            "from_me": from_me,
        }
        response = _api_request("POST", "/react", json=payload)
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}"
        return bool(result.get("success", False)), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def edit_message(chat_jid: str, message_id: str, new_text: str, from_me: bool = True) -> Tuple[bool, str]:
    """Edit the text of a previously sent message.

    from_me is accepted for API symmetry but ignored — WhatsApp (whatsmeow's
    BuildEdit) only allows editing your own messages; the bridge never reads it.
    """
    try:
        if not chat_jid or not chat_jid.strip():
            return False, "chat_jid is required"
        if not message_id or not message_id.strip():
            return False, "message_id is required"
        payload = {
            "chat_jid": chat_jid,
            "message_id": message_id,
            "new_text": new_text,
            "from_me": from_me,
        }
        response = _api_request("POST", "/edit", json=payload)
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}"
        return bool(result.get("success", False)), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def delete_message(chat_jid: str, message_id: str, from_me: bool = True) -> Tuple[bool, str]:
    """Delete a message for everyone (revoke).

    from_me defaults True (your own message). from_me=False works only in direct
    chats; in group chats the bridge returns an error because the original
    sender's JID isn't available.
    """
    try:
        if not chat_jid or not chat_jid.strip():
            return False, "chat_jid is required"
        if not message_id or not message_id.strip():
            return False, "message_id is required"
        payload = {
            "chat_jid": chat_jid,
            "message_id": message_id,
            "from_me": from_me,
        }
        response = _api_request("POST", "/revoke", json=payload)
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}"
        return bool(result.get("success", False)), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def update_group_participants(group_jid: str, participants: List[str], action: str) -> Tuple[bool, str, List[dict]]:
    """Update group participants (add/remove/promote/demote).

    Returns (success, message, participants_list). Each participant in the list
    has jid, is_admin, error (0 = applied, non-0 = WhatsApp error code),
    and optionally add_request (true if invitation pending).
    """
    try:
        if not group_jid or not group_jid.strip():
            return False, "group_jid is required", []
        if not participants:
            return False, "participants list is required", []
        payload = {
            "group_jid": group_jid,
            "participants": participants,
            "action": action,
        }
        response = _api_request("POST", "/group_participants", json=payload)
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}", []
        return (
            bool(result.get("success", False)),
            result.get("message", "Unknown response"),
            result.get("participants", []),
        )
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}", []
    except Exception as e:
        return False, f"Unexpected error: {str(e)}", []


def send_chat_presence(chat_jid: str, state: str, media: str = "") -> Tuple[bool, str]:
    """Send chat presence (typing/paused).

    state: 'composing' (typing) or 'paused' (stopped).
    media: '' (text, default) or 'audio' (recording).
    """
    try:
        if not chat_jid or not chat_jid.strip():
            return False, "chat_jid is required"
        payload = {
            "chat_jid": chat_jid,
            "state": state,
            "media": media,
        }
        response = _api_request("POST", "/chat_presence", json=payload)
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}"
        return bool(result.get("success", False)), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"


def check_whatsapp(phones: List[str]) -> Tuple[bool, str, List[dict]]:
    """Check if phone numbers are on WhatsApp.

    phones: list of phone numbers (with or without +). Each number is checked
    and result includes query (normalized), jid, is_in, and optionally verified_name.
    """
    try:
        if not phones:
            return False, "phones list is required", []
        payload = {"phones": phones}
        response = _api_request("POST", "/is_on_whatsapp", json=payload)
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}", []
        return (
            bool(result.get("success", False)),
            result.get("message", "Unknown response"),
            result.get("results", []),
        )
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}", []
    except Exception as e:
        return False, f"Unexpected error: {str(e)}", []
