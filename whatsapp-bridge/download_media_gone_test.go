package main

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"go.mau.fi/whatsmeow"
)

func TestMediaGoneOnCDN(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"403", whatsmeow.ErrMediaDownloadFailedWith403, true},
		{"404", whatsmeow.ErrMediaDownloadFailedWith404, true},
		{"410", whatsmeow.ErrMediaDownloadFailedWith410, true},
		{"wrapped like downloadMedia does", fmt.Errorf("failed to download media: %w",
			fmt.Errorf("failed to download media from last host: %w", whatsmeow.ErrMediaDownloadFailedWith403)), true},
		{"real response with 410", fmt.Errorf("x: %w", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 410}}), true},
		{"500 is not gone", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 500}}, false},
		{"429 is not gone", whatsmeow.DownloadHTTPError{Response: &http.Response{StatusCode: 429}}, false},
		{"network error", errors.New("dial tcp: connection refused"), false},
		{"bad hash", whatsmeow.ErrInvalidMediaSHA256, false},
		{"incomplete info", errors.New("incomplete media information for download"), false},
		{"nil", nil, false},
	} {
		if got := mediaGoneOnCDN(tc.err); got != tc.want {
			t.Errorf("%s: mediaGoneOnCDN = %v, want %v", tc.name, got, tc.want)
		}
	}
}
