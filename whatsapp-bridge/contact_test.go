package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeContactPhone(t *testing.T) {
	for in, want := range map[string]string{
		"+55 (62) 99999-8888": "5562999998888",
		"5562999998888":       "5562999998888",
	} {
		if got, err := normalizeContactPhone(in); err != nil || got != want {
			t.Errorf("normalizeContactPhone(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "1234567", "1234567890123456", "55 62 9999x8888"} {
		if _, err := normalizeContactPhone(bad); err == nil {
			t.Errorf("normalizeContactPhone(%q) accepted an invalid phone", bad)
		}
	}
}

func TestBuildContactVCard(t *testing.T) {
	v := buildContactVCard("Joao Silva", "5562999998888")
	for _, want := range []string{"BEGIN:VCARD", "FN:Joao Silva", "TEL;type=CELL;waid=5562999998888:+5562999998888", "END:VCARD"} {
		if !strings.Contains(v, want) {
			t.Errorf("vcard missing %q:\n%s", want, v)
		}
	}
	// a name must not be able to add vCard lines or break the FN value
	v = buildContactVCard("A, B; C\r\nTEL:999", "5562999998888")
	tel := 0
	for _, line := range strings.Split(v, "\n") {
		if strings.HasPrefix(line, "TEL") {
			tel++
		}
	}
	if tel != 1 {
		t.Errorf("name injected a vCard line:\n%q", v)
	}
	if !strings.Contains(v, `FN:A\, B\; C\nTEL:999`) {
		t.Errorf("name not escaped:\n%q", v)
	}
}

func TestHandleSendContactValidation(t *testing.T) {
	h := handleSendContact(nil, nil)
	for name, c := range map[string]struct {
		method, body string
		want         int
	}{
		"get":           {http.MethodGet, "", http.StatusMethodNotAllowed},
		"bad json":      {http.MethodPost, "{", http.StatusBadRequest},
		"no recipient":  {http.MethodPost, `{"name":"Joao","phone_number":"5562999998888"}`, http.StatusBadRequest},
		"no name":       {http.MethodPost, `{"recipient":"5562911112222","phone_number":"5562999998888"}`, http.StatusBadRequest},
		"invalid phone": {http.MethodPost, `{"recipient":"5562911112222","name":"Joao","phone_number":"abc"}`, http.StatusBadRequest},
		"invalid jid":   {http.MethodPost, `{"recipient":"x:y:z@s.whatsapp.net","name":"Joao","phone_number":"5562999998888"}`, http.StatusBadRequest},
		"not connected": {http.MethodPost, `{"recipient":"5562911112222","name":"Joao","phone_number":"5562999998888"}`, http.StatusServiceUnavailable},
	} {
		rec := httptest.NewRecorder()
		h(rec, httptest.NewRequest(c.method, "/api/send_contact", strings.NewReader(c.body)))
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", name, rec.Code, c.want, rec.Body.String())
		}
	}
}
