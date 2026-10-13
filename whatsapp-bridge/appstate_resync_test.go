package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.mau.fi/whatsmeow/appstate"
)

func TestParseAppStateName(t *testing.T) {
	for in, want := range map[string]appstate.WAPatchName{
		"regular": appstate.WAPatchRegular, "regular_low": appstate.WAPatchRegularLow, "regular_high": appstate.WAPatchRegularHigh,
		"critical_block": appstate.WAPatchCriticalBlock, "critical_unblock_low": appstate.WAPatchCriticalUnblockLow,
	} {
		if got, ok := parseAppStateName(in); !ok || got != want {
			t.Errorf("parseAppStateName(%q) = %q, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", "all", "REGULAR_LOW", "regular_low; drop", "../regular"} {
		if _, ok := parseAppStateName(bad); ok {
			t.Errorf("%q must be refused", bad)
		}
	}
}

func TestHandleAppStateResyncValidation(t *testing.T) {
	h := handleAppStateResync(nil)
	for name, c := range map[string]struct {
		method, body string
		want         int
	}{
		"get":           {http.MethodGet, "", http.StatusMethodNotAllowed},
		"bad json":      {http.MethodPost, "{", http.StatusBadRequest},
		"no name":       {http.MethodPost, `{}`, http.StatusBadRequest},
		"unknown name":  {http.MethodPost, `{"name":"everything"}`, http.StatusBadRequest},
		"not connected": {http.MethodPost, `{"name":"regular_low"}`, http.StatusServiceUnavailable},
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(c.method, "/api/appstate/resync", strings.NewReader(c.body)))
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, c.want, rec.Body.String())
		}
	}
}
