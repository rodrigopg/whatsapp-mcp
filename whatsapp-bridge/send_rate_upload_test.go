package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// An uploaded file waits in the same queue as every other send: a long wait answers 429.
func TestSendRateUploadAnswers429(t *testing.T) {
	h := sendFileHandler(func(context.Context, string, string, *outboundFile) (string, error) {
		return "", &sendRateError{retryAfter: 17 * time.Second}
	})
	w := httptest.NewRecorder()
	h(w, uploadRequest(url.Values{"recipient": {"1"}, "filename": {"a.pdf"}}, []byte("x")))
	if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "17" {
		t.Fatalf("status %d, Retry-After %q: %s", w.Code, w.Header().Get("Retry-After"), w.Body)
	}
}
