package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"go.mau.fi/whatsmeow"
	waProto "go.mau.fi/whatsmeow/binary/proto"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
)

type messageActionClient interface {
	IsConnected() bool
	OwnID() *types.JID
	NormalizeChat(types.JID) types.JID
	OutboundChatJIDs(context.Context, types.JID) []types.JID
	BuildReaction(types.JID, types.JID, types.MessageID, string) *waProto.Message
	BuildRevoke(types.JID, types.JID, types.MessageID) *waProto.Message
	SendMessage(context.Context, types.JID, *waProto.Message, ...whatsmeow.SendRequestExtra) (whatsmeow.SendResponse, error)
	GroupAuthor(ctx context.Context, group, author types.JID) (types.JID, error)
}

type whatsmeowActions struct{ *whatsmeow.Client }

func (c whatsmeowActions) OwnID() *types.JID {
	if c.Client == nil || c.Store == nil {
		return nil
	}
	return c.Store.ID
}

func (c whatsmeowActions) NormalizeChat(chat types.JID) types.JID {
	return resolveToPN(c.Client, chat)
}

func (c whatsmeowActions) OutboundChatJIDs(ctx context.Context, chat types.JID) []types.JID {
	return outboundChatJIDs(ctx, c.Client, chat)
}

func (c whatsmeowActions) GroupAuthor(ctx context.Context, group, author types.JID) (types.JID, error) {
	info, err := c.GetGroupInfo(ctx, group)
	if err != nil {
		return types.EmptyJID, fmt.Errorf("failed to read the group's addressing: %w", err)
	}
	return groupActionAuthor(ctx, info, author, c.Store.LIDs)
}

// groupActionAuthor names the author the way the group addresses its members. A LID group needs the
// author's LID in the message key (messages.sender usually holds the number), or WhatsApp drops the
// reaction or revoke silently (whatsmeow#1071). An author it cannot map is refused before sending.
func groupActionAuthor(ctx context.Context, info *types.GroupInfo, author types.JID, lids store.LIDStore) (types.JID, error) {
	for _, p := range info.Participants {
		if p.JID == author || p.LID == author || p.PhoneNumber == author {
			return p.JID, nil
		}
	}
	// A former member: map through the stored LID <-> number pairs.
	lidGroup := info.AddressingMode == types.AddressingModeLID
	if lidGroup == (author.Server == types.HiddenUserServer) {
		return author, nil
	}
	var mapped types.JID
	var err error
	if lidGroup {
		mapped, err = lids.GetLIDForPN(ctx, author)
	} else {
		mapped, err = lids.GetPNForLID(ctx, author)
	}
	if err != nil {
		return types.EmptyJID, fmt.Errorf("failed to map the author to the group's addressing: %w", err)
	}
	if mapped.IsEmpty() {
		return types.EmptyJID, invalid("the author is no longer in the group and the bridge does not know their address in it, so WhatsApp would drop this action")
	}
	return mapped, nil
}

func groupMessageSender(store *MessageStore, chat types.JID, id string) (types.JID, error) {
	if store == nil {
		return types.EmptyJID, invalid("unknown group message sender: message store unavailable")
	}
	var sender sql.NullString
	err := store.db.QueryRow("SELECT sender FROM messages WHERE id = ? AND chat_jid = ?", id, chat.String()).Scan(&sender)
	if errors.Is(err, sql.ErrNoRows) {
		return types.EmptyJID, invalid("unknown group message sender: message not found in this chat")
	} else if err != nil {
		return types.EmptyJID, fmt.Errorf("failed to read group message sender: %w", err)
	}
	if sender.String == "" || sender.String == chat.User || sender.String == chat.String() {
		return types.EmptyJID, invalid("unknown group message sender: stored author is empty or identifies the group")
	}
	jid, err := parseSender(sender.String)
	if err != nil || jid.User == "" || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
		return types.EmptyJID, invalid("invalid group message sender: expected a PN or LID author")
	}
	return jid.ToNonAD(), nil
}

// writeActionError answers 400 for a request the bridge cannot act on and 500 for a store failure.
func writeActionError(w http.ResponseWriter, err error) {
	if writeSendRateError(w, err) {
		return
	}
	status := http.StatusInternalServerError
	var bad *errInvalidRequest
	if errors.As(err, &bad) {
		status = http.StatusBadRequest
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(MarkChatResponse{Success: false, Message: err.Error()})
}
