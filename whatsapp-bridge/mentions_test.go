package main

import (
	"testing"

	waProto "go.mau.fi/whatsmeow/proto/waE2E"
	"google.golang.org/protobuf/proto"
)

func TestMentionedJIDs(t *testing.T) {
	ctx := &waProto.ContextInfo{MentionedJID: []string{"5562999998888@s.whatsapp.net", "123@lid"}}
	for name, m := range map[string]*waProto.Message{
		"text":  {ExtendedTextMessage: &waProto.ExtendedTextMessage{Text: proto.String("@a"), ContextInfo: ctx}},
		"image": {ImageMessage: &waProto.ImageMessage{ContextInfo: ctx}},
		"video": {VideoMessage: &waProto.VideoMessage{ContextInfo: ctx}},
		"doc":   {DocumentMessage: &waProto.DocumentMessage{ContextInfo: ctx}},
		"audio": {AudioMessage: &waProto.AudioMessage{ContextInfo: ctx}},
	} {
		if got := mentionedJIDs(m); len(got) != 2 || got[0] != "5562999998888@s.whatsapp.net" {
			t.Errorf("%s: mentions = %v", name, got)
		}
	}
	if got := mentionedJIDs(&waProto.Message{Conversation: proto.String("oi")}); len(got) != 0 {
		t.Errorf("a plain message mentions nobody, got %v", got)
	}
	if got := mentionedJIDs(nil); len(got) != 0 {
		t.Errorf("nil message: %v", got)
	}
}

func TestGroupMattersNeedTheOwnerMention(t *testing.T) {
	db := seedMatterChat(t)
	grp := "120363@g.us"
	seedActiveMsg(t, db, "gm1", grp, "@eu manda o relatorio", "", false, ts("2026-10-10T11:00:00-03:00"))
	seedActiveMsg(t, db, "gm2", grp, "alguem manda o relatorio?", "", false, ts("2026-10-10T11:05:00-03:00"))
	seedActiveMsg(t, db, "gm3", grp, "@outra pessoa manda", "", false, ts("2026-10-10T11:10:00-03:00"))
	seedActiveMsg(t, db, "gm4", grp, "@eu pelo lid", "", false, ts("2026-10-10T11:15:00-03:00"))
	for id, who := range map[string][]string{"gm1": {"999@s.whatsapp.net"}, "gm3": {"555@s.whatsapp.net"}, "gm4": {"4242@lid"}} {
		if err := storeMentions(db, grp, id, who); err != nil {
			t.Fatal(err)
		}
	}
	self := []string{"999", "4242"}
	record := func(id string) error {
		_, err := recordMatter(db, rec(grp, id, "owner", "request"), self, matterNow)
		return err
	}
	if err := record("gm1"); err != nil {
		t.Errorf("a group message that mentions the owner must be tracked: %v", err)
	}
	if err := record("gm4"); err != nil {
		t.Errorf("a mention through the owner's LID must be tracked: %v", err)
	}
	for _, id := range []string{"gm2", "gm3"} {
		if err := record(id); err == nil {
			t.Errorf("%s: a group message that does not mention the owner must be refused", id)
		}
	}
	if _, err := recordMatter(db, rec("status@broadcast", "st", "owner", "request"), self, matterNow); err == nil {
		t.Error("status must still be refused")
	}

	next, err := reviewNext(db, ReviewNextRequest{}, self, matterNow)
	if err != nil {
		t.Fatal(err)
	}
	groupIDs := map[string]bool{}
	for _, m := range next.Messages {
		if m.ChatJID == grp {
			groupIDs[m.ID] = true
		}
	}
	if len(groupIDs) != 2 || !groupIDs["gm1"] || !groupIDs["gm4"] {
		t.Errorf("review must return only the group messages that mention the owner, got %v", groupIDs)
	}
}

func TestStoreMentionsIsIdempotent(t *testing.T) {
	db := setupChatStore(t).db
	for i := 0; i < 2; i++ {
		if err := storeMentions(db, "1@g.us", "x", []string{"999@s.whatsapp.net", "999:5@s.whatsapp.net"}); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM message_mentions`).Scan(&n); err != nil || n != 1 {
		t.Errorf("mentions rows = %d (%v), want 1: the user part is what counts, device suffix ignored", n, err)
	}
}
