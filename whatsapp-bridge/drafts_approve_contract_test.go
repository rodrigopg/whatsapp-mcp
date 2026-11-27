package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestApproveDraftErrorContract(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"empty text", http.StatusBadRequest, "text must be"},
		{"missing draft", http.StatusBadRequest, "not found"},
		{"discarded", http.StatusBadRequest, "not pending"},
		{"send failure", http.StatusInternalServerError, "still pending"},
		{"store failure", http.StatusInternalServerError, "database is closed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := seedMatterChat(t)
			matter, _ := chaseMatter(t, db)
			draft, err := createDraft(db, CreateDraftRequest{MatterID: matter.ID, Text: "hello"}, matterNow)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE matters SET valid_until = ?`, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			text := "hello"
			switch tc.name {
			case "empty text":
				text = ""
			case "missing draft":
				draft.ID++
			case "discarded":
				if _, err := discardDraft(db, DiscardDraftRequest{DraftID: draft.ID}, time.Now()); err != nil {
					t.Fatal(err)
				}
			case "store failure":
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			body := fmt.Sprintf(`{"draft_id":%d,"text":%q}`, draft.ID, text)
			w := httptest.NewRecorder()
			handleApproveDraft(&MessageStore{db: db}, nil)(w, httptest.NewRequest(http.MethodPost, "/api/drafts/approve", strings.NewReader(body)))
			var result map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			message, ok := result["error"].(string)
			if w.Code != tc.status || !ok || len(result) != 1 || !strings.Contains(message, tc.want) {
				t.Fatalf("status=%d body=%s; want status=%d and error containing %q", w.Code, w.Body.String(), tc.status, tc.want)
			}
			if tc.name == "send failure" {
				pending, err := loadDraft(db, draft.ID)
				if err != nil || pending.State != "written" {
					t.Fatalf("draft = %+v, %v", pending, err)
				}
			}
		})
	}
}
