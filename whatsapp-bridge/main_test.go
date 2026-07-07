package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
