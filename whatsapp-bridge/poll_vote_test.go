package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestValidateVote(t *testing.T) {
	opts := []string{"pizza", "sushi", "salad"}
	ok := map[string]struct {
		chosen     []string
		selectable int
		want       []string
	}{
		"one":            {[]string{"pizza"}, 1, []string{"pizza"}},
		"two of three":   {[]string{"sushi", "pizza"}, 2, []string{"sushi", "pizza"}},
		"retract (none)": {nil, 1, []string{}},
		"trimmed":        {[]string{" pizza "}, 1, []string{"pizza"}},
	}
	for name, c := range ok {
		got, err := validateVote(opts, c.selectable, c.chosen)
		if err != nil || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, %v; want %v", name, got, err, c.want)
		}
	}
	bad := map[string]struct {
		chosen     []string
		selectable int
	}{
		"unknown option": {[]string{"tacos"}, 1},
		"too many":       {[]string{"pizza", "sushi"}, 1},
		"duplicate":      {[]string{"pizza", "pizza"}, 2},
		"case differs":   {[]string{"Pizza"}, 1},
	}
	for name, c := range bad {
		if _, err := validateVote(opts, c.selectable, c.chosen); err == nil {
			t.Errorf("%s accepted", name)
		} else if _, isInvalid := err.(*errInvalidRequest); !isInvalid {
			t.Errorf("%s: error %T, want *errInvalidRequest", name, err)
		}
	}
}

func TestHandlePollVoteValidation(t *testing.T) {
	h := handlePollVote(nil, nil)
	for name, c := range map[string]struct {
		method, body string
		want         int
	}{
		"get":           {http.MethodGet, "", http.StatusMethodNotAllowed},
		"bad json":      {http.MethodPost, "{", http.StatusBadRequest},
		"no chat":       {http.MethodPost, `{"message_id":"m","options":["a"]}`, http.StatusBadRequest},
		"no message":    {http.MethodPost, `{"chat_jid":"1@s.whatsapp.net","options":["a"]}`, http.StatusBadRequest},
		"invalid jid":   {http.MethodPost, `{"chat_jid":"x:y:z@s.whatsapp.net","message_id":"m","options":["a"]}`, http.StatusBadRequest},
		"not connected": {http.MethodPost, `{"chat_jid":"1@s.whatsapp.net","message_id":"m","options":["a"]}`, http.StatusServiceUnavailable},
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(c.method, "/api/poll_vote", strings.NewReader(c.body)))
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, c.want, rec.Body.String())
		}
	}
}
