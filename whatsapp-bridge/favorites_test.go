package main

import (
	"database/sql"
	"fmt"
	"reflect"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

var favNow = ts("2026-10-12T09:00:00-03:00")

// talk seeds `in` messages from the contact and `out` from the owner, days ago.
func talk(t *testing.T, db *sql.DB, jid, name string, in, out int, daysAgo int) {
	t.Helper()
	if _, err := db.Exec(`INSERT OR IGNORE INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)`, jid, name, favNow); err != nil {
		t.Fatal(err)
	}
	at := favNow.AddDate(0, 0, -daysAgo)
	for i := 0; i < in+out; i++ {
		seedActiveMsg(t, db, fmt.Sprintf("%s-%d", jid, i), jid, "oi", "", i >= in, at.Add(time.Duration(i)*time.Minute))
	}
}

func favNames(fs []FavoriteSuggestion) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.Name)
	}
	return out
}

func TestSuggestFavorites(t *testing.T) {
	db := setupChatStore(t).db
	talk(t, db, "1@s.whatsapp.net", "Ana", 4, 4, 3)
	talk(t, db, "2@s.whatsapp.net", "Vendedor", 10, 0, 3) // never answered: not a conversation
	talk(t, db, "3@s.whatsapp.net", "Caio", 3, 3, 5)
	talk(t, db, "4@s.whatsapp.net", "Dora", 2, 5, 3) // too little from her
	talk(t, db, "120363@g.us", "Familia", 5, 5, 3)   // group
	talk(t, db, "999@s.whatsapp.net", "Eu", 5, 5, 3) // own chat
	talk(t, db, "5@s.whatsapp.net", "Edu", 5, 5, 3)  // already a favorite
	talk(t, db, "6@s.whatsapp.net", "Fabi", 5, 5, 3) // rejected before
	talk(t, db, "7@s.whatsapp.net", "Gil", 5, 5, 60) // older than a month
	talk(t, db, "8@s.whatsapp.net", "Hugo", 4, 3, 2)
	for jid, st := range map[string]string{"5@s.whatsapp.net": "favorite", "6@s.whatsapp.net": "rejected"} {
		if _, err := db.Exec(`INSERT INTO favorites (chat_jid, status, source, decided_at) VALUES (?, ?, 'owner', ?)`, jid, st, favNow); err != nil {
			t.Fatal(err)
		}
	}
	got, err := suggestFavorites(db, SuggestFavoritesRequest{}, []string{"999"}, favNow)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"Ana", "Hugo", "Caio"}; !reflect.DeepEqual(favNames(got), want) {
		t.Fatalf("suggestions = %v, want %v", favNames(got), want)
	}
	if got[0].Incoming != 4 || got[0].Outgoing != 4 {
		t.Errorf("counts = %d/%d, want 4/4", got[0].Incoming, got[0].Outgoing)
	}
	if one, _ := suggestFavorites(db, SuggestFavoritesRequest{Limit: 1}, []string{"999"}, favNow); len(one) != 1 {
		t.Errorf("limit 1 returned %d", len(one))
	}
	empty := setupChatStore(t).db
	if got, err := suggestFavorites(empty, SuggestFavoritesRequest{}, nil, favNow); err != nil || got == nil || len(got) != 0 {
		t.Errorf("no conversations: got %v, %v; want an empty list", got, err)
	}
}

func TestDecideAndListFavorites(t *testing.T) {
	db := setupChatStore(t).db
	talk(t, db, "1@s.whatsapp.net", "Ana", 4, 4, 3)
	talk(t, db, "120363@g.us", "Familia", 5, 5, 3)
	decide := func(jid, d string) error {
		_, err := decideFavorite(db, DecideFavoriteRequest{ChatJID: jid, Decision: d}, []string{"999"}, favNow)
		return err
	}
	if err := decide("1@s.whatsapp.net", "accept"); err != nil {
		t.Fatal(err)
	}
	if got, _ := listFavorites(db); len(got) != 1 || got[0].Name != "Ana" || got[0].Source != "owner" {
		t.Fatalf("favorites = %+v", got)
	}
	if err := decide("1@s.whatsapp.net", "reject"); err != nil {
		t.Fatal(err)
	}
	if got, _ := listFavorites(db); len(got) != 0 {
		t.Errorf("a rejected contact must not be a favorite: %+v", got)
	}
	if sug, _ := suggestFavorites(db, SuggestFavoritesRequest{}, nil, favNow); len(sug) != 0 {
		t.Errorf("a rejected contact must not be suggested again: %v", favNames(sug))
	}
	if err := decide("1@s.whatsapp.net", "remove"); err != nil {
		t.Fatal(err)
	}
	if sug, _ := suggestFavorites(db, SuggestFavoritesRequest{}, nil, favNow); len(sug) != 1 {
		t.Errorf("after remove the decision is forgotten, got %v", favNames(sug))
	}
	for name, c := range map[string][2]string{"group": {"120363@g.us", "accept"}, "own chat": {"999@s.whatsapp.net", "accept"}, "bad decision": {"1@s.whatsapp.net", "maybe"}} {
		if err := decide(c[0], c[1]); err == nil {
			t.Errorf("%s accepted", name)
		} else if _, ok := err.(*errInvalidRequest); !ok {
			t.Errorf("%s: error %T", name, err)
		}
	}
}

func TestMirrorWhatsAppFavorites(t *testing.T) {
	db := setupChatStore(t).db
	talk(t, db, "1@s.whatsapp.net", "Ana", 1, 1, 3)
	talk(t, db, "2@s.whatsapp.net", "Bia", 1, 1, 3)
	talk(t, db, "3@s.whatsapp.net", "Caio", 1, 1, 3)
	if _, err := decideFavorite(db, DecideFavoriteRequest{ChatJID: "3@s.whatsapp.net", Decision: "accept"}, nil, favNow); err != nil {
		t.Fatal(err)
	}
	src := func() map[string]string {
		m := map[string]string{}
		fs, _ := listFavorites(db)
		for _, f := range fs {
			m[f.Name] = f.Source
		}
		return m
	}
	if err := mirrorWhatsAppFavorites(db, []string{"1@s.whatsapp.net", "2@s.whatsapp.net", "3@s.whatsapp.net"}, favNow); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"Ana": "whatsapp", "Bia": "whatsapp", "Caio": "owner"}; !reflect.DeepEqual(src(), want) {
		t.Errorf("after first mirror: %v, want %v", src(), want)
	}
	// The WhatsApp list is replaced as a whole; what the owner accepted stays.
	if err := mirrorWhatsAppFavorites(db, []string{"2@s.whatsapp.net"}, favNow); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"Bia": "whatsapp", "Caio": "owner"}; !reflect.DeepEqual(src(), want) {
		t.Errorf("after second mirror: %v, want %v", src(), want)
	}
	if err := mirrorWhatsAppFavorites(db, nil, favNow); err != nil || !reflect.DeepEqual(src(), map[string]string{"Caio": "owner"}) {
		t.Errorf("an empty WhatsApp list clears only its own: %v, %v", src(), err)
	}
}

func TestFavoritesFromAppState(t *testing.T) {
	evt := &events.AppState{Index: []string{"favorites"}, SyncActionValue: &waSyncAction.SyncActionValue{
		FavoritesAction: &waSyncAction.FavoritesAction{Favorites: []*waSyncAction.FavoritesAction_Favorite{
			{ID: proto.String("5562999998888@s.whatsapp.net")}, {ID: proto.String("5562911112222")}}}}}
	got, ok := favoritesFromAppState(evt)
	if !ok || !reflect.DeepEqual(got, []string{"5562999998888@s.whatsapp.net", "5562911112222@s.whatsapp.net"}) {
		t.Errorf("got %v, %v", got, ok)
	}
	if _, ok := favoritesFromAppState(&events.AppState{Index: []string{"pin_v1"}, SyncActionValue: evt.SyncActionValue}); ok {
		t.Error("another index must not be read as favorites")
	}
	if got, ok := favoritesFromAppState(&events.AppState{Index: []string{"favorites"}, SyncActionValue: &waSyncAction.SyncActionValue{}}); !ok || len(got) != 0 {
		t.Errorf("an empty list is still the favorites list: %v, %v", got, ok)
	}
}
