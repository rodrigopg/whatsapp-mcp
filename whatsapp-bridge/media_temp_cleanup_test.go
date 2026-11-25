package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestStaleMediaTempsAreRemoved(t *testing.T) {
	dir := t.TempDir()
	old, fresh, other := filepath.Join(dir, ".media-old.tmp"), filepath.Join(dir, ".media-new.tmp"), filepath.Join(dir, "id_photo.jpg")
	for _, p := range []string{old, fresh, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * staleMediaTemp)
	for _, p := range []string{old, other} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatal(err)
		}
	}
	removeStaleMediaTemps(dir)
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("stale temp file kept: %v", err)
	}
	for _, p := range []string{fresh, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s removed: %v", filepath.Base(p), err)
		}
	}
}
