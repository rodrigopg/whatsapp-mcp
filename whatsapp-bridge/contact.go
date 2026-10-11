package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// ContactRequest asks the bridge to send a native contact card (vCard) of
// name/phone_number to recipient (a phone number or a JID).
type ContactRequest struct {
	Recipient   string `json:"recipient"`
	Name        string `json:"name"`
	PhoneNumber string `json:"phone_number"`
}

type ContactResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	MessageID string `json:"message_id,omitempty"`
}

var phoneSeparators = strings.NewReplacer(" ", "", "-", "", "(", "", ")", "", "+", "", ".", "")

// normalizeContactPhone keeps the digits of a phone typed the way people type
// it and requires an E.164-sized number (8 to 15 digits): no guessing.
func normalizeContactPhone(s string) (string, error) {
	digits := phoneSeparators.Replace(s)
	if !isDigits(digits) || len(digits) < 8 || len(digits) > 15 {
		return "", fmt.Errorf("invalid phone_number %q: expected 8 to 15 digits with country code", s)
	}
	return digits, nil
}

var vcardEscaper = strings.NewReplacer(`\`, `\\`, ",", `\,`, ";", `\;`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`)

// buildContactVCard renders a vCard 3.0 whose name cannot add lines or fields.
func buildContactVCard(name, digits string) string {
	return "BEGIN:VCARD\nVERSION:3.0\nFN:" + vcardEscaper.Replace(name) +
		"\nTEL;type=CELL;waid=" + digits + ":+" + digits + "\nEND:VCARD"
}

// handleSendContact returns the handler for POST /api/send_contact.
func handleSendContact(client *whatsmeow.Client, store *MessageStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		var req ContactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		name := strings.TrimSpace(req.Name)
		if strings.TrimSpace(req.Recipient) == "" || name == "" {
			writeJSONError(w, http.StatusBadRequest, "recipient, name and phone_number are required")
			return
		}
		digits, err := normalizeContactPhone(req.PhoneNumber)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		chatJID := types.JID{User: strings.TrimSpace(req.Recipient), Server: types.DefaultUserServer}
		if strings.Contains(chatJID.User, "@") {
			if chatJID, err = types.ParseJID(chatJID.User); err != nil {
				writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("Invalid recipient: %v", err))
				return
			}
		}
		if client == nil || !client.IsConnected() {
			writeJSONError(w, http.StatusServiceUnavailable, "WhatsApp client not connected")
			return
		}
		msg := &waE2E.Message{ContactMessage: &waE2E.ContactMessage{
			DisplayName: proto.String(name),
			Vcard:       proto.String(buildContactVCard(name, digits)),
		}}
		resp, err := client.SendMessage(context.Background(), chatJID, msg)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("SendMessage error: %v", err))
			return
		}
		// Own sends don't echo back through handleMessage on single-device accounts.
		if client.Store != nil && client.Store.ID != nil {
			chat := chatJID.String()
			if err := store.EnsureChat(chat, resp.Timestamp); err == nil {
				_ = store.StoreMessage(resp.ID, chat, client.Store.ID.User, "[contact] "+name+" +"+digits, resp.Timestamp, true, "", "", "", nil, nil, nil, 0)
				_ = store.TouchChatLastMessageTime(chat, resp.Timestamp)
			}
		}
		writeJSON(w, ContactResponse{Success: true, Message: "Contact sent", MessageID: resp.ID})
	}
}
