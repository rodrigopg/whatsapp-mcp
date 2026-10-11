package main

import (
	"database/sql"
	"strings"
	"testing"
)

type sendStub struct {
	calls []string // "chat|text"
	ok    bool
}

func (s *sendStub) send(chat, text string) (bool, string) {
	s.calls = append(s.calls, chat+"|"+text)
	if !s.ok {
		return false, "Not connected to WhatsApp"
	}
	return true, "sent"
}

// a matter where the OTHER person owes (111 promised), plus one the owner owes.
func chaseMatter(t *testing.T, db *sql.DB) (other, mine APIMatter) {
	t.Helper()
	dl := "2026-10-09"
	r := rec("111@s.whatsapp.net", "m3", "other", "promise")
	r.Deadline = &dl
	other = mustRecord(t, db, r)
	mine = mustRecord(t, db, rec("111@s.whatsapp.net", "m1", "owner", "request"))
	return
}

func TestCreateDraft(t *testing.T) {
	db := seedMatterChat(t)
	other, mine := chaseMatter(t, db)
	if _, err := createDraft(db, CreateDraftRequest{MatterID: mine.ID, Text: "oi"}, matterNow); err == nil {
		t.Error("a draft chasing someone for what the owner owes must be refused")
	}
	if _, err := createDraft(db, CreateDraftRequest{MatterID: other.ID, Text: "  "}, matterNow); err == nil {
		t.Error("an empty text must be refused")
	}
	if _, err := createDraft(db, CreateDraftRequest{MatterID: other.ID, Text: strings.Repeat("a", 1001)}, matterNow); err == nil {
		t.Error("a text over 1000 characters must be refused")
	}
	d, err := createDraft(db, CreateDraftRequest{MatterID: other.ID, Text: "Oi, e o contrato?"}, matterNow)
	if err != nil || d.State != "written" || d.Name != "111" || d.Text != "Oi, e o contrato?" || d.ChatJID != "111@s.whatsapp.net" {
		t.Fatalf("draft = %+v, %v", d, err)
	}
	again, err := createDraft(db, CreateDraftRequest{MatterID: other.ID, Text: "Oi! Conseguiu ver o contrato?"}, matterNow)
	if err != nil || again.ID != d.ID || again.Text != "Oi! Conseguiu ver o contrato?" {
		t.Errorf("a second draft for the same matter must rewrite the first: %+v, %v", again, err)
	}
	if _, err := closeMatter(db, CloseMatterRequest{MatterID: other.ID, By: "owner"}, matterNow); err != nil {
		t.Fatal(err)
	}
	if _, err := createDraft(db, CreateDraftRequest{MatterID: other.ID, Text: "oi"}, matterNow); err == nil {
		t.Error("a closed matter must not get a draft")
	}
}

func TestApproveDraftSendsExactlyTheApprovedText(t *testing.T) {
	db := seedMatterChat(t)
	other, _ := chaseMatter(t, db)
	d, _ := createDraft(db, CreateDraftRequest{MatterID: other.ID, Text: "Oi, e o contrato?"}, matterNow)
	stub := &sendStub{}

	// Nothing is sent by writing, listing or failing to approve.
	if len(stub.calls) != 0 {
		t.Fatal("writing a draft must not send")
	}
	// A failed send keeps the draft pending and reports why.
	if _, err := approveDraft(db, ApproveDraftRequest{DraftID: d.ID, Text: "Oi, e o contrato?"}, stub.send, matterNow); err == nil || !strings.Contains(err.Error(), "Not connected") {
		t.Fatalf("failed send: err = %v", err)
	}
	if got, _ := listDrafts(db, ListDraftsRequest{State: "written"}); len(got) != 1 {
		t.Fatalf("after a failed send the draft must stay pending, got %+v", got)
	}

	// Approving with an edited text sends that text, once.
	stub.ok = true
	sent, err := approveDraft(db, ApproveDraftRequest{DraftID: d.ID, Text: "  Oi Ana, conseguiu ver o contrato?  "}, stub.send, matterNow)
	if err != nil || sent.State != "sent" || sent.SentText == nil || *sent.SentText != "Oi Ana, conseguiu ver o contrato?" {
		t.Fatalf("approved = %+v, %v", sent, err)
	}
	if want := "111@s.whatsapp.net|Oi Ana, conseguiu ver o contrato?"; stub.calls[len(stub.calls)-1] != want {
		t.Errorf("sent %q, want %q", stub.calls[len(stub.calls)-1], want)
	}
	n := len(stub.calls)
	if _, err := approveDraft(db, ApproveDraftRequest{DraftID: d.ID, Text: "outra vez"}, stub.send, matterNow); err == nil || len(stub.calls) != n {
		t.Errorf("a sent draft must never be sent again (calls %d -> %d, err %v)", n, len(stub.calls), err)
	}
}

func TestApproveRefusalsNeverSend(t *testing.T) {
	db := seedMatterChat(t)
	other, _ := chaseMatter(t, db)
	stub := &sendStub{ok: true}
	d, _ := createDraft(db, CreateDraftRequest{MatterID: other.ID, Text: "oi"}, matterNow)

	if _, err := approveDraft(db, ApproveDraftRequest{DraftID: d.ID, Text: "   "}, stub.send, matterNow); err == nil {
		t.Error("an empty approved text must be refused")
	}
	if _, err := approveDraft(db, ApproveDraftRequest{DraftID: 999, Text: "oi"}, stub.send, matterNow); err == nil {
		t.Error("an unknown draft must be refused")
	}
	// The matter was resolved before the owner approved: nothing goes out.
	if _, err := closeMatter(db, CloseMatterRequest{MatterID: other.ID, By: "owner"}, matterNow); err != nil {
		t.Fatal(err)
	}
	if _, err := approveDraft(db, ApproveDraftRequest{DraftID: d.ID, Text: "oi"}, stub.send, matterNow); err == nil {
		t.Error("a draft of a closed matter must not be sent")
	}
	// A discarded draft is never sent.
	other2 := mustRecord(t, db, RecordMatterRequest{ChatJID: "111@s.whatsapp.net", MessageID: "m2", OwedBy: "other", Origin: "promise"})
	d2, _ := createDraft(db, CreateDraftRequest{MatterID: other2.ID, Text: "oi"}, matterNow)
	if got, err := discardDraft(db, DiscardDraftRequest{DraftID: d2.ID}, matterNow); err != nil || got.State != "discarded" {
		t.Fatalf("discard = %+v, %v", got, err)
	}
	if _, err := approveDraft(db, ApproveDraftRequest{DraftID: d2.ID, Text: "oi"}, stub.send, matterNow); err == nil {
		t.Error("a discarded draft must not be sent")
	}
	if len(stub.calls) != 0 {
		t.Errorf("no refusal may send anything, got %v", stub.calls)
	}
}

func TestSummaryListsPendingDrafts(t *testing.T) {
	db := seedMatterChat(t)
	other, _ := chaseMatter(t, db)
	if _, err := createDraft(db, CreateDraftRequest{MatterID: other.ID, Text: "Oi, e o contrato?"}, matterNow); err != nil {
		t.Fatal(err)
	}
	s, err := morningSummary(db, SummaryRequest{AsOf: "2026-10-12T07:00:00-03:00"}, nil, matterNow)
	if err != nil || len(s.Drafts) != 1 || s.Drafts[0].Text != "Oi, e o contrato?" || s.Drafts[0].MatterID != other.ID {
		t.Fatalf("summary drafts = %+v, %v", s.Drafts, err)
	}
	empty, _ := morningSummary(setupChatStore(t).db, SummaryRequest{AsOf: "2026-10-12T07:00:00-03:00"}, nil, matterNow)
	if empty.Drafts == nil {
		t.Error("drafts must be an empty list, not null")
	}
}
