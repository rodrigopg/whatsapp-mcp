package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
)

// safeMediaPath is the load-bearing guard for two invariants: it must reject
// path-traversal attempts, and it must give distinct messages distinct paths
// even when their stored filename collides (the bug that made the download
// cache return the wrong message's bytes).
func TestSafeMediaPath(t *testing.T) {
	chatDir := "store/chat"

	t.Run("rejects traversal in message ID", func(t *testing.T) {
		if _, err := safeMediaPath(chatDir, "../../etc/passwd", "a.ogg"); err == nil {
			t.Fatal("expected error for traversal in message ID")
		}
	})

	t.Run("rejects separator in filename", func(t *testing.T) {
		if _, err := safeMediaPath(chatDir, "ABC", "a/b.ogg"); err == nil {
			t.Fatal("expected error for separator in filename")
		}
	})

	for _, bad := range []string{"", ".", ".."} {
		t.Run("rejects component "+bad, func(t *testing.T) {
			if _, err := safeMediaPath(chatDir, bad, "a.ogg"); err == nil {
				t.Fatalf("expected error for message ID %q", bad)
			}
			if _, err := safeMediaPath(chatDir, "ABC", bad); err == nil {
				t.Fatalf("expected error for filename %q", bad)
			}
		})
	}

	t.Run("distinct IDs with same filename produce distinct paths", func(t *testing.T) {
		p1, err1 := safeMediaPath(chatDir, "MSG1", "audio_20260608.ogg")
		p2, err2 := safeMediaPath(chatDir, "MSG2", "audio_20260608.ogg")
		if err1 != nil || err2 != nil {
			t.Fatalf("unexpected error: %v %v", err1, err2)
		}
		if p1 == p2 {
			t.Fatalf("colliding filename produced identical paths: %s", p1)
		}
	})

	t.Run("valid path stays under chat dir", func(t *testing.T) {
		p, err := safeMediaPath(chatDir, "MSG1", "audio.ogg")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		abs, _ := filepath.Abs(chatDir)
		pabs, _ := filepath.Abs(p)
		if !strings.HasPrefix(pabs, abs) {
			t.Fatalf("path %s escaped chat dir %s", pabs, abs)
		}
	})
}

// extractDirectPathFromURL must keep the query string. whatsmeow's
// DownloadMediaWithPath concatenates "&hash=..." onto the direct path, so the
// query is what supplies the "?" and the CDN authorization token (oh) — drop it
// and every download 403s, which is exactly what happened to this bridge.
func TestExtractDirectPathFromURL(t *testing.T) {
	const url = "https://mmg.whatsapp.net/v/t62.7118-24/582729677_1690209148760939_n.enc" +
		"?ccb=11-4&oh=01_Q5Aa5QEFyuE2cc8EmroSnUoZk72zv970&oe=6AA1BE81&_nc_sid=5e03e0&mms3=true"

	got := extractDirectPathFromURL(url)

	if !strings.HasPrefix(got, "/v/t62.7118-24/") {
		t.Fatalf("direct path must start with the slashed path: got %q", got)
	}
	for _, param := range []string{"?ccb=11-4", "oh=01_Q5Aa5QEFyuE2cc8EmroSnUoZk72zv970", "oe=6AA1BE81"} {
		if !strings.Contains(got, param) {
			t.Errorf("direct path lost %q: got %q", param, got)
		}
	}

	// The invariant that broke: the URL whatsmeow builds on top of this must be
	// a single well-formed query, not a path with a stray '&'.
	built := "https://media.example.net" + got + "&hash=abc&mms-type=image&__wa-mms="
	if strings.Count(built, "?") != 1 {
		t.Fatalf("built URL must have exactly one '?': %s", built)
	}
	if strings.Index(built, "&") < strings.Index(built, "?") {
		t.Fatalf("built URL has a '&' before its '?': %s", built)
	}

	t.Run("unparseable URL is returned unchanged", func(t *testing.T) {
		if got := extractDirectPathFromURL("not-a-media-url"); got != "not-a-media-url" {
			t.Fatalf("expected the input back, got %q", got)
		}
	})
}

// normalizePhone must strip '+', spaces and '-' so contact resolution matches
// regardless of formatting (parity with the Python _normalize_phone).
func TestNormalizePhone(t *testing.T) {
	cases := map[string]string{
		"+55 62 99999-9999": "5562999999999",
		"5562999999999":     "5562999999999",
		"55-62-99999-9999":  "5562999999999",
		"  +5562 ":          "5562",
	}
	for in, want := range cases {
		if got := normalizePhone(in); got != want {
			t.Errorf("normalizePhone(%q) = %q, want %q", in, got, want)
		}
	}
}

// resolveContactJIDs must always return the PN JID first, even with a nil client
// (no LID store available), and must never panic on nil.
func TestResolveContactJIDsPNFirst(t *testing.T) {
	jids := resolveContactJIDs(nil, "+55 62 99999-9999")
	if len(jids) == 0 || jids[0] != "5562999999999@s.whatsapp.net" {
		t.Fatalf("expected PN JID first, got %v", jids)
	}
}

// actionSenderJID backs react/revoke sender resolution: fromMe must resolve to
// the local account (non-AD), never panic on a nil own JID, and fall back to
// chatJID (DM-correct) when fromMe is false.
func TestActionSenderJID(t *testing.T) {
	chatJID, err := types.ParseJID("5562999999999@s.whatsapp.net")
	if err != nil {
		t.Fatalf("unexpected error parsing chat JID: %v", err)
	}

	t.Run("fromMe true uses own JID non-AD", func(t *testing.T) {
		ownJID, err := types.ParseJID("5562988888888:1@s.whatsapp.net")
		if err != nil {
			t.Fatalf("unexpected error parsing own JID: %v", err)
		}
		got := actionSenderJID(&ownJID, chatJID, true)
		want := ownJID.ToNonAD()
		if got != want {
			t.Fatalf("actionSenderJID() = %v, want %v", got, want)
		}
	})

	t.Run("fromMe true with nil own JID falls back to chatJID", func(t *testing.T) {
		got := actionSenderJID(nil, chatJID, true)
		if got != chatJID {
			t.Fatalf("actionSenderJID() = %v, want %v", got, chatJID)
		}
	})

	t.Run("fromMe false falls back to chatJID", func(t *testing.T) {
		ownJID, _ := types.ParseJID("5562988888888:1@s.whatsapp.net")
		got := actionSenderJID(&ownJID, chatJID, false)
		if got != chatJID {
			t.Fatalf("actionSenderJID() = %v, want %v", got, chatJID)
		}
	})
}

// doHandlerRequest posts body (nil for none) to handler and returns the response.
// A nil *whatsmeow.Client is safe here: every case below is rejected before the
// handler dereferences client, so the 503 (client nil/disconnected) path isn't
// exercised by these tests — only the request-validation 405/400 paths are.
func doHandlerRequest(t *testing.T, handler http.HandlerFunc, method string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *bytes.Reader
	if body != nil {
		reqBody = bytes.NewReader(body)
	} else {
		reqBody = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, "/", reqBody)
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

// TestHandleReact covers /api/react request validation: method guard, decode
// guard, required-field guard, and the group + from_me=false rejection that
// prevents actionSenderJID from silently using the group JID as sender.
func TestHandleReact(t *testing.T) {
	handler := handleReact(nil)

	t.Run("non-POST returns 405", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodGet, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("malformed JSON returns 400", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodPost, []byte("{not json"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing message_id returns 400", func(t *testing.T) {
		body, _ := json.Marshal(ReactRequest{ChatJID: "5562999999999@s.whatsapp.net", Emoji: "👍"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("group chat with from_me=false returns 400", func(t *testing.T) {
		body, _ := json.Marshal(ReactRequest{ChatJID: "123456@g.us", MessageID: "MSG1", Emoji: "👍", FromMe: false})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
		var resp MarkChatResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Success {
			t.Fatalf("expected Success=false, got response: %+v", resp)
		}
	})
}

// TestHandleEdit covers /api/edit request validation. Edit has no group guard
// (WhatsApp only allows editing your own messages), so it isn't tested here.
func TestHandleEdit(t *testing.T) {
	handler := handleEdit(nil)

	t.Run("non-POST returns 405", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodGet, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("malformed JSON returns 400", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodPost, []byte("{not json"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing chat_jid returns 400", func(t *testing.T) {
		body, _ := json.Marshal(EditRequest{MessageID: "MSG1", NewText: "hi"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})
}

// TestHandleRevoke covers /api/revoke request validation: same shape as
// TestHandleReact, including the group + from_me=false rejection.
func TestHandleRevoke(t *testing.T) {
	handler := handleRevoke(nil)

	t.Run("non-POST returns 405", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodGet, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("malformed JSON returns 400", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodPost, []byte("{not json"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing message_id returns 400", func(t *testing.T) {
		body, _ := json.Marshal(RevokeRequest{ChatJID: "5562999999999@s.whatsapp.net"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("group chat with from_me=false returns 400", func(t *testing.T) {
		body, _ := json.Marshal(RevokeRequest{ChatJID: "123456@g.us", MessageID: "MSG1", FromMe: false})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
		var resp MarkChatResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if resp.Success {
			t.Fatalf("expected Success=false, got response: %+v", resp)
		}
	})
}

// TestHandleGroupParticipants covers /api/group_participants request
// validation: method guard, decode guard, group_jid @g.us guard, action
// whitelist, and the nil-client 503 path (which the action/JID checks in the
// handler must run before, so it's reached deterministically here).
func TestHandleGroupParticipants(t *testing.T) {
	handler := handleGroupParticipants(nil)

	t.Run("non-POST returns 405", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodGet, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("malformed JSON returns 400", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodPost, []byte("{not json"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing participants returns 400", func(t *testing.T) {
		body, _ := json.Marshal(GroupParticipantsRequest{GroupJID: "123456@g.us", Action: "add"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("group_jid not @g.us returns 400", func(t *testing.T) {
		body, _ := json.Marshal(GroupParticipantsRequest{GroupJID: "5562999999999@s.whatsapp.net", Participants: []string{"5562988887777"}, Action: "add"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid action returns 400", func(t *testing.T) {
		body, _ := json.Marshal(GroupParticipantsRequest{GroupJID: "123456@g.us", Participants: []string{"5562988887777"}, Action: "kick"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("disconnected client returns 503", func(t *testing.T) {
		body, _ := json.Marshal(GroupParticipantsRequest{GroupJID: "123456@g.us", Participants: []string{"5562988887777"}, Action: "add"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})
}

// TestHandleChatPresence covers /api/chat_presence request validation: method
// guard, decode guard, state whitelist, media whitelist, and 503 disconnected.
func TestHandleChatPresence(t *testing.T) {
	handler := handleChatPresence(nil)

	t.Run("non-POST returns 405", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodGet, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("malformed JSON returns 400", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodPost, []byte("{not json"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("missing chat_jid returns 400", func(t *testing.T) {
		body, _ := json.Marshal(ChatPresenceRequest{State: "composing"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid state returns 400", func(t *testing.T) {
		body, _ := json.Marshal(ChatPresenceRequest{ChatJID: "5562999999999@s.whatsapp.net", State: "typing"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("invalid media returns 400", func(t *testing.T) {
		body, _ := json.Marshal(ChatPresenceRequest{ChatJID: "5562999999999@s.whatsapp.net", State: "composing", Media: "video"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("disconnected client returns 503", func(t *testing.T) {
		body, _ := json.Marshal(ChatPresenceRequest{ChatJID: "5562999999999@s.whatsapp.net", State: "composing"})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})
}

// TestHandleIsOnWhatsApp covers /api/is_on_whatsapp request validation: method
// guard, decode guard, empty phones guard, and 503 disconnected.
func TestHandleIsOnWhatsApp(t *testing.T) {
	handler := handleIsOnWhatsApp(nil)

	t.Run("non-POST returns 405", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodGet, nil)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusMethodNotAllowed)
		}
	})

	t.Run("malformed JSON returns 400", func(t *testing.T) {
		rec := doHandlerRequest(t, handler, http.MethodPost, []byte("{not json"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("empty phones returns 400", func(t *testing.T) {
		body, _ := json.Marshal(IsOnWhatsAppRequest{Phones: []string{}})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("disconnected client returns 503", func(t *testing.T) {
		body, _ := json.Marshal(IsOnWhatsAppRequest{Phones: []string{"5562999999999"}})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
		}
	})

	t.Run("malformed phone returns 400", func(t *testing.T) {
		body, _ := json.Marshal(IsOnWhatsAppRequest{Phones: []string{"+55 (62) 9999-9999"}})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})

	t.Run("more than 50 phones returns 400", func(t *testing.T) {
		phones := make([]string, 51)
		for i := range phones {
			phones[i] = "5562999999999"
		}
		body, _ := json.Marshal(IsOnWhatsAppRequest{Phones: phones})
		rec := doHandlerRequest(t, handler, http.MethodPost, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
		}
	})
}

// TestParseGroupParticipantJIDs covers the pure parsing/validation logic
// behind /api/group_participants, independent of the whatsmeow client.
func TestParseGroupParticipantJIDs(t *testing.T) {
	t.Run("bare phone with internal space and hyphen normalizes", func(t *testing.T) {
		jids, err := parseGroupParticipantJIDs([]string{"55 62-99999-7777"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if jids[0].User != "5562999997777" || jids[0].Server != types.DefaultUserServer {
			t.Fatalf("got %+v", jids[0])
		}
	})

	t.Run("00 prefix is kept as literal digits, not stripped", func(t *testing.T) {
		jids, err := parseGroupParticipantJIDs([]string{"0055629999977"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if jids[0].User != "0055629999977" {
			t.Fatalf("got %q", jids[0].User)
		}
	})

	t.Run("full JID with default user server is accepted as-is", func(t *testing.T) {
		jids, err := parseGroupParticipantJIDs([]string{"5562999999999@s.whatsapp.net"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if jids[0].String() != "5562999999999@s.whatsapp.net" {
			t.Fatalf("got %q", jids[0].String())
		}
	})

	t.Run("empty item after trim returns error", func(t *testing.T) {
		if _, err := parseGroupParticipantJIDs([]string{"5562999999999", "  "}); err == nil {
			t.Fatal("expected error for empty participant")
		}
	})

	t.Run("123@lid is accepted as a participant", func(t *testing.T) {
		jids, err := parseGroupParticipantJIDs([]string{"123@lid"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if jids[0].Server != types.HiddenUserServer {
			t.Fatalf("got server %q", jids[0].Server)
		}
	})

	t.Run("unsupported server returns error", func(t *testing.T) {
		if _, err := parseGroupParticipantJIDs([]string{"123@foo.bar"}); err == nil {
			t.Fatal("expected error for unsupported server")
		}
	})

	t.Run("no participants after parsing returns error", func(t *testing.T) {
		if _, err := parseGroupParticipantJIDs([]string{}); err == nil {
			t.Fatal("expected error for empty list")
		}
	})
}

// TestNormalizeCheckPhones covers the pure validation/normalization logic
// behind /api/is_on_whatsapp, independent of the whatsmeow client.
func TestNormalizeCheckPhones(t *testing.T) {
	t.Run("internal space and hyphen normalize and gain +", func(t *testing.T) {
		phones, err := normalizeCheckPhones([]string{"55 62-99999-7777"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if phones[0] != "+5562999997777" {
			t.Fatalf("got %q", phones[0])
		}
	})

	t.Run("00 prefix rejected as invalid digit count edge case still normalizes", func(t *testing.T) {
		phones, err := normalizeCheckPhones([]string{"0055629999977"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if phones[0] != "+0055629999977" {
			t.Fatalf("got %q", phones[0])
		}
	})

	t.Run("plus already present is not duplicated", func(t *testing.T) {
		phones, err := normalizeCheckPhones([]string{"+5562999999999"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if phones[0] != "+5562999999999" {
			t.Fatalf("got %q", phones[0])
		}
	})

	t.Run("empty item returns error", func(t *testing.T) {
		if _, err := normalizeCheckPhones([]string{""}); err == nil {
			t.Fatal("expected error for empty phone")
		}
	})

	t.Run("non-digit characters return error", func(t *testing.T) {
		if _, err := normalizeCheckPhones([]string{"abc12345"}); err == nil {
			t.Fatal("expected error for non-digit phone")
		}
	})

	t.Run("more than 50 phones returns error", func(t *testing.T) {
		phones := make([]string, 51)
		for i := range phones {
			phones[i] = "5562999999999"
		}
		if _, err := normalizeCheckPhones(phones); err == nil {
			t.Fatal("expected error for exceeding cap")
		}
	})
}

// TestMergeIsOnWhatsAppResults covers the fill-in-omissions merge behind
// /api/is_on_whatsapp: whatsmeow omits unregistered numbers from its response
// instead of returning IsIn=false, so the merge must backfill them.
func TestMergeIsOnWhatsAppResults(t *testing.T) {
	t.Run("registered number keeps lib result", func(t *testing.T) {
		resp := []types.IsOnWhatsAppResponse{
			{Query: "+5562999999999", IsIn: true, JID: types.NewJID("5562999999999", types.DefaultUserServer)},
		}
		out := mergeIsOnWhatsAppResults([]string{"+5562999999999"}, resp)
		if len(out) != 1 || !out[0].IsIn || out[0].JID != "5562999999999@s.whatsapp.net" {
			t.Fatalf("got %+v", out)
		}
	})

	t.Run("unregistered number omitted by lib is backfilled as is_in false", func(t *testing.T) {
		resp := []types.IsOnWhatsAppResponse{
			{Query: "+556291788888", IsIn: true, JID: types.NewJID("556291788888", types.DefaultUserServer)},
		}
		out := mergeIsOnWhatsAppResults([]string{"+556291788888", "+5562000000000"}, resp)
		if len(out) != 2 {
			t.Fatalf("got %d results, want 2: %+v", len(out), out)
		}
		if out[0].Query != "+556291788888" || !out[0].IsIn {
			t.Fatalf("got[0] = %+v", out[0])
		}
		if out[1].Query != "+5562000000000" || out[1].IsIn || out[1].JID != "" {
			t.Fatalf("got[1] = %+v", out[1])
		}
	})

	t.Run("output order matches input order regardless of response order", func(t *testing.T) {
		resp := []types.IsOnWhatsAppResponse{
			{Query: "+5562000000002", IsIn: true, JID: types.NewJID("5562000000002", types.DefaultUserServer)},
		}
		out := mergeIsOnWhatsAppResults([]string{"+5562000000001", "+5562000000002", "+5562000000003"}, resp)
		if len(out) != 3 {
			t.Fatalf("got %d results, want 3", len(out))
		}
		wantOrder := []string{"+5562000000001", "+5562000000002", "+5562000000003"}
		for i, q := range wantOrder {
			if out[i].Query != q {
				t.Fatalf("out[%d].Query = %q, want %q", i, out[i].Query, q)
			}
		}
		if !out[1].IsIn {
			t.Fatalf("out[1] should be registered: %+v", out[1])
		}
	})
}

// setupChatStore returns a MessageStore backed by the production schema in a
// fresh temp dir, so listChats/getChat run against the real CREATE TABLE
// block instead of a hand-maintained copy, and the test needs no
// machine-local database.
func setupChatStore(t *testing.T) *MessageStore {
	t.Helper()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	// Cleanups run LIFO, so registering the chdir-back after t.TempDir makes it
	// run before TempDir removal - Windows cannot delete a process's CWD.
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	store, err := NewMessageStore()
	if err != nil {
		t.Fatalf("NewMessageStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func boolPtr(b bool) *bool {
	return &b
}

// seedChatWithLastMessage inserts one chat and one message that is that
// chat's last message: last_message_time matches messages.timestamp, which
// is what the LEFT JOIN in listChats/getChat keys on.
func seedChatWithLastMessage(t *testing.T, db *sql.DB, jid, name, content, sender string, isFromMe bool, ts time.Time) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`, jid, name, ts); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, ?, ?, ?, ?)`,
		"msg1", jid, sender, content, ts, isFromMe); err != nil {
		t.Fatalf("insert message: %v", err)
	}
}

// TestListChatsIncludeLastMessage is the regression test for the 500 that
// include_last_message:false triggered in /api/chats: the SELECT projected
// messages.content/sender/is_from_me unconditionally while the JOIN that
// makes those columns exist was conditional ("no such column:
// messages.content"). The fix keeps 6 columns in the projection always,
// swapping in NULL literals for the message columns when the JOIN is
// skipped, so scanAPIChatRow (shared by 4 callers) never changes.
func TestListChatsIncludeLastMessage(t *testing.T) {
	store := setupChatStore(t)
	ts := time.Date(2026, 8, 23, 10, 0, 0, 0, time.UTC)
	seedChatWithLastMessage(t, store.db, "123@s.whatsapp.net", "Alice", "oi", "123@s.whatsapp.net", false, ts)

	t.Run("false: no SQL error, three fields nil", func(t *testing.T) {
		resp, err := listChats(store.db, ChatsRequest{Limit: 10, IncludeLastMessage: boolPtr(false)})
		if err != nil {
			t.Fatalf("listChats: %v", err)
		}
		if len(resp.Chats) != 1 {
			t.Fatalf("got %d chats, want 1", len(resp.Chats))
		}
		chat := resp.Chats[0]
		if chat.LastMessage != nil || chat.LastSender != nil || chat.LastIsFromMe != nil {
			t.Errorf("expected nil last_message/last_sender/last_is_from_me, got %v / %v / %v",
				chat.LastMessage, chat.LastSender, chat.LastIsFromMe)
		}
	})

	t.Run("true: three fields filled", func(t *testing.T) {
		resp, err := listChats(store.db, ChatsRequest{Limit: 10, IncludeLastMessage: boolPtr(true)})
		if err != nil {
			t.Fatalf("listChats: %v", err)
		}
		if len(resp.Chats) != 1 {
			t.Fatalf("got %d chats, want 1", len(resp.Chats))
		}
		chat := resp.Chats[0]
		if chat.LastMessage == nil || *chat.LastMessage != "oi" {
			t.Errorf("last_message = %v, want %q", chat.LastMessage, "oi")
		}
		if chat.LastSender == nil || *chat.LastSender != "123@s.whatsapp.net" {
			t.Errorf("last_sender = %v, want sender jid", chat.LastSender)
		}
		if chat.LastIsFromMe == nil || *chat.LastIsFromMe != false {
			t.Errorf("last_is_from_me = %v, want false", chat.LastIsFromMe)
		}
	})
}

// TestGetChatIncludeLastMessage mirrors TestListChatsIncludeLastMessage for
// /api/chat: same defect ("no such column: m.content"), same fix.
func TestGetChatIncludeLastMessage(t *testing.T) {
	store := setupChatStore(t)
	ts := time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC)
	seedChatWithLastMessage(t, store.db, "456@s.whatsapp.net", "Bob", "tudo bem", "456@s.whatsapp.net", true, ts)

	t.Run("false: no SQL error, three fields nil", func(t *testing.T) {
		resp, err := getChat(store.db, ChatRequest{ChatJID: "456@s.whatsapp.net", IncludeLastMessage: boolPtr(false)})
		if err != nil {
			t.Fatalf("getChat: %v", err)
		}
		if resp.Chat == nil {
			t.Fatal("expected a chat, got nil")
		}
		if resp.Chat.LastMessage != nil || resp.Chat.LastSender != nil || resp.Chat.LastIsFromMe != nil {
			t.Errorf("expected nil last_message/last_sender/last_is_from_me, got %v / %v / %v",
				resp.Chat.LastMessage, resp.Chat.LastSender, resp.Chat.LastIsFromMe)
		}
	})

	t.Run("true: three fields filled", func(t *testing.T) {
		resp, err := getChat(store.db, ChatRequest{ChatJID: "456@s.whatsapp.net", IncludeLastMessage: boolPtr(true)})
		if err != nil {
			t.Fatalf("getChat: %v", err)
		}
		if resp.Chat == nil {
			t.Fatal("expected a chat, got nil")
		}
		if resp.Chat.LastMessage == nil || *resp.Chat.LastMessage != "tudo bem" {
			t.Errorf("last_message = %v, want %q", resp.Chat.LastMessage, "tudo bem")
		}
		if resp.Chat.LastSender == nil || *resp.Chat.LastSender != "456@s.whatsapp.net" {
			t.Errorf("last_sender = %v, want sender jid", resp.Chat.LastSender)
		}
		if resp.Chat.LastIsFromMe == nil || *resp.Chat.LastIsFromMe != true {
			t.Errorf("last_is_from_me = %v, want true", resp.Chat.LastIsFromMe)
		}
	})
}

// TestMessageStoreIndexes checks the schema NewMessageStore creates. An index is
// only worth anything if the planner actually picks it - existing in
// sqlite_master is not the same as being used, so this asserts both.
func TestMessageStoreIndexes(t *testing.T) {
	store := setupChatStore(t)

	for _, name := range []string{
		"idx_messages_chat_time",
		"idx_messages_audio_pending",
		"idx_senders_names",
	} {
		var found string
		err := store.db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='index' AND name=?", name).Scan(&found)
		if err != nil {
			t.Errorf("index %s not created: %v", name, err)
		}
	}

	// The reads that matter must resolve through the index instead of scanning.
	for _, tc := range []struct {
		name  string
		query string
		args  []any
		want  string
	}{
		{
			name:  "recent messages of one chat",
			query: "SELECT id FROM messages WHERE chat_jid=? ORDER BY timestamp DESC LIMIT 20",
			args:  []any{"x@g.us"},
			want:  "idx_messages_chat_time",
		},
		{
			name:  "audio pending transcription",
			query: "SELECT id FROM messages WHERE media_type='audio' AND (content IS NULL OR content='')",
			args:  nil,
			want:  "idx_messages_audio_pending",
		},
	} {
		rows, err := store.db.Query("EXPLAIN QUERY PLAN "+tc.query, tc.args...)
		if err != nil {
			t.Fatalf("%s: explain: %v", tc.name, err)
		}
		var plan strings.Builder
		cols, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			cells := make([]any, len(cols))
			for i := range cells {
				cells[i] = new(any)
			}
			if err := rows.Scan(cells...); err != nil {
				t.Fatalf("%s: scan: %v", tc.name, err)
			}
			// The last column is the human-readable detail ("SEARCH ... USING INDEX ...").
			fmt.Fprintf(&plan, "%v ", *(cells[len(cells)-1].(*any)))
		}
		rows.Close()
		if !strings.Contains(plan.String(), tc.want) {
			t.Errorf("%s: plan does not use %s\n  plan: %s", tc.name, tc.want, plan.String())
		}
	}
}
