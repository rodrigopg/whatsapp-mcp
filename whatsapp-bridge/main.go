package main

import (
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/mdp/qrterminal"
	goqr "github.com/skip2/go-qrcode"

	"bytes"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// qrState holds the latest QR code PNG in memory so /qr can serve it.
var qrState struct {
	sync.RWMutex
	png       []byte // nil = not waiting for QR (already authenticated or not yet started)
	connected bool
}

// Message represents a chat message for our client
type Message struct {
	Time      time.Time
	Sender    string
	Content   string
	IsFromMe  bool
	MediaType string
	Filename  string
}

// Database handler for storing message history
type MessageStore struct {
	db *sql.DB
}

// Initialize message store
func NewMessageStore() (*MessageStore, error) {
	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		return nil, fmt.Errorf("failed to create store directory: %v", err)
	}

	// Open SQLite database for messages
	db, err := sql.Open("sqlite3", "file:store/messages.db?_foreign_keys=on")
	if err != nil {
		return nil, fmt.Errorf("failed to open message database: %v", err)
	}

	// Create tables if they don't exist
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (
			jid TEXT PRIMARY KEY,
			name TEXT,
			last_message_time TIMESTAMP
		);
		
		CREATE TABLE IF NOT EXISTS messages (
			id TEXT,
			chat_jid TEXT,
			sender TEXT,
			content TEXT,
			timestamp TIMESTAMP,
			is_from_me BOOLEAN,
			media_type TEXT,
			filename TEXT,
			url TEXT,
			media_key BLOB,
			file_sha256 BLOB,
			file_enc_sha256 BLOB,
			file_length INTEGER,
			PRIMARY KEY (id, chat_jid),
			FOREIGN KEY (chat_jid) REFERENCES chats(jid)
		);

		CREATE TABLE IF NOT EXISTS senders (
			jid TEXT PRIMARY KEY,
			push_name TEXT,
			full_name TEXT,
			first_name TEXT,
			business_name TEXT,
			updated_at TIMESTAMP
		);
		CREATE INDEX IF NOT EXISTS idx_senders_names ON senders(full_name, push_name);
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create tables: %v", err)
	}

	return &MessageStore{db: db}, nil
}

// Close the database connection
func (store *MessageStore) Close() error {
	return store.db.Close()
}

// TouchChatLastMessageTime updates only last_message_time on an existing chat row.
func (store *MessageStore) TouchChatLastMessageTime(jid string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"UPDATE chats SET last_message_time = ? WHERE jid = ?",
		lastMessageTime, jid,
	)
	return err
}

// EnsureChat creates a chat row if none exists, leaving any existing row untouched.
// Required before StoreMessage in the outbound path to satisfy the FOREIGN KEY constraint.
func (store *MessageStore) EnsureChat(jid string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR IGNORE INTO chats (jid, name, last_message_time) VALUES (?, '', ?)",
		jid, lastMessageTime,
	)
	return err
}

// StoreSender upserts a sender row, preserving existing non-empty fields.
func (store *MessageStore) StoreSender(jid, pushName, fullName, firstName, businessName string) error {
	if jid == "" {
		return nil
	}
	_, err := store.db.Exec(`
		INSERT INTO senders (jid, push_name, full_name, first_name, business_name, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET
			push_name     = COALESCE(NULLIF(excluded.push_name, ''),     senders.push_name),
			full_name     = COALESCE(NULLIF(excluded.full_name, ''),     senders.full_name),
			first_name    = COALESCE(NULLIF(excluded.first_name, ''),    senders.first_name),
			business_name = COALESCE(NULLIF(excluded.business_name, ''), senders.business_name),
			updated_at    = excluded.updated_at
	`, jid, pushName, fullName, firstName, businessName, time.Now())
	return err
}

// ResolveName returns the best human-readable name for a JID from the senders table.
func (store *MessageStore) ResolveName(jid string) string {
	var fullName, businessName, pushName sql.NullString
	err := store.db.QueryRow(
		"SELECT full_name, business_name, push_name FROM senders WHERE jid = ?", jid,
	).Scan(&fullName, &businessName, &pushName)
	if err != nil {
		return ""
	}
	if fullName.Valid && fullName.String != "" {
		return fullName.String
	}
	if businessName.Valid && businessName.String != "" {
		return businessName.String
	}
	if pushName.Valid && pushName.String != "" {
		return pushName.String
	}
	return ""
}

// SyncAllContacts pulls the full whatsmeow contact store into the senders table.
func SyncAllContacts(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	if client == nil || client.Store == nil || client.Store.Contacts == nil {
		return
	}
	contacts, err := client.Store.Contacts.GetAllContacts(context.Background())
	if err != nil {
		logger.Warnf("Failed to sync contacts: %v", err)
		return
	}
	count := 0
	for jid, info := range contacts {
		if err := store.StoreSender(jid.String(), info.PushName, info.FullName, info.FirstName, info.BusinessName); err == nil {
			count++
		}
	}
	logger.Infof("Synced %d contacts into senders table", count)
}

// Store a chat in the database
func (store *MessageStore) StoreChat(jid, name string, lastMessageTime time.Time) error {
	_, err := store.db.Exec(
		"INSERT OR REPLACE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)",
		jid, name, lastMessageTime,
	)
	return err
}

// Store a message in the database
func (store *MessageStore) StoreMessage(id, chatJID, sender, content string, timestamp time.Time, isFromMe bool,
	mediaType, filename, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	// Only store if there's actual content or media
	if content == "" && mediaType == "" {
		return nil
	}

	_, err := store.db.Exec(
		`INSERT OR REPLACE INTO messages 
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, chatJID, sender, content, timestamp, isFromMe, mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength,
	)
	return err
}

// Get messages from a chat
func (store *MessageStore) GetMessages(chatJID string, limit int) ([]Message, error) {
	rows, err := store.db.Query(
		"SELECT sender, content, timestamp, is_from_me, media_type, filename FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT ?",
		chatJID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var messages []Message
	for rows.Next() {
		var msg Message
		var timestamp time.Time
		err := rows.Scan(&msg.Sender, &msg.Content, &timestamp, &msg.IsFromMe, &msg.MediaType, &msg.Filename)
		if err != nil {
			return nil, err
		}
		msg.Time = timestamp
		messages = append(messages, msg)
	}

	return messages, nil
}

// Get all chats
func (store *MessageStore) GetChats() (map[string]time.Time, error) {
	rows, err := store.db.Query("SELECT jid, last_message_time FROM chats ORDER BY last_message_time DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	chats := make(map[string]time.Time)
	for rows.Next() {
		var jid string
		var lastMessageTime time.Time
		err := rows.Scan(&jid, &lastMessageTime)
		if err != nil {
			return nil, err
		}
		chats[jid] = lastMessageTime
	}

	return chats, nil
}

// Extract text content from a message
func extractTextContent(msg *waProto.Message) string {
	if msg == nil {
		return ""
	}

	// Try to get text content
	if text := msg.GetConversation(); text != "" {
		return text
	} else if extendedText := msg.GetExtendedTextMessage(); extendedText != nil {
		return extendedText.GetText()
	}

	// Media messages can carry a text caption that should be searchable
	if img := msg.GetImageMessage(); img != nil {
		return img.GetCaption()
	} else if video := msg.GetVideoMessage(); video != nil {
		return video.GetCaption()
	} else if doc := msg.GetDocumentMessage(); doc != nil {
		return doc.GetCaption()
	}

	return ""
}

// SendMessageResponse represents the response for the send message API
type SendMessageResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// SendMessageRequest represents the request body for the send message API
type SendMessageRequest struct {
	Recipient string `json:"recipient"`
	Message   string `json:"message"`
	MediaPath string `json:"media_path,omitempty"`
}

// Function to send a WhatsApp message
func sendWhatsAppMessage(client *whatsmeow.Client, messageStore *MessageStore, recipient string, message string, mediaPath string) (bool, string) {
	if !client.IsConnected() {
		return false, "Not connected to WhatsApp"
	}

	// Create JID for recipient
	var recipientJID types.JID
	var err error

	// Check if recipient is a JID
	isJID := strings.Contains(recipient, "@")

	if isJID {
		// Parse the JID string
		recipientJID, err = types.ParseJID(recipient)
		if err != nil {
			return false, fmt.Sprintf("Error parsing JID: %v", err)
		}
	} else {
		// Create JID from phone number
		recipientJID = types.JID{
			User:   recipient,
			Server: "s.whatsapp.net", // For personal chats
		}
	}

	msg := &waProto.Message{}

	// Check if we have media to send
	if mediaPath != "" {
		// Read media file
		mediaData, err := os.ReadFile(mediaPath)
		if err != nil {
			return false, fmt.Sprintf("Error reading media file: %v", err)
		}

		// Determine media type and mime type based on file extension
		fileExt := strings.ToLower(mediaPath[strings.LastIndex(mediaPath, ".")+1:])
		var mediaType whatsmeow.MediaType
		var mimeType string

		// Handle different media types
		switch fileExt {
		// Image types
		case "jpg", "jpeg":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/jpeg"
		case "png":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/png"
		case "gif":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/gif"
		case "webp":
			mediaType = whatsmeow.MediaImage
			mimeType = "image/webp"

		// Audio types
		case "ogg":
			mediaType = whatsmeow.MediaAudio
			mimeType = "audio/ogg; codecs=opus"

		// Video types
		case "mp4":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/mp4"
		case "avi":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/avi"
		case "mov":
			mediaType = whatsmeow.MediaVideo
			mimeType = "video/quicktime"

		// Document types — use stdlib mime detection, fallback to octet-stream.
		default:
			mediaType = whatsmeow.MediaDocument
			if detected := mime.TypeByExtension("." + fileExt); detected != "" {
				mimeType = detected
			} else {
				mimeType = "application/octet-stream"
			}
		}

		// Upload media to WhatsApp servers
		resp, err := client.Upload(context.Background(), mediaData, mediaType)
		if err != nil {
			return false, fmt.Sprintf("Error uploading media: %v", err)
		}

		fmt.Println("Media uploaded", resp)

		// Create the appropriate message type based on media type
		switch mediaType {
		case whatsmeow.MediaImage:
			msg.ImageMessage = &waProto.ImageMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaAudio:
			// Handle ogg audio files
			var seconds uint32 = 30 // Default fallback
			var waveform []byte = nil

			// Try to analyze the ogg file
			if strings.Contains(mimeType, "ogg") {
				analyzedSeconds, analyzedWaveform, err := analyzeOggOpus(mediaData)
				if err == nil {
					seconds = analyzedSeconds
					waveform = analyzedWaveform
				} else {
					return false, fmt.Sprintf("Failed to analyze Ogg Opus file: %v", err)
				}
			} else {
				fmt.Printf("Not an Ogg Opus file: %s\n", mimeType)
			}

			msg.AudioMessage = &waProto.AudioMessage{
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
				Seconds:       proto.Uint32(seconds),
				PTT:           proto.Bool(true),
				Waveform:      waveform,
			}
		case whatsmeow.MediaVideo:
			msg.VideoMessage = &waProto.VideoMessage{
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		case whatsmeow.MediaDocument:
			docFilename := filepath.Base(mediaPath)
			msg.DocumentMessage = &waProto.DocumentMessage{
				FileName:      proto.String(docFilename),
				Title:         proto.String(docFilename),
				Caption:       proto.String(message),
				Mimetype:      proto.String(mimeType),
				URL:           &resp.URL,
				DirectPath:    &resp.DirectPath,
				MediaKey:      resp.MediaKey,
				FileEncSHA256: resp.FileEncSHA256,
				FileSHA256:    resp.FileSHA256,
				FileLength:    &resp.FileLength,
			}
		}
	} else {
		msg.Conversation = proto.String(message)
	}

	// Send message
	resp, err := client.SendMessage(context.Background(), recipientJID, msg)

	if err != nil {
		return false, fmt.Sprintf("Error sending message: %v", err)
	}

	// Persist text outbounds so own-sends appear in the local store.
	// Multi-device echo via handleMessage doesn't fire on single-device accounts.
	if messageStore != nil && mediaPath == "" && client.Store != nil && client.Store.ID != nil {
		chatJID := recipientJID.String()
		sender := client.Store.ID.User
		if ensureErr := messageStore.EnsureChat(chatJID, resp.Timestamp); ensureErr != nil {
			fmt.Printf("Failed to ensure chat row: %v\n", ensureErr)
		}
		if storeErr := messageStore.StoreMessage(
			resp.ID, chatJID, sender, message, resp.Timestamp, true,
			"", "", "", nil, nil, nil, 0,
		); storeErr != nil {
			fmt.Printf("Failed to persist outbound: %v\n", storeErr)
		} else {
			_ = messageStore.TouchChatLastMessageTime(chatJID, resp.Timestamp)
		}
	}

	return true, fmt.Sprintf("Message sent to %s", recipient)
}

// Extract media info from a message
func extractMediaInfo(msg *waProto.Message) (mediaType string, filename string, url string, mediaKey []byte, fileSHA256 []byte, fileEncSHA256 []byte, fileLength uint64) {
	if msg == nil {
		return "", "", "", nil, nil, nil, 0
	}

	// Check for image message
	if img := msg.GetImageMessage(); img != nil {
		return "image", "image_" + time.Now().Format("20060102_150405") + ".jpg",
			img.GetURL(), img.GetMediaKey(), img.GetFileSHA256(), img.GetFileEncSHA256(), img.GetFileLength()
	}

	// Check for video message
	if vid := msg.GetVideoMessage(); vid != nil {
		return "video", "video_" + time.Now().Format("20060102_150405") + ".mp4",
			vid.GetURL(), vid.GetMediaKey(), vid.GetFileSHA256(), vid.GetFileEncSHA256(), vid.GetFileLength()
	}

	// Check for audio message
	if aud := msg.GetAudioMessage(); aud != nil {
		return "audio", "audio_" + time.Now().Format("20060102_150405") + ".ogg",
			aud.GetURL(), aud.GetMediaKey(), aud.GetFileSHA256(), aud.GetFileEncSHA256(), aud.GetFileLength()
	}

	// Check for document message
	if doc := msg.GetDocumentMessage(); doc != nil {
		filename := doc.GetFileName()
		if filename == "" {
			filename = "document_" + time.Now().Format("20060102_150405")
		}
		return "document", filename,
			doc.GetURL(), doc.GetMediaKey(), doc.GetFileSHA256(), doc.GetFileEncSHA256(), doc.GetFileLength()
	}

	return "", "", "", nil, nil, nil, 0
}

// resolveToPN converts a LID JID (xxxx@lid) to its PN (phone number) JID using
// the local whatsmeow LID store. Returns the input unchanged for non-LID JIDs.
func resolveToPN(client *whatsmeow.Client, jid types.JID) types.JID {
	if client == nil || client.Store == nil || client.Store.LIDs == nil {
		return jid
	}
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	pn, err := client.Store.LIDs.GetPNForLID(ctx, jid)
	if err != nil || pn.IsEmpty() {
		return jid
	}
	return pn
}

// migrateLIDChats merges any chat stored under a LID JID into its PN JID.
// Idempotent: chats with no known mapping are left for the next startup.
func migrateLIDChats(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) {
	if client == nil || store == nil || store.db == nil {
		return
	}
	rows, err := store.db.Query("SELECT jid, name, last_message_time FROM chats WHERE jid LIKE '%@" + types.HiddenUserServer + "'")
	if err != nil {
		logger.Warnf("LID migration: failed to list LID chats: %v", err)
		return
	}
	type lidChat struct {
		jid             string
		name            string
		lastMessageTime time.Time
	}
	var lidChats []lidChat
	for rows.Next() {
		var c lidChat
		if err := rows.Scan(&c.jid, &c.name, &c.lastMessageTime); err == nil {
			lidChats = append(lidChats, c)
		}
	}
	rows.Close()
	if len(lidChats) == 0 {
		return
	}
	logger.Infof("LID migration: found %d chat(s) under @lid, attempting to merge", len(lidChats))
	tx, err := store.db.Begin()
	if err != nil {
		logger.Warnf("LID migration: cannot start tx: %v", err)
		return
	}
	merged, skipped := 0, 0
	for _, c := range lidChats {
		lidJID, err := types.ParseJID(c.jid)
		if err != nil {
			skipped++
			continue
		}
		pnJID := resolveToPN(client, lidJID)
		if pnJID.Server != types.DefaultUserServer {
			skipped++
			continue
		}
		pnStr := pnJID.String()
		if _, err := tx.Exec(
			"INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?) "+
				"ON CONFLICT(jid) DO UPDATE SET "+
				"  name = COALESCE(NULLIF(chats.name, ''), excluded.name), "+
				"  last_message_time = MAX(chats.last_message_time, excluded.last_message_time)",
			pnStr, c.name, c.lastMessageTime,
		); err != nil {
			skipped++
			continue
		}
		if _, err := tx.Exec("UPDATE OR IGNORE messages SET chat_jid = ? WHERE chat_jid = ?", pnStr, c.jid); err != nil {
			skipped++
			continue
		}
		if _, err := tx.Exec("DELETE FROM messages WHERE chat_jid = ?", c.jid); err != nil {
			skipped++
			continue
		}
		if _, err := tx.Exec("DELETE FROM chats WHERE jid = ?", c.jid); err != nil {
			skipped++
			continue
		}
		merged++
		logger.Infof("LID migration: merged %s -> %s", c.jid, pnStr)
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		logger.Warnf("LID migration: commit failed: %v", err)
		return
	}
	logger.Infof("LID migration: %d merged, %d skipped (no mapping yet)", merged, skipped)
}

// Handle regular incoming messages with media support
func handleMessage(client *whatsmeow.Client, messageStore *MessageStore, msg *events.Message, logger waLog.Logger) {
	// Normalize LID -> PN so the same contact doesn't split across two chat_jid values.
	chatJID := resolveToPN(client, msg.Info.Chat).String()
	sender := resolveToPN(client, msg.Info.Sender).User
	senderJID := resolveToPN(client, msg.Info.Sender).String()

	// Enrich senders table with identity data from this event.
	var fullName, firstName, businessName string
	if client.Store != nil && client.Store.Contacts != nil {
		if contact, err := client.Store.Contacts.GetContact(context.Background(), msg.Info.Sender); err == nil {
			fullName = contact.FullName
			firstName = contact.FirstName
			businessName = contact.BusinessName
		}
	}
	if err := messageStore.StoreSender(senderJID, msg.Info.PushName, fullName, firstName, businessName); err != nil {
		logger.Warnf("Failed to store sender: %v", err)
	}

	// Get appropriate chat name (pass nil for conversation since we don't have one for regular messages)
	name := GetChatName(client, messageStore, msg.Info.Chat, chatJID, nil, sender, logger)

	// Update chat in database with the message timestamp (keeps last message time updated)
	err := messageStore.StoreChat(chatJID, name, msg.Info.Timestamp)
	if err != nil {
		logger.Warnf("Failed to store chat: %v", err)
	}

	// Extract text content
	content := extractTextContent(msg.Message)

	// Extract media info
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength := extractMediaInfo(msg.Message)

	// Skip if there's no content and no media
	if content == "" && mediaType == "" {
		return
	}

	// Store message in database
	err = messageStore.StoreMessage(
		msg.Info.ID,
		chatJID,
		sender,
		content,
		msg.Info.Timestamp,
		msg.Info.IsFromMe,
		mediaType,
		filename,
		url,
		mediaKey,
		fileSHA256,
		fileEncSHA256,
		fileLength,
	)

	if err != nil {
		logger.Warnf("Failed to store message: %v", err)
	} else {
		// Log message reception
		timestamp := msg.Info.Timestamp.Format("2006-01-02 15:04:05")
		direction := "←"
		if msg.Info.IsFromMe {
			direction = "→"
		}

		displayName := messageStore.ResolveName(senderJID)
		if displayName == "" {
			displayName = sender
		}

		// Log based on message type
		if mediaType != "" {
			fmt.Printf("[%s] %s %s: [%s: %s] %s\n", timestamp, direction, displayName, mediaType, filename, content)
		} else if content != "" {
			fmt.Printf("[%s] %s %s: %s\n", timestamp, direction, displayName, content)
		}
	}
}

// DownloadMediaRequest represents the request body for the download media API
type DownloadMediaRequest struct {
	MessageID string `json:"message_id"`
	ChatJID   string `json:"chat_jid"`
}

// DownloadMediaResponse represents the response for the download media API
// CreateGroupRequest represents the request body for the create group API.
type CreateGroupRequest struct {
	Name               string   `json:"name"`
	Participants       []string `json:"participants"`
	IsCommunity        bool     `json:"is_community,omitempty"`
	CommunityParentJID string   `json:"community_parent_jid,omitempty"`
}

// CreateGroupResponse represents the response for the create group API.
type CreateGroupResponse struct {
	Success          bool   `json:"success"`
	Message          string `json:"message"`
	JID              string `json:"jid,omitempty"`
	Name             string `json:"name,omitempty"`
	ParticipantCount int    `json:"participant_count,omitempty"`
}

// LeaveGroupRequest represents the request body for the leave group API.
type LeaveGroupRequest struct {
	JID string `json:"jid"`
}

// LeaveGroupResponse represents the response for the leave group API.
type LeaveGroupResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// MarkChatReadRequest represents a request to mark a chat as read.
type MarkChatReadRequest struct {
	ChatJID    string   `json:"chat_jid"`
	MessageIDs []string `json:"message_ids"`
	SenderJID  string   `json:"sender_jid,omitempty"`
	Timestamp  int64    `json:"timestamp,omitempty"`
}

// MarkChatUnreadRequest represents a request to mark a chat as unread.
type MarkChatUnreadRequest struct {
	ChatJID string `json:"chat_jid"`
}

// ArchiveChatRequest represents a request to archive or unarchive a chat.
// Archive is a pointer so an omitted field is rejected rather than silently
// defaulting to false (which would unarchive on an "archive" endpoint).
type ArchiveChatRequest struct {
	ChatJID string `json:"chat_jid"`
	Archive *bool  `json:"archive"`
}

// MarkChatResponse represents the response for mark-read / mark-unread.
type MarkChatResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// safeSendAppState calls cli.SendAppState recovering from any panic (e.g. uninitialized
// app-state keys during session restore) and returns it as a regular error.
func safeSendAppState(cli *whatsmeow.Client, ctx context.Context, patch appstate.PatchInfo) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("app state not ready: %v", r)
		}
	}()
	return cli.SendAppState(ctx, patch)
}

// createWhatsAppGroup creates a new group on WhatsApp.
func createWhatsAppGroup(client *whatsmeow.Client, messageStore *MessageStore, req CreateGroupRequest) CreateGroupResponse {
	if !client.IsConnected() {
		return CreateGroupResponse{Success: false, Message: "Not connected to WhatsApp"}
	}
	if strings.TrimSpace(req.Name) == "" {
		return CreateGroupResponse{Success: false, Message: "Group name is required"}
	}
	if len([]rune(req.Name)) > 25 {
		return CreateGroupResponse{Success: false, Message: "Group name must be 25 characters or fewer"}
	}
	if len(req.Participants) == 0 {
		return CreateGroupResponse{Success: false, Message: "At least one participant is required"}
	}

	participantJIDs := make([]types.JID, 0, len(req.Participants))
	for _, p := range req.Participants {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var jid types.JID
		var err error
		if strings.Contains(p, "@") {
			jid, err = types.ParseJID(p)
			if err != nil {
				return CreateGroupResponse{Success: false, Message: fmt.Sprintf("Invalid participant JID %q: %v", p, err)}
			}
		} else {
			jid = types.JID{User: strings.TrimPrefix(p, "+"), Server: "s.whatsapp.net"}
		}
		participantJIDs = append(participantJIDs, jid)
	}
	if len(participantJIDs) == 0 {
		return CreateGroupResponse{Success: false, Message: "No valid participants after parsing"}
	}

	createReq := whatsmeow.ReqCreateGroup{
		Name:         req.Name,
		Participants: participantJIDs,
	}
	if req.IsCommunity {
		createReq.GroupParent.IsParent = true
	}
	if req.CommunityParentJID != "" {
		parentJID, err := types.ParseJID(req.CommunityParentJID)
		if err != nil {
			return CreateGroupResponse{Success: false, Message: fmt.Sprintf("Invalid community_parent_jid: %v", err)}
		}
		createReq.GroupLinkedParent.LinkedParentJID = parentJID
	}

	groupInfo, err := client.CreateGroup(context.Background(), createReq)
	if err != nil {
		return CreateGroupResponse{Success: false, Message: fmt.Sprintf("Error creating group: %v", err)}
	}

	createdAt := groupInfo.GroupCreated
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	if err := messageStore.StoreChat(groupInfo.JID.String(), groupInfo.Name, createdAt); err != nil {
		fmt.Printf("Warning: failed to store newly created group chat: %v\n", err)
	}

	return CreateGroupResponse{
		Success:          true,
		Message:          "Group created",
		JID:              groupInfo.JID.String(),
		Name:             groupInfo.Name,
		ParticipantCount: len(groupInfo.Participants),
	}
}

// leaveWhatsAppGroup leaves the specified group on WhatsApp.
func leaveWhatsAppGroup(client *whatsmeow.Client, jidStr string) LeaveGroupResponse {
	if !client.IsConnected() {
		return LeaveGroupResponse{Success: false, Message: "Not connected to WhatsApp"}
	}
	jidStr = strings.TrimSpace(jidStr)
	if jidStr == "" {
		return LeaveGroupResponse{Success: false, Message: "Group JID is required"}
	}
	jid, err := types.ParseJID(jidStr)
	if err != nil {
		return LeaveGroupResponse{Success: false, Message: fmt.Sprintf("Invalid JID: %v", err)}
	}
	if jid.Server != "g.us" {
		return LeaveGroupResponse{Success: false, Message: "Only group JIDs (@g.us) can be left"}
	}
	if err := client.LeaveGroup(context.Background(), jid); err != nil {
		return LeaveGroupResponse{Success: false, Message: fmt.Sprintf("Error leaving group: %v", err)}
	}
	return LeaveGroupResponse{Success: true, Message: fmt.Sprintf("Left group %s", jid.String())}
}

type DownloadMediaResponse struct {
	Success  bool   `json:"success"`
	Message  string `json:"message"`
	Filename string `json:"filename,omitempty"`
	Path     string `json:"path,omitempty"`
}

// Store additional media info in the database
func (store *MessageStore) StoreMediaInfo(id, chatJID, url string, mediaKey, fileSHA256, fileEncSHA256 []byte, fileLength uint64) error {
	_, err := store.db.Exec(
		"UPDATE messages SET url = ?, media_key = ?, file_sha256 = ?, file_enc_sha256 = ?, file_length = ? WHERE id = ? AND chat_jid = ?",
		url, mediaKey, fileSHA256, fileEncSHA256, fileLength, id, chatJID,
	)
	return err
}

// Get media info from the database
func (store *MessageStore) GetMediaInfo(id, chatJID string) (string, string, string, []byte, []byte, []byte, uint64, error) {
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64

	err := store.db.QueryRow(
		"SELECT media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&mediaType, &filename, &url, &mediaKey, &fileSHA256, &fileEncSHA256, &fileLength)

	return mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err
}

// safeMediaPath builds a media file path inside chatDir, rejecting any message
// ID or filename that could escape the directory via path traversal. Both
// components are partly attacker-influenced (filename comes from the message,
// the retry message ID comes from the phone's response), so they are reduced to
// their base name and checked for separators / dot segments.
func safeMediaPath(chatDir, messageID, filename string) (string, error) {
	// Reject the raw components rather than silently reducing them with
	// filepath.Base — a value containing a separator or dot segment is treated
	// as an attack and surfaced, not sanitized away.
	for _, c := range []string{messageID, filename} {
		if c == "" || c == "." || c == ".." || strings.ContainsAny(c, `/\`) {
			return "", fmt.Errorf("unsafe media path component: %q", c)
		}
	}
	joined := filepath.Join(chatDir, messageID+"_"+filename)
	// Defense in depth: ensure the cleaned result is still under chatDir.
	rel, err := filepath.Rel(chatDir, joined)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("media path escapes chat directory: %q", joined)
	}
	return joined, nil
}

// MediaDownloader implements the whatsmeow.DownloadableMessage interface
type MediaDownloader struct {
	URL           string
	DirectPath    string
	MediaKey      []byte
	FileLength    uint64
	FileSHA256    []byte
	FileEncSHA256 []byte
	MediaType     whatsmeow.MediaType
}

// GetDirectPath implements the DownloadableMessage interface
func (d *MediaDownloader) GetDirectPath() string {
	return d.DirectPath
}

// GetURL implements the DownloadableMessage interface
func (d *MediaDownloader) GetURL() string {
	return d.URL
}

// GetMediaKey implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaKey() []byte {
	return d.MediaKey
}

// GetFileLength implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileLength() uint64 {
	return d.FileLength
}

// GetFileSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileSHA256() []byte {
	return d.FileSHA256
}

// GetFileEncSHA256 implements the DownloadableMessage interface
func (d *MediaDownloader) GetFileEncSHA256() []byte {
	return d.FileEncSHA256
}

// GetMediaType implements the DownloadableMessage interface
func (d *MediaDownloader) GetMediaType() whatsmeow.MediaType {
	return d.MediaType
}

// Function to download media from a message
func downloadMedia(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) (bool, string, string, string, error) {
	// Query the database for the message
	var mediaType, filename, url string
	var mediaKey, fileSHA256, fileEncSHA256 []byte
	var fileLength uint64
	var err error

	// First, check if we already have this file
	chatDir := fmt.Sprintf("store/%s", strings.ReplaceAll(chatJID, ":", "_"))
	localPath := ""

	// Get media info from the database
	mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength, err = messageStore.GetMediaInfo(messageID, chatJID)

	if err != nil {
		// Try to get basic info if extended info isn't available
		err = messageStore.db.QueryRow(
			"SELECT media_type, filename FROM messages WHERE id = ? AND chat_jid = ?",
			messageID, chatJID,
		).Scan(&mediaType, &filename)

		if err != nil {
			return false, "", "", "", fmt.Errorf("failed to find message: %v", err)
		}
	}

	// Check if this is a media message
	if mediaType == "" {
		return false, "", "", "", fmt.Errorf("not a media message")
	}

	// Create directory for the chat if it doesn't exist
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		return false, "", "", "", fmt.Errorf("failed to create chat directory: %v", err)
	}

	// Generate a local path for the file. Prefix with the message ID because the
	// stored filename is derived from sync time and collides across messages
	// received in the same second within a chat (the cache check below would
	// otherwise return the wrong message's bytes).
	localPath, err = safeMediaPath(chatDir, messageID, filename)
	if err != nil {
		return false, "", "", "", err
	}

	// Get absolute path
	absPath, err := filepath.Abs(localPath)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to get absolute path: %v", err)
	}

	// Check if file already exists
	if _, err := os.Stat(localPath); err == nil {
		// File exists, return it
		return true, mediaType, filename, absPath, nil
	}

	// If we don't have all the media info we need, we can't download
	if url == "" || len(mediaKey) == 0 || len(fileSHA256) == 0 || len(fileEncSHA256) == 0 || fileLength == 0 {
		return false, "", "", "", fmt.Errorf("incomplete media information for download")
	}

	fmt.Printf("Attempting to download media for message %s in chat %s...\n", messageID, chatJID)

	// Extract direct path from URL
	directPath := extractDirectPathFromURL(url)

	// Create a downloader that implements DownloadableMessage
	var waMediaType whatsmeow.MediaType
	switch mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		return false, "", "", "", fmt.Errorf("unsupported media type: %s", mediaType)
	}

	downloader := &MediaDownloader{
		URL:           url,
		DirectPath:    directPath,
		MediaKey:      mediaKey,
		FileLength:    fileLength,
		FileSHA256:    fileSHA256,
		FileEncSHA256: fileEncSHA256,
		MediaType:     waMediaType,
	}

	// Download the media using whatsmeow client
	mediaData, err := client.Download(context.Background(), downloader)
	if err != nil {
		return false, "", "", "", fmt.Errorf("failed to download media: %v", err)
	}

	// Save the downloaded media to file
	if err := os.WriteFile(localPath, mediaData, 0644); err != nil {
		return false, "", "", "", fmt.Errorf("failed to save media file: %v", err)
	}

	fmt.Printf("Successfully downloaded %s media to %s (%d bytes)\n", mediaType, absPath, len(mediaData))
	return true, mediaType, filename, absPath, nil
}

// Extract direct path from a WhatsApp media URL
func extractDirectPathFromURL(url string) string {
	// The direct path is typically in the URL, we need to extract it
	// Example URL: https://mmg.whatsapp.net/v/t62.7118-24/13812002_698058036224062_3424455886509161511_n.enc?ccb=11-4&oh=...

	// Find the path part after the domain
	parts := strings.SplitN(url, ".net/", 2)
	if len(parts) < 2 {
		return url // Return original URL if parsing fails
	}

	pathPart := parts[1]

	// Remove query parameters
	pathPart = strings.SplitN(pathPart, "?", 2)[0]

	// Create proper direct path format
	return "/" + pathPart
}

// Start a REST API server to expose the WhatsApp client functionality
func startRESTServer(client *whatsmeow.Client, messageStore *MessageStore, port int) {
	// /qr — serves the current QR code as PNG (during pairing) or a status page (when connected).
	// Open http://localhost:8080/qr in a browser to scan the QR code on first setup.
	http.HandleFunc("/qr", func(w http.ResponseWriter, r *http.Request) {
		qrState.RLock()
		png := qrState.png
		connected := qrState.connected
		qrState.RUnlock()

		if connected {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			fmt.Fprint(w, `<!DOCTYPE html><html><body style="font-family:sans-serif;text-align:center;padding:4rem">
<h2 style="color:#25d366">✓ WhatsApp connected</h2>
<p>You can close this tab.</p></body></html>`)
			return
		}
		if png == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("Refresh", "2")
			fmt.Fprint(w, `<!DOCTYPE html><html><body style="font-family:sans-serif;text-align:center;padding:4rem">
<h2>Waiting for QR code…</h2><p>This page refreshes automatically.</p></body></html>`)
			return
		}
		// Serve an auto-refreshing HTML page that embeds the QR as a data URI.
		// Refreshes every 20 s so a new QR is shown if the first one expires.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html><html><head>
<meta http-equiv="refresh" content="20">
<style>body{font-family:sans-serif;text-align:center;padding:2rem;background:#f0f0f0}
img{border:8px solid white;border-radius:8px;box-shadow:0 4px 20px rgba(0,0,0,.2)}</style>
</head><body>
<h2>Scan with WhatsApp to connect</h2>
<p>Open WhatsApp → Settings → Linked Devices → Link a Device</p>
<img src="/qr.png" width="300" height="300" alt="QR Code">
<p style="color:#888;font-size:.85rem">Page refreshes every 20 s</p>
</body></html>`)
	})

	// /qr.png — raw PNG for embedding or direct download
	http.HandleFunc("/qr.png", func(w http.ResponseWriter, r *http.Request) {
		qrState.RLock()
		png := qrState.png
		qrState.RUnlock()
		if png == nil {
			http.Error(w, "QR not available", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(png)
	})

	// Handler for sending messages
	http.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req SendMessageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.Recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}

		if req.Message == "" && req.MediaPath == "" {
			http.Error(w, "Message or media path is required", http.StatusBadRequest)
			return
		}

		fmt.Println("Received request to send message", req.Message, req.MediaPath)

		// Send the message
		success, message := sendWhatsAppMessage(client, messageStore, req.Recipient, req.Message, req.MediaPath)
		fmt.Println("Message sent", success, message)
		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Set appropriate status code
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}

		// Send response
		json.NewEncoder(w).Encode(SendMessageResponse{
			Success: success,
			Message: message,
		})
	})

	// Handler for downloading media
	http.HandleFunc("/api/download", func(w http.ResponseWriter, r *http.Request) {
		// Only allow POST requests
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Parse the request body
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}

		// Validate request
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}

		// Download the media
		success, mediaType, filename, path, err := downloadMedia(client, messageStore, req.MessageID, req.ChatJID)

		// Set response headers
		w.Header().Set("Content-Type", "application/json")

		// Handle download result
		if !success || err != nil {
			errMsg := "Unknown error"
			if err != nil {
				errMsg = err.Error()
			}

			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(DownloadMediaResponse{
				Success: false,
				Message: fmt.Sprintf("Failed to download media: %s", errMsg),
			})
			return
		}

		// Send successful response
		json.NewEncoder(w).Encode(DownloadMediaResponse{
			Success:  true,
			Message:  fmt.Sprintf("Successfully downloaded %s media", mediaType),
			Filename: filename,
			Path:     path,
		})
	})

	// Handler for requesting media retry (re-upload of expired media from the phone)
	http.HandleFunc("/api/mediaretry", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req DownloadMediaRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := requestMediaRetry(client, messageStore, req.MessageID, req.ChatJID); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": false, "message": fmt.Sprintf("Failed to request media retry: %s", err.Error()),
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "message": "Media retry requested; watch bridge log for response",
		})
	})

	// Handler for creating a group
	http.HandleFunc("/api/create_group", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req CreateGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		fmt.Printf("Received request to create group %q with %d participants\n", req.Name, len(req.Participants))
		resp := createWhatsAppGroup(client, messageStore, req)
		w.Header().Set("Content-Type", "application/json")
		if !resp.Success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(resp)
	})

	// Handler for getting group info (name + participants)
	http.HandleFunc("/api/group_info", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		jid, err := types.ParseJID(r.URL.Query().Get("jid"))
		if err != nil {
			http.Error(w, "Invalid JID", http.StatusBadRequest)
			return
		}
		if client == nil || !client.IsConnected() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": "WhatsApp client not connected"})
			return
		}
		groupInfo, err := client.GetGroupInfo(context.Background(), jid)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		participants := make([]map[string]string, 0, len(groupInfo.Participants))
		for _, p := range groupInfo.Participants {
			participants = append(participants, map[string]string{
				"jid":          p.JID.String(),
				"phone_number": p.PhoneNumber.User,
				"lid":          p.LID.String(),
				"display_name": p.DisplayName,
			})
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": true, "name": groupInfo.Name, "participants": participants,
		})
	})

	// Handler for leaving a group
	http.HandleFunc("/api/leave_group", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req LeaveGroupRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		fmt.Printf("Received request to leave group %s\n", req.JID)
		resp := leaveWhatsAppGroup(client, req.JID)
		w.Header().Set("Content-Type", "application/json")
		if !resp.Success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(resp)
	})

	// Handler for marking a chat as read
	http.HandleFunc("/api/mark_chat_read", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req MarkChatReadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" {
			http.Error(w, "Invalid request: chat_jid is required", http.StatusBadRequest)
			return
		}
		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid chat_jid: %v", err), http.StatusBadRequest)
			return
		}
		var senderJID types.JID
		if req.SenderJID != "" {
			senderJID, err = types.ParseJID(req.SenderJID)
			if err != nil {
				http.Error(w, fmt.Sprintf("Invalid sender_jid: %v", err), http.StatusBadRequest)
				return
			}
		}
		ts := time.Now()
		if req.Timestamp > 0 {
			ts = time.Unix(req.Timestamp, 0)
		}
		if client == nil || !client.IsConnected() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(MarkChatResponse{Success: false, Message: "WhatsApp client not connected"})
			return
		}
		ctx := context.Background()
		// Send read receipts to the sender(s)
		if len(req.MessageIDs) > 0 {
			if err := client.MarkRead(ctx, req.MessageIDs, ts, chatJID, senderJID); err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				json.NewEncoder(w).Encode(MarkChatResponse{Success: false, Message: fmt.Sprintf("MarkRead error: %v", err)})
				return
			}
		}
		// Sync read state via app state so the unread badge clears on all devices
		if err := safeSendAppState(client, ctx, appstate.BuildMarkChatAsRead(chatJID, true, ts, nil)); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(MarkChatResponse{Success: false, Message: fmt.Sprintf("AppState error: %v", err)})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MarkChatResponse{Success: true, Message: fmt.Sprintf("Chat %s marked as read", req.ChatJID)})
	})

	// Handler for marking a chat as unread
	http.HandleFunc("/api/mark_chat_unread", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req MarkChatUnreadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" {
			http.Error(w, "Invalid request: chat_jid required", http.StatusBadRequest)
			return
		}
		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid chat_jid: %v", err), http.StatusBadRequest)
			return
		}
		if client == nil || !client.IsConnected() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(MarkChatResponse{Success: false, Message: "WhatsApp client not connected"})
			return
		}
		ctx := context.Background()
		if err := safeSendAppState(client, ctx, appstate.BuildMarkChatAsRead(chatJID, false, time.Time{}, nil)); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(MarkChatResponse{Success: false, Message: fmt.Sprintf("AppState error: %v", err)})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MarkChatResponse{Success: true, Message: fmt.Sprintf("Chat %s marked as unread", req.ChatJID)})
	})

	// Handler for archiving / unarchiving a chat
	http.HandleFunc("/api/archive_chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req ArchiveChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, fmt.Sprintf("Invalid request format: %v", err), http.StatusBadRequest)
			return
		}
		if req.ChatJID == "" {
			http.Error(w, "Invalid request: chat_jid required", http.StatusBadRequest)
			return
		}
		if req.Archive == nil {
			http.Error(w, "Invalid request: archive (true|false) required", http.StatusBadRequest)
			return
		}
		chatJID, err := types.ParseJID(req.ChatJID)
		if err != nil {
			http.Error(w, fmt.Sprintf("Invalid chat_jid: %v", err), http.StatusBadRequest)
			return
		}
		if client == nil || !client.IsConnected() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(MarkChatResponse{Success: false, Message: "WhatsApp client not connected"})
			return
		}
		ctx := context.Background()
		// last message timestamp/key are optional; zero values are accepted by BuildArchive.
		if err := safeSendAppState(client, ctx, appstate.BuildArchive(chatJID, *req.Archive, time.Time{}, nil)); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(MarkChatResponse{Success: false, Message: fmt.Sprintf("AppState error: %v", err)})
			return
		}
		action := "archived"
		if !*req.Archive {
			action = "unarchived"
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(MarkChatResponse{Success: true, Message: fmt.Sprintf("Chat %s %s", req.ChatJID, action)})
	})

	// Bind to loopback only — no auth on REST API, anyone on LAN could send messages.
	// Set BIND_ADDR=0.0.0.0 to opt into LAN exposure.
	bindAddr := os.Getenv("BIND_ADDR")
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	serverAddr := fmt.Sprintf("%s:%d", bindAddr, port)
	fmt.Printf("Starting REST API server on %s...\n", serverAddr)

	// Run server in a goroutine so it doesn't block
	go func() {
		if err := http.ListenAndServe(serverAddr, nil); err != nil {
			fmt.Printf("REST API server error: %v\n", err)
		}
	}()
}

func main() {
	// Set up logger
	logger := waLog.Stdout("Client", "INFO", true)
	logger.Infof("Starting WhatsApp client...")

	// Create database connection for storing session data
	dbLog := waLog.Stdout("Database", "INFO", true)

	// Create directory for database if it doesn't exist
	if err := os.MkdirAll("store", 0755); err != nil {
		logger.Errorf("Failed to create store directory: %v", err)
		return
	}

	container, err := sqlstore.New(context.Background(), "sqlite3", "file:store/whatsapp.db?_foreign_keys=on", dbLog)
	if err != nil {
		logger.Errorf("Failed to connect to database: %v", err)
		return
	}

	// Get device store - This contains session information
	deviceStore, err := container.GetFirstDevice(context.Background())
	if err != nil {
		if err == sql.ErrNoRows {
			// No device exists, create one
			deviceStore = container.NewDevice()
			logger.Infof("Created new device")
		} else {
			logger.Errorf("Failed to get device: %v", err)
			return
		}
	}

	// Request a full history sync on pairing (only fires on a fresh QR scan).
	// WhatsApp delivers up to this window as events.HistorySync after login.
	store.DeviceProps.RequireFullSync = proto.Bool(true)
	store.DeviceProps.HistorySyncConfig = &waCompanionReg.DeviceProps_HistorySyncConfig{
		FullSyncDaysLimit:   proto.Uint32(365),
		FullSyncSizeMbLimit: proto.Uint32(10240),
		StorageQuotaMb:      proto.Uint32(10240),
	}

	// Create client instance
	client := whatsmeow.NewClient(deviceStore, logger)
	if client == nil {
		logger.Errorf("Failed to create WhatsApp client")
		return
	}

	// Initialize message store
	messageStore, err := NewMessageStore()
	if err != nil {
		logger.Errorf("Failed to initialize message store: %v", err)
		return
	}
	defer messageStore.Close()

	// Setup event handling for messages and history sync
	client.AddEventHandler(func(evt interface{}) {
		switch v := evt.(type) {
		case *events.Message:
			// Process regular messages
			handleMessage(client, messageStore, v, logger)

		case *events.HistorySync:
			// Process history sync events
			handleHistorySync(client, messageStore, v, logger)

		case *events.MediaRetry:
			// Phone responded to a media retry request with a (hopefully) fresh
			// directPath. Handle off the event loop — it does a synchronous
			// download + disk write, and whatsmeow serializes event callbacks,
			// so running it inline would stall all other events during a
			// recover_audios.py flood.
			go handleMediaRetry(client, messageStore, v, logger)

		case *events.Connected:
			logger.Infof("Connected to WhatsApp")
			go SyncAllContacts(client, messageStore, logger)
			sweepOnce.Do(func() { startTranscriptionSweep(5 * time.Minute) })

		case *events.LoggedOut:
			logger.Warnf("Device logged out, please scan QR code to log in again")
		}
	})

	// Create channel to track connection success
	connected := make(chan bool, 1)

	// Start REST API server early so /qr is available during the QR pairing flow.
	bridgePort := 8080
	if portStr := os.Getenv("WHATSAPP_BRIDGE_PORT"); portStr != "" {
		if p, err := fmt.Sscanf(portStr, "%d", &bridgePort); p != 1 || err != nil {
			bridgePort = 8080
		}
	}
	startRESTServer(client, messageStore, bridgePort)

	// Connect to WhatsApp
	if client.Store.ID == nil {
		// No ID stored, this is a new client, need to pair with phone
		qrChan, _ := client.GetQRChannel(context.Background())
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}

		fmt.Printf("\nOpen http://localhost:%d/qr in your browser to scan the QR code.\n", bridgePort)

		// Print QR code for pairing with phone
		for evt := range qrChan {
			if evt.Event == "code" {
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)

				// Generate PNG and store in memory for /qr endpoint.
				if pngBytes, err := goqr.Encode(evt.Code, goqr.Medium, 512); err == nil {
					qrState.Lock()
					qrState.png = pngBytes
					qrState.connected = false
					qrState.Unlock()
				}

				// Also save to disk for convenience.
				qrFile := "/tmp/whatsapp-qr.png"
				if err := goqr.WriteFile(evt.Code, goqr.Medium, 512, qrFile); err == nil {
					fmt.Printf("\nQR also saved as image: %s\n", qrFile)
					_ = exec.Command("open", qrFile).Start()
				}
			} else if evt.Event == "success" {
				qrState.Lock()
				qrState.png = nil
				qrState.connected = true
				qrState.Unlock()
				connected <- true
				break
			}
		}

		// Wait for connection
		select {
		case <-connected:
			fmt.Println("\nSuccessfully connected and authenticated!")
		case <-time.After(3 * time.Minute):
			logger.Errorf("Timeout waiting for QR code scan")
			return
		}
	} else {
		// Already logged in, just connect
		err = client.Connect()
		if err != nil {
			logger.Errorf("Failed to connect: %v", err)
			return
		}
		qrState.Lock()
		qrState.connected = true
		qrState.Unlock()
		connected <- true
	}

	// Wait a moment for connection to stabilize
	time.Sleep(2 * time.Second)

	if !client.IsConnected() {
		logger.Errorf("Failed to establish stable connection")
		return
	}

	fmt.Println("\n✓ Connected to WhatsApp! Type 'help' for commands.")

	// Merge any chats stored under LID JIDs into their PN equivalents.
	migrateLIDChats(client, messageStore, logger)

	// Create a channel to keep the main goroutine alive
	exitChan := make(chan os.Signal, 1)
	signal.Notify(exitChan, syscall.SIGINT, syscall.SIGTERM)

	fmt.Println("REST server is running. Press Ctrl+C to disconnect and exit.")

	// Wait for termination signal
	<-exitChan

	fmt.Println("Disconnecting...")
	// Disconnect client
	client.Disconnect()
}

// GetChatName determines the appropriate name for a chat based on JID and other info
func GetChatName(client *whatsmeow.Client, messageStore *MessageStore, jid types.JID, chatJID string, conversation interface{}, sender string, logger waLog.Logger) string {
	// First, check if chat already exists in database with a name
	var existingName string
	err := messageStore.db.QueryRow("SELECT name FROM chats WHERE jid = ?", chatJID).Scan(&existingName)
	if err == nil && existingName != "" {
		// Chat exists with a name, use that
		logger.Infof("Using existing chat name for %s: %s", chatJID, existingName)
		return existingName
	}

	// Need to determine chat name
	var name string

	if jid.Server == "g.us" {
		// This is a group chat
		logger.Infof("Getting name for group: %s", chatJID)

		// Use conversation data if provided (from history sync)
		if conversation != nil {
			// Extract name from conversation if available
			// This uses type assertions to handle different possible types
			var displayName, convName *string
			// Try to extract the fields we care about regardless of the exact type
			v := reflect.ValueOf(conversation)
			if v.Kind() == reflect.Ptr && !v.IsNil() {
				v = v.Elem()

				// Try to find DisplayName field
				if displayNameField := v.FieldByName("DisplayName"); displayNameField.IsValid() && displayNameField.Kind() == reflect.Ptr && !displayNameField.IsNil() {
					dn := displayNameField.Elem().String()
					displayName = &dn
				}

				// Try to find Name field
				if nameField := v.FieldByName("Name"); nameField.IsValid() && nameField.Kind() == reflect.Ptr && !nameField.IsNil() {
					n := nameField.Elem().String()
					convName = &n
				}
			}

			// Use the name we found
			if displayName != nil && *displayName != "" {
				name = *displayName
			} else if convName != nil && *convName != "" {
				name = *convName
			}
		}

		// If we didn't get a name, try group info
		if name == "" {
			groupInfo, err := client.GetGroupInfo(context.Background(), jid)
			if err == nil && groupInfo.Name != "" {
				name = groupInfo.Name
			} else {
				// Fallback name for groups
				name = fmt.Sprintf("Group %s", jid.User)
			}
		}

		logger.Infof("Using group name: %s", name)
	} else {
		// This is an individual contact
		logger.Infof("Getting name for contact: %s", chatJID)

		// Try senders table first (populated from every message event and SyncAllContacts)
		if resolved := messageStore.ResolveName(chatJID); resolved != "" {
			name = resolved
		} else {
			contact, err := client.Store.Contacts.GetContact(context.Background(), jid)
			if err == nil && contact.FullName != "" {
				name = contact.FullName
			} else if err == nil && contact.BusinessName != "" {
				name = contact.BusinessName
			} else if err == nil && contact.PushName != "" {
				name = contact.PushName
			} else if sender != "" {
				name = sender
			} else {
				name = jid.User
			}
		}

		logger.Infof("Using contact name: %s", name)
	}

	return name
}

// Handle history sync events
func handleHistorySync(client *whatsmeow.Client, messageStore *MessageStore, historySync *events.HistorySync, logger waLog.Logger) {
	fmt.Printf("Received history sync event with %d conversations\n", len(historySync.Data.Conversations))

	syncedCount := 0
	for _, conversation := range historySync.Data.Conversations {
		// Parse JID from the conversation
		if conversation.ID == nil {
			continue
		}

		chatJID := *conversation.ID

		// Try to parse the JID
		jid, err := types.ParseJID(chatJID)
		if err != nil {
			logger.Warnf("Failed to parse JID %s: %v", chatJID, err)
			continue
		}

		// Normalize LID -> PN at write time
		jid = resolveToPN(client, jid)
		chatJID = jid.String()

		// Get appropriate chat name by passing the history sync conversation directly
		name := GetChatName(client, messageStore, jid, chatJID, conversation, "", logger)

		// Process messages
		messages := conversation.Messages
		if len(messages) > 0 {
			// Update chat with latest message timestamp
			latestMsg := messages[0]
			if latestMsg == nil || latestMsg.Message == nil {
				continue
			}

			// Get timestamp from message info
			timestamp := time.Time{}
			if ts := latestMsg.Message.GetMessageTimestamp(); ts != 0 {
				timestamp = time.Unix(int64(ts), 0)
			} else {
				continue
			}

			messageStore.StoreChat(chatJID, name, timestamp)

			// Store messages
			for _, msg := range messages {
				if msg == nil || msg.Message == nil {
					continue
				}

				// Extract text content (includes media captions)
				content := extractTextContent(msg.Message.Message)

				// Extract media info
				var mediaType, filename, url string
				var mediaKey, fileSHA256, fileEncSHA256 []byte
				var fileLength uint64

				if msg.Message.Message != nil {
					mediaType, filename, url, mediaKey, fileSHA256, fileEncSHA256, fileLength = extractMediaInfo(msg.Message.Message)
				}

				// Log the message content for debugging
				logger.Infof("Message content: %v, Media Type: %v", content, mediaType)

				// Skip messages with no content and no media
				if content == "" && mediaType == "" {
					continue
				}

				// Determine sender
				var sender string
				isFromMe := false
				if msg.Message.Key != nil {
					if msg.Message.Key.FromMe != nil {
						isFromMe = *msg.Message.Key.FromMe
					}
					if !isFromMe && msg.Message.Key.Participant != nil && *msg.Message.Key.Participant != "" {
						if pjid, perr := types.ParseJID(*msg.Message.Key.Participant); perr == nil {
							sender = resolveToPN(client, pjid).User
						} else {
							sender = *msg.Message.Key.Participant
						}
					} else if isFromMe {
						sender = client.Store.ID.User
					} else {
						sender = jid.User
					}
				} else {
					sender = jid.User
				}

				// Store message
				msgID := ""
				if msg.Message.Key != nil && msg.Message.Key.ID != nil {
					msgID = *msg.Message.Key.ID
				}

				// Get message timestamp
				timestamp := time.Time{}
				if ts := msg.Message.GetMessageTimestamp(); ts != 0 {
					timestamp = time.Unix(int64(ts), 0)
				} else {
					continue
				}

				err = messageStore.StoreMessage(
					msgID,
					chatJID,
					sender,
					content,
					timestamp,
					isFromMe,
					mediaType,
					filename,
					url,
					mediaKey,
					fileSHA256,
					fileEncSHA256,
					fileLength,
				)
				if err != nil {
					logger.Warnf("Failed to store history message: %v", err)
				} else {
					syncedCount++
					// Log successful message storage
					if mediaType != "" {
						logger.Infof("Stored message: [%s] %s -> %s: [%s: %s] %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, mediaType, filename, content)
					} else {
						logger.Infof("Stored message: [%s] %s -> %s: %s",
							timestamp.Format("2006-01-02 15:04:05"), sender, chatJID, content)
					}
				}
			}
		}
	}

	fmt.Printf("History sync complete. Stored %d messages.\n", syncedCount)
	SyncAllContacts(client, messageStore, logger)
}

// Request history sync from the server
func requestHistorySync(client *whatsmeow.Client) {
	if client == nil {
		fmt.Println("Client is not initialized. Cannot request history sync.")
		return
	}

	if !client.IsConnected() {
		fmt.Println("Client is not connected. Please ensure you are connected to WhatsApp first.")
		return
	}

	if client.Store.ID == nil {
		fmt.Println("Client is not logged in. Please scan the QR code first.")
		return
	}

	// Build and send a history sync request
	historyMsg := client.BuildHistorySyncRequest(nil, 100)
	if historyMsg == nil {
		fmt.Println("Failed to build history sync request.")
		return
	}

	_, err := client.SendMessage(context.Background(), types.JID{
		Server: "s.whatsapp.net",
		User:   "status",
	}, historyMsg)

	if err != nil {
		fmt.Printf("Failed to request history sync: %v\n", err)
	} else {
		fmt.Println("History sync requested. Waiting for server response...")
	}
}

// sweepOnce guards against launching the transcription ticker more than once,
// since events.Connected fires on every reconnect.
var sweepOnce sync.Once

// startTranscriptionSweep periodically shells out to the Python transcriber to
// turn newly-arrived audio messages into searchable text. Whisper runs in a
// separate process so it never blocks the bridge's message handling. A lockfile
// prevents overlapping runs if a sweep outlasts the interval (e.g. a backlog
// after downtime).
func startTranscriptionSweep(interval time.Duration) {
	pyDir, err := filepath.Abs("../whatsapp-mcp-server")
	if err != nil {
		fmt.Printf("transcription sweep disabled: %v\n", err)
		return
	}
	python := filepath.Join(pyDir, ".venv", "bin", "python3")
	script := filepath.Join(pyDir, "transcribe.py")
	lockPath := filepath.Join(os.TempDir(), "wa_transcribe.lock")

	if _, err := os.Stat(python); err != nil {
		fmt.Printf("transcription sweep disabled: python not found at %s\n", python)
		return
	}
	if _, err := os.Stat(script); err != nil {
		fmt.Printf("transcription sweep disabled: script not found at %s\n", script)
		return
	}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			// Drop media-retry entries the phone never answered, so unused
			// decryption keys don't accumulate in memory across a long run.
			mediaRetryCache.evictOlderThan(30*time.Minute, time.Now())

			// Skip if a previous sweep is still running.
			if data, err := os.ReadFile(lockPath); err == nil {
				if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
					if proc, ferr := os.FindProcess(pid); ferr == nil && proc.Signal(syscall.Signal(0)) == nil {
						continue // prior sweep alive
					}
				}
			}
			cmd := exec.Command(python, script)
			cmd.Dir = pyDir
			// Surface the transcriber's output (its DONE summary, per-audio
			// errors, the oversized-audio RuntimeError) in the bridge log
			// instead of discarding it to /dev/null.
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			// Point transcribe.py at THIS bridge's REST port. Without this it
			// defaults to :8080 and every download fails when the bridge runs
			// on a non-default port. A pre-set WHATSAPP_API_BASE_URL wins.
			cmd.Env = os.Environ()
			if os.Getenv("WHATSAPP_API_BASE_URL") == "" {
				port := "8080"
				if p := os.Getenv("WHATSAPP_BRIDGE_PORT"); p != "" {
					port = p
				}
				cmd.Env = append(cmd.Env, fmt.Sprintf("WHATSAPP_API_BASE_URL=http://localhost:%s/api", port))
			}
			if err := cmd.Start(); err != nil {
				fmt.Printf("transcription sweep: failed to start: %v\n", err)
				continue
			}
			// The lockfile is the only overlap guard; if we can't write it,
			// don't leave a process running unguarded — kill it and retry next tick.
			if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0644); err != nil {
				fmt.Printf("transcription sweep: cannot write lockfile (%v); killing sweep to preserve overlap guard\n", err)
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				continue
			}
			go func(c *exec.Cmd) {
				if err := c.Wait(); err != nil {
					fmt.Printf("transcription sweep: transcribe.py exited with error: %v\n", err)
				}
				_ = os.Remove(lockPath)
			}(cmd)
		}
	}()
	fmt.Printf("Transcription sweep started (every %s)\n", interval)
}

// mediaRetryEntry holds the info needed to decrypt + download a media retry
// response. It is keyed by message ID inside mediaRetryCache. The four crypto
// fields (mediaKey, fileSHA256, fileEncSHA256, fileLength) plus mediaType are
// consumed together by DownloadMediaWithPath and must travel as a set.
type mediaRetryEntry struct {
	chatJID       string
	mediaKey      []byte
	fileSHA256    []byte
	fileEncSHA256 []byte
	fileLength    uint64
	mediaType     string
	filename      string
	storedAt      time.Time
}

// retryCache maps message ID -> pending retry entry. All access goes through its
// methods so the map is never touched without the lock, and consume() evicts on
// read so entries (which pin a decryption key in memory) don't accumulate.
type retryCache struct {
	mu sync.Mutex // guards m
	m  map[string]mediaRetryEntry
}

func (c *retryCache) store(id string, e mediaRetryEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[id] = e
}

// consume returns the entry for id and removes it, so a retry response is
// handled at most once and the key material is freed.
func (c *retryCache) consume(id string) (mediaRetryEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[id]
	if ok {
		delete(c.m, id)
	}
	return e, ok
}

// evictOlderThan drops entries the phone never answered, so a media-retry
// request that gets no response doesn't leak its key material forever.
func (c *retryCache) evictOlderThan(maxAge time.Duration, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, e := range c.m {
		if now.Sub(e.storedAt) > maxAge {
			delete(c.m, id)
		}
	}
}

var mediaRetryCache = &retryCache{m: make(map[string]mediaRetryEntry)}

// requestMediaRetry asks the phone to re-upload media whose CDN reference has
// expired (download returns 403). The phone responds with an events.MediaRetry
// carrying a fresh directPath, handled by handleMediaRetry.
func requestMediaRetry(client *whatsmeow.Client, messageStore *MessageStore, messageID, chatJID string) error {
	mediaType, filename, _, mediaKey, fileSHA256, fileEncSHA256, fileLength, err := messageStore.GetMediaInfo(messageID, chatJID)
	if err != nil {
		return fmt.Errorf("failed to get media info: %v", err)
	}
	if len(mediaKey) == 0 {
		return fmt.Errorf("no media key for message")
	}

	jid, err := types.ParseJID(chatJID)
	if err != nil {
		return fmt.Errorf("invalid chat jid: %v", err)
	}

	// Read sender + direction: groups require the participant JID in the retry
	// receipt, and from-me messages must be flagged correctly.
	var sender string
	var isFromMe bool
	err = messageStore.db.QueryRow(
		"SELECT sender, is_from_me FROM messages WHERE id = ? AND chat_jid = ?",
		messageID, chatJID,
	).Scan(&sender, &isFromMe)
	if err != nil {
		return fmt.Errorf("failed to read message sender: %v", err)
	}

	isGroup := jid.Server == types.GroupServer
	senderJID := jid
	if isGroup && sender != "" {
		senderJID = types.JID{User: sender, Server: types.DefaultUserServer}
	}

	mediaRetryCache.store(messageID, mediaRetryEntry{
		chatJID: chatJID, mediaKey: mediaKey, fileSHA256: fileSHA256,
		fileEncSHA256: fileEncSHA256, fileLength: fileLength,
		mediaType: mediaType, filename: filename, storedAt: time.Now(),
	})

	info := &types.MessageInfo{
		ID: messageID,
		MessageSource: types.MessageSource{
			Chat:     jid,
			Sender:   senderJID,
			IsFromMe: isFromMe,
			IsGroup:  isGroup,
		},
	}
	return client.SendMediaRetryReceipt(context.Background(), info, mediaKey)
}

// handleMediaRetry processes the phone's response to a media retry request: on
// success it downloads with the fresh directPath and persists the file so the
// normal download/transcription path can use it.
// Stable log contract consumed by recover_audios.py. Every terminal outcome
// emits exactly one of these tags so the recovery orchestrator can classify it
// without guessing — keep these in sync with the regexes in recover_audios.py.
//   MEDIA RETRY <id>: SUCCESS recovered <n> bytes -> <path>
//   MEDIA RETRY <id>: NOTONPHONE <result>   (phone no longer has the file)
//   MEDIA RETRY <id>: ERROR <reason>        (terminal local/decrypt failure)
func handleMediaRetry(client *whatsmeow.Client, messageStore *MessageStore, evt *events.MediaRetry, logger waLog.Logger) {
	// consume() evicts the entry so a duplicate response can't re-run the
	// download and the key material is freed on every path below.
	entry, ok := mediaRetryCache.consume(evt.MessageID)
	if !ok {
		logger.Warnf("media retry response for unknown message %s", evt.MessageID)
		return
	}

	retryData, err := whatsmeow.DecryptMediaRetryNotification(evt, entry.mediaKey)
	if err != nil {
		fmt.Printf("MEDIA RETRY %s: ERROR decrypt failed: %v\n", evt.MessageID, err)
		return
	}
	if retryData.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS {
		// Phone-side result (NOT_FOUND etc.) — the file is gone from the phone.
		fmt.Printf("MEDIA RETRY %s: NOTONPHONE %s\n", evt.MessageID, retryData.GetResult())
		return
	}

	var waMediaType whatsmeow.MediaType
	switch entry.mediaType {
	case "image":
		waMediaType = whatsmeow.MediaImage
	case "video":
		waMediaType = whatsmeow.MediaVideo
	case "audio":
		waMediaType = whatsmeow.MediaAudio
	case "document":
		waMediaType = whatsmeow.MediaDocument
	default:
		fmt.Printf("MEDIA RETRY %s: ERROR unsupported media type %q\n", evt.MessageID, entry.mediaType)
		return
	}

	newPath := retryData.GetDirectPath()
	data, err := client.DownloadMediaWithPath(context.Background(), newPath,
		entry.fileEncSHA256, entry.fileSHA256, entry.mediaKey,
		int(entry.fileLength), waMediaType, "")
	if err != nil {
		fmt.Printf("MEDIA RETRY %s: ERROR download with fresh path failed: %v\n", evt.MessageID, err)
		return
	}

	chatDir := fmt.Sprintf("store/%s", strings.ReplaceAll(entry.chatJID, ":", "_"))
	if err := os.MkdirAll(chatDir, 0755); err != nil {
		fmt.Printf("MEDIA RETRY %s: ERROR mkdir failed: %v\n", evt.MessageID, err)
		return
	}
	localPath, err := safeMediaPath(chatDir, evt.MessageID, entry.filename)
	if err != nil {
		fmt.Printf("MEDIA RETRY %s: ERROR %v\n", evt.MessageID, err)
		return
	}
	if err := os.WriteFile(localPath, data, 0644); err != nil {
		fmt.Printf("MEDIA RETRY %s: ERROR write failed: %v\n", evt.MessageID, err)
		return
	}
	fmt.Printf("MEDIA RETRY %s: SUCCESS recovered %d bytes -> %s\n", evt.MessageID, len(data), localPath)
}

// analyzeOggOpus tries to extract duration and generate a simple waveform from an Ogg Opus file
func analyzeOggOpus(data []byte) (duration uint32, waveform []byte, err error) {
	// Try to detect if this is a valid Ogg file by checking for the "OggS" signature
	// at the beginning of the file
	if len(data) < 4 || string(data[0:4]) != "OggS" {
		return 0, nil, fmt.Errorf("not a valid Ogg file (missing OggS signature)")
	}

	// Parse Ogg pages to find the last page with a valid granule position
	var lastGranule uint64
	var sampleRate uint32 = 48000 // Default Opus sample rate
	var preSkip uint16 = 0
	var foundOpusHead bool

	// Scan through the file looking for Ogg pages
	for i := 0; i < len(data); {
		// Check if we have enough data to read Ogg page header
		if i+27 >= len(data) {
			break
		}

		// Verify Ogg page signature
		if string(data[i:i+4]) != "OggS" {
			// Skip until next potential page
			i++
			continue
		}

		// Extract header fields
		granulePos := binary.LittleEndian.Uint64(data[i+6 : i+14])
		pageSeqNum := binary.LittleEndian.Uint32(data[i+18 : i+22])
		numSegments := int(data[i+26])

		// Extract segment table
		if i+27+numSegments >= len(data) {
			break
		}
		segmentTable := data[i+27 : i+27+numSegments]

		// Calculate page size
		pageSize := 27 + numSegments
		for _, segLen := range segmentTable {
			pageSize += int(segLen)
		}

		// Check if we're looking at an OpusHead packet (should be in first few pages)
		if !foundOpusHead && pageSeqNum <= 1 {
			// Look for "OpusHead" marker in this page
			pageData := data[i : i+pageSize]
			headPos := bytes.Index(pageData, []byte("OpusHead"))
			if headPos >= 0 && headPos+12 < len(pageData) {
				// Found OpusHead, extract sample rate and pre-skip
				// OpusHead format: Magic(8) + Version(1) + Channels(1) + PreSkip(2) + SampleRate(4) + ...
				headPos += 8 // Skip "OpusHead" marker
				// PreSkip is 2 bytes at offset 10
				if headPos+12 <= len(pageData) {
					preSkip = binary.LittleEndian.Uint16(pageData[headPos+10 : headPos+12])
					sampleRate = binary.LittleEndian.Uint32(pageData[headPos+12 : headPos+16])
					foundOpusHead = true
					fmt.Printf("Found OpusHead: sampleRate=%d, preSkip=%d\n", sampleRate, preSkip)
				}
			}
		}

		// Keep track of last valid granule position
		if granulePos != 0 {
			lastGranule = granulePos
		}

		// Move to next page
		i += pageSize
	}

	if !foundOpusHead {
		fmt.Println("Warning: OpusHead not found, using default values")
	}

	// Calculate duration based on granule position
	if lastGranule > 0 {
		// Formula for duration: (lastGranule - preSkip) / sampleRate
		durationSeconds := float64(lastGranule-uint64(preSkip)) / float64(sampleRate)
		duration = uint32(math.Ceil(durationSeconds))
		fmt.Printf("Calculated Opus duration from granule: %f seconds (lastGranule=%d)\n",
			durationSeconds, lastGranule)
	} else {
		// Fallback to rough estimation if granule position not found
		fmt.Println("Warning: No valid granule position found, using estimation")
		durationEstimate := float64(len(data)) / 2000.0 // Very rough approximation
		duration = uint32(durationEstimate)
	}

	// Make sure we have a reasonable duration (at least 1 second, at most 300 seconds)
	if duration < 1 {
		duration = 1
	} else if duration > 300 {
		duration = 300
	}

	// Generate waveform
	waveform = placeholderWaveform(duration)

	fmt.Printf("Ogg Opus analysis: size=%d bytes, calculated duration=%d sec, waveform=%d bytes\n",
		len(data), duration, len(waveform))

	return duration, waveform, nil
}

// min returns the smaller of x or y
func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

// placeholderWaveform generates a synthetic waveform for WhatsApp voice messages
// that appears natural with some variability based on the duration
func placeholderWaveform(duration uint32) []byte {
	// WhatsApp expects a 64-byte waveform for voice messages
	const waveformLength = 64
	waveform := make([]byte, waveformLength)

	// Seed the random number generator for consistent results with the same duration
	rand.Seed(int64(duration))

	// Create a more natural looking waveform with some patterns and variability
	// rather than completely random values

	// Base amplitude and frequency - longer messages get faster frequency
	baseAmplitude := 35.0
	frequencyFactor := float64(min(int(duration), 120)) / 30.0

	for i := range waveform {
		// Position in the waveform (normalized 0-1)
		pos := float64(i) / float64(waveformLength)

		// Create a wave pattern with some randomness
		// Use multiple sine waves of different frequencies for more natural look
		val := baseAmplitude * math.Sin(pos*math.Pi*frequencyFactor*8)
		val += (baseAmplitude / 2) * math.Sin(pos*math.Pi*frequencyFactor*16)

		// Add some randomness to make it look more natural
		val += (rand.Float64() - 0.5) * 15

		// Add some fade-in and fade-out effects
		fadeInOut := math.Sin(pos * math.Pi)
		val = val * (0.7 + 0.3*fadeInOut)

		// Center around 50 (typical voice baseline)
		val = val + 50

		// Ensure values stay within WhatsApp's expected range (0-100)
		if val < 0 {
			val = 0
		} else if val > 100 {
			val = 100
		}

		waveform[i] = byte(val)
	}

	return waveform
}
