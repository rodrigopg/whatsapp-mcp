package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type sentFile struct {
	recipient, message string
	file               *outboundFile
}

func uploadRequest(query url.Values, body []byte) *http.Request {
	return httptest.NewRequest(http.MethodPost, "/api/send/file?"+query.Encode(), bytes.NewReader(body))
}

func TestSendFileUploadsBytesNotPaths(t *testing.T) {
	var got []sentFile
	h := sendFileHandler(func(recipient, message string, f *outboundFile) (bool, string) {
		got = append(got, sentFile{recipient, message, f})
		return true, "Message sent to " + recipient
	})
	body := []byte("%PDF-1.1 fake")
	w := httptest.NewRecorder()
	h(w, uploadRequest(url.Values{"recipient": {"5511@s.whatsapp.net"}, "filename": {"report.pdf"}, "message": {"see"}}, body))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp SendMessageResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || !resp.Success {
		t.Fatalf("response %s (%v)", w.Body, err)
	}
	if len(got) != 1 || got[0].file.name != "report.pdf" || !bytes.Equal(got[0].file.data, body) || got[0].message != "see" {
		t.Fatalf("sent %+v", got)
	}
}

func TestSendFileRefusals(t *testing.T) {
	big := bytes.Repeat([]byte{1}, maxMediaBytes+1)
	for _, tc := range []struct {
		name  string
		query url.Values
		body  []byte
		code  int
	}{
		{"no recipient", url.Values{"filename": {"a.pdf"}}, []byte("x"), http.StatusBadRequest},
		{"no name", url.Values{"recipient": {"1"}}, []byte("x"), http.StatusBadRequest},
		{"slash in name", url.Values{"recipient": {"1"}, "filename": {"../etc/passwd"}}, []byte("x"), http.StatusBadRequest},
		{"backslash in name", url.Values{"recipient": {"1"}, "filename": {`a\b.pdf`}}, []byte("x"), http.StatusBadRequest},
		{"dot-dot name", url.Values{"recipient": {"1"}, "filename": {".."}}, []byte("x"), http.StatusBadRequest},
		{"control char", url.Values{"recipient": {"1"}, "filename": {"a\nb.pdf"}}, []byte("x"), http.StatusBadRequest},
		{"empty body", url.Values{"recipient": {"1"}, "filename": {"a.pdf"}}, nil, http.StatusBadRequest},
		{"too big", url.Values{"recipient": {"1"}, "filename": {"a.pdf"}}, big, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := sendFileHandler(func(string, string, *outboundFile) (bool, string) { called = true; return true, "" })
			w := httptest.NewRecorder()
			h(w, uploadRequest(tc.query, tc.body))
			if w.Code != tc.code || called {
				t.Fatalf("status %d (want %d), sent=%v: %s", w.Code, tc.code, called, w.Body)
			}
		})
	}
	w := httptest.NewRecorder()
	sendFileHandler(nil)(w, httptest.NewRequest(http.MethodGet, "/api/send/file", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d", w.Code)
	}
}

// The real send path refuses when WhatsApp is not connected, after the upload was accepted.
func TestSendFileReachesSendContent(t *testing.T) {
	h := sendFileHandler(func(recipient, message string, f *outboundFile) (bool, string) {
		return sendWhatsAppContent(nil, nil, recipient, message, f, "", nil)
	})
	w := httptest.NewRecorder()
	h(w, uploadRequest(url.Values{"recipient": {"1"}, "filename": {"a.pdf"}}, []byte("x")))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "Not connected") {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
}

func seedMedia(t *testing.T, store *MessageStore, chat, id, filename string) {
	t.Helper()
	if _, err := store.db.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, 'x', datetime('now'))`, chat); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename)
		VALUES (?, ?, 'someone', '', datetime('now'), 0, 'document', ?)`, id, chat, filename); err != nil {
		t.Fatal(err)
	}
}

func downloadRequest(id, chat string) *http.Request {
	body, _ := json.Marshal(DownloadMediaRequest{MessageID: id, ChatJID: chat})
	return httptest.NewRequest(http.MethodPost, "/api/download/file", bytes.NewReader(body))
}

func TestDownloadFileServesStoredMedia(t *testing.T) {
	store := setupChatStore(t)
	const chat, id = "5511@s.whatsapp.net", "ABC123"
	seedMedia(t, store, chat, id, "relatório final.pdf")
	want := []byte("media bytes")
	if err := os.MkdirAll(filepath.Join("store", chat), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("store", chat, id+"_relatório final.pdf"), want, 0o600); err != nil {
		t.Fatal(err)
	}
	h := downloadFileHandler(func(id, chat string) (bool, string, string, string, error) {
		return downloadMedia(nil, store, id, chat)
	})
	w := httptest.NewRecorder()
	h(w, downloadRequest(id, chat))
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), want) {
		t.Fatalf("status %d body %q", w.Code, w.Body)
	}
	name, err := url.PathUnescape(w.Header().Get("X-Media-Filename"))
	if err != nil || name != "relatório final.pdf" {
		t.Fatalf("X-Media-Filename %q (%v)", w.Header().Get("X-Media-Filename"), err)
	}
	if w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers %v", w.Header())
	}
	if strings.Contains(w.Body.String()+strings.Join(w.Header().Values("X-Media-Filename"), ""), "store") {
		t.Fatal("response names a bridge path")
	}
}

func TestDownloadFileRefusals(t *testing.T) {
	store := setupChatStore(t)
	const chat = "5511@s.whatsapp.net"
	seedMedia(t, store, chat, "LINK", "x.pdf")
	if err := os.MkdirAll(filepath.Join("store", chat), 0o700); err != nil {
		t.Fatal(err)
	}
	// A symlink planted where the media file should be is never followed.
	if err := os.Symlink(filepath.Join("..", "messages.db"), filepath.Join("store", chat, "LINK_x.pdf")); err != nil {
		t.Fatal(err)
	}
	h := downloadFileHandler(func(id, chat string) (bool, string, string, string, error) {
		return downloadMedia(nil, store, id, chat)
	})
	for _, tc := range []struct{ name, id, chat string }{
		{"unknown message", "NOPE", chat},
		{"symlinked media", "LINK", chat},
		{"traversal chat", "LINK", "../store"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h(w, downloadRequest(tc.id, tc.chat))
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status %d: %s", w.Code, w.Body)
			}
			var resp DownloadMediaResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.Success || resp.Message != "Failed to download media" {
				t.Fatalf("response %s (%v)", w.Body, err)
			}
		})
	}
	// A path returned by the downloader that is not a chat media file is not served.
	for _, path := range []string{"store/messages.db", "store/backup/whatsapp.db"} {
		w := httptest.NewRecorder()
		downloadFileHandler(func(string, string) (bool, string, string, string, error) {
			abs, _ := filepath.Abs(path)
			return true, "document", "x", abs, nil
		})(w, downloadRequest("X", chat))
		if w.Code == http.StatusOK {
			t.Fatalf("%s served", path)
		}
	}
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodPost, "/api/download/file", strings.NewReader(`{"chat_jid":"`+chat+`","message_id":"`+strings.Repeat("x", 1<<20)+`"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized body: %d", w.Code)
	}
}

func TestDownloadFileAllowedInReadOnly(t *testing.T) {
	if !readOnlyAllowed["/api/download/file"] || readOnlyAllowed["/api/send/file"] {
		t.Fatal("download/file must be a read route and send/file a write route")
	}
}
