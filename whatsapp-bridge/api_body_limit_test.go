package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAPIBodyLimit(t *testing.T) {
	var readErr error
	var read int
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b []byte
		b, readErr = io.ReadAll(r.Body)
		read = len(b)
	})
	for _, token := range []string{"", "secret"} {
		h := apiGuard(token, apiHosts(""), next)
		send := func(path string, body []byte, chunked bool) int {
			readErr, read = nil, 0
			r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
			r.Host = "127.0.0.1:8080"
			if chunked {
				r.ContentLength = -1 // the size is not announced, so only reading can stop it
			}
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, r)
			return rec.Code
		}
		message, _ := json.Marshal(SendMessageRequest{Recipient: "123", Message: string(bytes.Repeat([]byte("a"), 65536))})
		if code := send("/api/send", message, false); code != http.StatusOK || readErr != nil || read != len(message) {
			t.Errorf("token=%q: a long message: code %d, read %d, err %v", token, code, read, readErr)
		}
		big := bytes.Repeat([]byte("a"), maxAPIBody+1)
		if code := send("/api/send", big, false); code != http.StatusRequestEntityTooLarge || read != 0 {
			t.Errorf("token=%q: announced oversize body: code %d, handler read %d", token, code, read)
		}
		var tooLarge *http.MaxBytesError
		if send("/api/send", big, true); !errors.As(readErr, &tooLarge) || read > maxAPIBody {
			t.Errorf("token=%q: unannounced oversize body: read %d, err %v", token, read, readErr)
		}
		if code := send("/status/settings", big, false); code != http.StatusOK || readErr != nil {
			t.Errorf("token=%q: the cap is for /api/ only, the panel has its own: code %d, err %v", token, code, readErr)
		}
	}
}
