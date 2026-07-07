import sqlite3
import unicodedata
from datetime import datetime
from dataclasses import dataclass
from typing import Optional, List, Tuple
import os
import os.path
import requests
import json
import audio

MESSAGES_DB_PATH = os.path.join(os.path.dirname(os.path.abspath(__file__)), '..', 'whatsapp-bridge', 'store', 'messages.db')
# whatsapp.db (whatsmeow session/lid_map/contacts) is no longer read from Python.
# Contact/LID resolution goes through the bridge REST API (see _resolve_phone_to_jids,
# search_contacts). This decouples the MCP server from the whatsmeow internal schema.
WHATSAPP_API_BASE_URL = os.environ.get("WHATSAPP_API_BASE_URL", "http://localhost:8080/api")


def _strip_accents(text: Optional[str]) -> Optional[str]:
    """Lowercase and remove diacritics so searches match regardless of accents."""
    if text is None:
        return None
    decomposed = unicodedata.normalize('NFD', text)
    return ''.join(c for c in decomposed if unicodedata.category(c) != 'Mn').lower()


def _connect_messages_db() -> sqlite3.Connection:
    """Open the messages DB with an `unaccent` SQL function registered."""
    conn = sqlite3.connect(MESSAGES_DB_PATH)
    conn.create_function("unaccent", 1, _strip_accents, deterministic=True)
    return conn

def _normalize_phone(phone: str) -> str:
    return phone.replace('+', '').replace(' ', '').replace('-', '')

def _resolve_phone_to_jids(phone: str) -> List[str]:
    """Return all JIDs (regular + LID) that match a phone number.

    Delegates to the bridge (GET /api/resolve_contact), which owns whatsmeow and
    resolves LID via the library API. No longer reads the internal whatsmeow_lid_map
    table directly, so a whatsmeow schema change can't break resolution silently.

    Behavior deltas vs the old direct-DB read: (a) resolution is exact-match only
    (the old fuzzy suffix fallback is gone); (b) requires the bridge to be running —
    if it's down, falls back to PN-only (no LID).
    """
    phone = _normalize_phone(phone)
    try:
        r = requests.get(f"{WHATSAPP_API_BASE_URL}/resolve_contact",
                         params={"phone": phone}, timeout=10)
        if r.status_code == 200:
            body = r.json()
            if body.get("success") and body.get("jids"):
                return body["jids"]
    except (requests.RequestException, json.JSONDecodeError):
        pass
    # Fallback: bridge unreachable — return at least the PN JID so callers still work.
    return [f"{phone}@s.whatsapp.net"]

def _get_contact_name(phone: str) -> Optional[str]:
    """Look up a contact name via the bridge search endpoint (by phone number).

    Requires the bridge to be running; returns None if it's down (no local DB fallback).
    """
    phone = _normalize_phone(phone)
    try:
        r = requests.get(f"{WHATSAPP_API_BASE_URL}/search_contacts",
                         params={"query": phone}, timeout=10)
        if r.status_code == 200:
            body = r.json()
            for c in body.get("contacts", []):
                if _normalize_phone(c.get("phone_number", "")) == phone and c.get("name"):
                    return c["name"]
    except (requests.RequestException, json.JSONDecodeError):
        pass
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

def get_sender_name(sender_jid: str) -> str:
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # First try matching by exact JID
        cursor.execute("""
            SELECT name
            FROM chats
            WHERE jid = ?
            LIMIT 1
        """, (sender_jid,))
        
        result = cursor.fetchone()
        
        # If no result, try looking for the number within JIDs
        if not result:
            # Extract the phone number part if it's a JID
            if '@' in sender_jid:
                phone_part = sender_jid.split('@')[0]
            else:
                phone_part = sender_jid
                
            cursor.execute("""
                SELECT name
                FROM chats
                WHERE jid LIKE ?
                LIMIT 1
            """, (f"%{phone_part}%",))
            
            result = cursor.fetchone()
        
        if result and result[0]:
            return result[0]
        else:
            return sender_jid
        
    except sqlite3.Error as e:
        print(f"Database error while getting sender name: {e}")
        return sender_jid
    finally:
        if 'conn' in locals():
            conn.close()

def format_message(message: Message, show_chat_info: bool = True) -> None:
    """Print a single message with consistent formatting."""
    output = ""
    
    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "
        
    content_prefix = ""
    if hasattr(message, 'media_type') and message.media_type:
        content_prefix = f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "
    
    try:
        sender_name = get_sender_name(message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        print(f"Error formatting message: {e}")
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
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type FROM messages"]
        query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
        where_clauses = []
        params = []
        
        # Add filters
        if after:
            try:
                after = datetime.fromisoformat(after)
            except ValueError:
                raise ValueError(f"Invalid date format for 'after': {after}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp > ?")
            params.append(after)

        if before:
            try:
                before = datetime.fromisoformat(before)
            except ValueError:
                raise ValueError(f"Invalid date format for 'before': {before}. Please use ISO-8601 format.")
            
            where_clauses.append("messages.timestamp < ?")
            params.append(before)

        if sender_phone_number:
            jids = _resolve_phone_to_jids(sender_phone_number)
            jid_placeholders = ','.join('?' * len(jids))
            where_clauses.append(
                f"(messages.chat_jid IN ({jid_placeholders}) AND messages.chat_jid NOT LIKE '%@g.us')"
            )
            params.extend(jids)
            
        if chat_jid:
            where_clauses.append("messages.chat_jid = ?")
            params.append(chat_jid)
            
        if query:
            where_clauses.append("unaccent(messages.content) LIKE unaccent(?)")
            params.append(f"%{query}%")
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add pagination
        offset = page * limit
        query_parts.append("ORDER BY messages.timestamp DESC")
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        messages = cursor.fetchall()
        
        result = []
        for msg in messages:
            message = Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            )
            result.append(message)
            
        if include_context and result:
            # Add context for each message
            messages_with_context = []
            for msg in result:
                context = get_message_context(msg.id, context_before, context_after)
                messages_with_context.extend(context.before)
                messages_with_context.append(context.message)
                messages_with_context.extend(context.after)
            
            return format_messages_list(messages_with_context, show_chat_info=True)
            
        # Format and display messages without context
        return format_messages_list(result, show_chat_info=True)    
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> MessageContext:
    """Get context around a specific message."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        # Get the target message first
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.chat_jid, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.id = ?
        """, (message_id,))
        msg_data = cursor.fetchone()
        
        if not msg_data:
            raise ValueError(f"Message with ID {message_id} not found")
            
        target_message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[8]
        )
        
        # Get messages before
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp < ?
            ORDER BY messages.timestamp DESC
            LIMIT ?
        """, (msg_data[7], msg_data[0], before))
        
        before_messages = []
        for msg in cursor.fetchall():
            before_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        # Get messages after
        cursor.execute("""
            SELECT messages.timestamp, messages.sender, chats.name, messages.content, messages.is_from_me, chats.jid, messages.id, messages.media_type
            FROM messages
            JOIN chats ON messages.chat_jid = chats.jid
            WHERE messages.chat_jid = ? AND messages.timestamp > ?
            ORDER BY messages.timestamp ASC
            LIMIT ?
        """, (msg_data[7], msg_data[0], after))
        
        after_messages = []
        for msg in cursor.fetchall():
            after_messages.append(Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7]
            ))
        
        return MessageContext(
            message=target_message,
            before=before_messages,
            after=after_messages
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        raise
    finally:
        if 'conn' in locals():
            conn.close()


def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Chat]:
    """Get chats matching the specified criteria."""
    try:
        conn = _connect_messages_db()
        cursor = conn.cursor()
        
        # Build base query
        query_parts = ["""
            SELECT 
                chats.jid,
                chats.name,
                chats.last_message_time,
                messages.content as last_message,
                messages.sender as last_sender,
                messages.is_from_me as last_is_from_me
            FROM chats
        """]
        
        if include_last_message:
            query_parts.append("""
                LEFT JOIN messages ON chats.jid = messages.chat_jid 
                AND chats.last_message_time = messages.timestamp
            """)
            
        where_clauses = []
        params = []
        
        if query:
            where_clauses.append("(unaccent(chats.name) LIKE unaccent(?) OR chats.jid LIKE ?)")
            params.extend([f"%{query}%", f"%{query}%"])
            
        if where_clauses:
            query_parts.append("WHERE " + " AND ".join(where_clauses))
            
        # Add sorting
        order_by = "chats.last_message_time DESC" if sort_by == "last_active" else "chats.name"
        query_parts.append(f"ORDER BY {order_by}")
        
        # Add pagination
        offset = (page ) * limit
        query_parts.append("LIMIT ? OFFSET ?")
        params.extend([limit, offset])
        
        cursor.execute(" ".join(query_parts), tuple(params))
        chats = cursor.fetchall()
        
        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def search_contacts(query: str) -> List[Contact]:
    """Search contacts by name or phone number.

    Delegates to the bridge (GET /api/search_contacts), which queries the three
    sources it owns (whatsmeow contact store + senders + chats). No longer reads
    whatsmeow_contacts/whatsmeow_lid_map directly, decoupling from the lib schema.

    Behavior deltas vs the old direct-DB read: (a) matching is exact-match via the
    bridge (no fuzzy suffix fallback); (b) requires the bridge to be running —
    returns an empty list if it's down.
    """
    try:
        r = requests.get(f"{WHATSAPP_API_BASE_URL}/search_contacts",
                         params={"query": query}, timeout=10)
        if r.status_code == 200:
            body = r.json()
            if body.get("success"):
                return [
                    Contact(
                        phone_number=c.get("phone_number", ""),
                        name=c.get("name"),
                        jid=c.get("jid", ""),
                    )
                    for c in body.get("contacts", [])
                ]
    except (requests.RequestException, json.JSONDecodeError) as e:
        print(f"search_contacts: bridge error: {e}")
    return []


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Chat]:
    """Get all chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT DISTINCT
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
            JOIN messages m ON c.jid = m.chat_jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY c.last_message_time DESC
            LIMIT ? OFFSET ?
        """, (jid, jid, limit, page * limit))
        
        chats = cursor.fetchall()
        
        result = []
        for chat_data in chats:
            chat = Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5]
            )
            result.append(chat)
            
        return result
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return []
    finally:
        if 'conn' in locals():
            conn.close()


def get_last_interaction(jid: str) -> str:
    """Get most recent message involving the contact."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        cursor.execute("""
            SELECT 
                m.timestamp,
                m.sender,
                c.name,
                m.content,
                m.is_from_me,
                c.jid,
                m.id,
                m.media_type
            FROM messages m
            JOIN chats c ON m.chat_jid = c.jid
            WHERE m.sender = ? OR c.jid = ?
            ORDER BY m.timestamp DESC
            LIMIT 1
        """, (jid, jid))
        
        msg_data = cursor.fetchone()
        
        if not msg_data:
            return None
            
        message = Message(
            timestamp=datetime.fromisoformat(msg_data[0]),
            sender=msg_data[1],
            chat_name=msg_data[2],
            content=msg_data[3],
            is_from_me=msg_data[4],
            chat_jid=msg_data[5],
            id=msg_data[6],
            media_type=msg_data[7]
        )
        
        return format_message(message)
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Chat]:
    """Get chat metadata by JID."""
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        
        query = """
            SELECT 
                c.jid,
                c.name,
                c.last_message_time,
                m.content as last_message,
                m.sender as last_sender,
                m.is_from_me as last_is_from_me
            FROM chats c
        """
        
        if include_last_message:
            query += """
                LEFT JOIN messages m ON c.jid = m.chat_jid 
                AND c.last_message_time = m.timestamp
            """
            
        query += " WHERE c.jid = ?"
        
        cursor.execute(query, (chat_jid,))
        chat_data = cursor.fetchone()
        
        if not chat_data:
            return None
            
        return Chat(
            jid=chat_data[0],
            name=chat_data[1],
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
        
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()


def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Chat]:
    """Get chat metadata by sender phone number (handles LID contacts)."""
    jids = _resolve_phone_to_jids(sender_phone_number)
    try:
        conn = sqlite3.connect(MESSAGES_DB_PATH)
        cursor = conn.cursor()
        placeholders = ','.join('?' * len(jids))
        cursor.execute(f"""
            SELECT c.jid, c.name, c.last_message_time,
                   m.content, m.sender, m.is_from_me
            FROM chats c
            LEFT JOIN messages m ON c.jid = m.chat_jid
                AND c.last_message_time = m.timestamp
            WHERE c.jid IN ({placeholders}) AND c.jid NOT LIKE '%@g.us'
            LIMIT 1
        """, jids)
        chat_data = cursor.fetchone()
        if not chat_data:
            return None
        name = chat_data[1]
        if not name or name.replace('@lid', '').isdigit():
            name = _get_contact_name(sender_phone_number) or name
        return Chat(
            jid=chat_data[0],
            name=name,
            last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
            last_message=chat_data[3],
            last_sender=chat_data[4],
            last_is_from_me=chat_data[5]
        )
    except sqlite3.Error as e:
        print(f"Database error: {e}")
        return None
    finally:
        if 'conn' in locals():
            conn.close()

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
        
        response = requests.post(url, json=payload)
        
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
        
        response = requests.post(url, json=payload)
        
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
        
        response = requests.post(url, json=payload)
        
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

def download_media(message_id: str, chat_jid: str) -> Optional[str]:
    """Download media from a message and return the local file path.
    
    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
    
    Returns:
        The local file path if download was successful, None otherwise
    """
    try:
        url = f"{WHATSAPP_API_BASE_URL}/download"
        payload = {
            "message_id": message_id,
            "chat_jid": chat_jid
        }
        
        response = requests.post(url, json=payload)
        
        if response.status_code == 200:
            result = response.json()
            if result.get("success", False):
                path = result.get("path")
                print(f"Media downloaded successfully: {path}")
                return path
            else:
                print(f"Download failed: {result.get('message', 'Unknown error')}")
                return None
        else:
            print(f"Error: HTTP {response.status_code} - {response.text}")
            return None
            
    except requests.RequestException as e:
        print(f"Request error: {str(e)}")
        return None
    except json.JSONDecodeError:
        print(f"Error parsing response: {response.text}")
        return None
    except Exception as e:
        print(f"Unexpected error: {str(e)}")
        return None


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
        response = requests.post(url, json=payload)
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
        url = f"{WHATSAPP_API_BASE_URL}/leave_group"
        response = requests.post(url, json={"jid": jid})
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
        url = f"{WHATSAPP_API_BASE_URL}/mark_chat_read"
        payload: Dict[str, Any] = {"chat_jid": chat_jid, "message_ids": message_ids}
        if sender_jid:
            payload["sender_jid"] = sender_jid
        if timestamp:
            payload["timestamp"] = timestamp
        response = requests.post(url, json=payload)
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
        url = f"{WHATSAPP_API_BASE_URL}/mark_chat_unread"
        response = requests.post(url, json={"chat_jid": chat_jid})
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
        url = f"{WHATSAPP_API_BASE_URL}/group_info"
        response = requests.get(url, params={"jid": jid})
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
        url = f"{WHATSAPP_API_BASE_URL}/archive_chat"
        response = requests.post(url, json={"chat_jid": chat_jid, "archive": archive})
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
        url = f"{WHATSAPP_API_BASE_URL}/resolve_contact"
        response = requests.get(url, params={"phone": phone})
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
        url = f"{WHATSAPP_API_BASE_URL}/react"
        payload = {
            "chat_jid": chat_jid,
            "message_id": message_id,
            "emoji": emoji,
            "from_me": from_me,
        }
        response = requests.post(url, json=payload)
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
        url = f"{WHATSAPP_API_BASE_URL}/edit"
        payload = {
            "chat_jid": chat_jid,
            "message_id": message_id,
            "new_text": new_text,
            "from_me": from_me,
        }
        response = requests.post(url, json=payload)
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
        url = f"{WHATSAPP_API_BASE_URL}/revoke"
        payload = {
            "chat_jid": chat_jid,
            "message_id": message_id,
            "from_me": from_me,
        }
        response = requests.post(url, json=payload)
        try:
            result = response.json()
        except json.JSONDecodeError:
            return False, f"Error parsing response: {response.text}"
        return bool(result.get("success", False)), result.get("message", "Unknown response")
    except requests.RequestException as e:
        return False, f"Request error: {str(e)}"
    except Exception as e:
        return False, f"Unexpected error: {str(e)}"
