package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mediaFixture builds a bridge working directory (with store/) and a separate user directory.
func mediaFixture(t *testing.T) (user string) {
	t.Helper()
	t.Setenv("MEDIA_ALLOWED_ROOTS", "")
	// The default install lives in a hidden folder (~/.whatsapp-mcp); received media must still be sendable.
	work := filepath.Join(t.TempDir(), ".whatsapp-mcp", "whatsapp-bridge")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	user = t.TempDir()
	t.Chdir(work)
	for path, body := range map[string]string{
		"store/whatsapp.db":                          "session keys",
		"store/messages.db":                          "history",
		"store/settings.json":                        "{}",
		"store/5511@s.whatsapp.net/ABC_photo.jpg":    "received",
		"store/5511@s.whatsapp.net/deeper/x.jpg":     "not a media file",
		"store/backup/whatsapp.db":                   "copy",
		filepath.Join(user, "report.pdf"):            "ordinary file",
		filepath.Join(user, ".ssh", "id_ed25519"):    "private key",
		filepath.Join(user, ".env"):                  "SECRET=1",
		filepath.Join(user, "proj", ".git", "c.png"): "hidden parent",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return user
}

func TestReadOutboundMedia(t *testing.T) {
	user := mediaFixture(t)
	abs := func(p string) string { a, _ := filepath.Abs(p); return a }
	link := func(target, name string) string {
		l := filepath.Join(user, name)
		if err := os.Symlink(target, l); err != nil {
			t.Skip("no symlinks here:", err)
		}
		return l
	}
	big := filepath.Join(user, "big.bin")
	if f, err := os.Create(big); err != nil || f.Truncate(maxMediaBytes+1) != nil || f.Close() != nil {
		t.Fatal("cannot create the sparse file", err)
	}

	for _, c := range []struct {
		name, path string
		ok         bool
	}{
		{"ordinary file by absolute path", filepath.Join(user, "report.pdf"), true},
		{"received media, absolute (what download_media returns)", abs("store/5511@s.whatsapp.net/ABC_photo.jpg"), true},
		{"received media, relative", "store/5511@s.whatsapp.net/ABC_photo.jpg", true},
		{"symlink to an ordinary file", link(filepath.Join(user, "report.pdf"), "alias.pdf"), true},
		{"session database", abs("store/whatsapp.db"), false},
		{"session database, relative", "store/whatsapp.db", false},
		{"session database, dot-dot", "store/5511@s.whatsapp.net/../whatsapp.db", false},
		{"message database", abs("store/messages.db"), false},
		{"settings", abs("store/settings.json"), false},
		{"store subfolder that is not a chat", abs("store/backup/whatsapp.db"), false},
		{"deeper than a chat folder", abs("store/5511@s.whatsapp.net/deeper/x.jpg"), false},
		{"symlink into store", link(abs("store/whatsapp.db"), "innocent.jpg"), false},
		{"symlinked directory into store", filepath.Join(link(abs("store"), "pics"), "whatsapp.db"), false},
		{"hidden directory", filepath.Join(user, ".ssh", "id_ed25519"), false},
		{"hidden file", filepath.Join(user, ".env"), false},
		{"hidden parent", filepath.Join(user, "proj", ".git", "c.png"), false},
		{"symlink to a hidden file", link(filepath.Join(user, ".env"), "notes.txt"), false},
		{"directory", user, false},
		{"missing", filepath.Join(user, "nope.jpg"), false},
		{"over the size cap", big, false},
	} {
		data, err := readOutboundMedia(c.path)
		if c.ok && (err != nil || len(data) == 0) {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: accepted (%d bytes)", c.name, len(data))
		}
	}
}

func TestReadOutboundMediaAllowedRoots(t *testing.T) {
	user := mediaFixture(t)
	other := t.TempDir()
	os.WriteFile(filepath.Join(other, "x.pdf"), []byte("x"), 0o600)
	t.Setenv("MEDIA_ALLOWED_ROOTS", strings.Join([]string{filepath.Join(user, "proj"), filepath.Join(user, "missing")}, string(os.PathListSeparator)))
	os.WriteFile(filepath.Join(user, "proj", "ok.pdf"), []byte("ok"), 0o600)
	os.WriteFile(filepath.Join(user, "proj-other.pdf"), []byte("sibling"), 0o600)

	for path, ok := range map[string]bool{
		filepath.Join(user, "proj", "ok.pdf"):        true,
		filepath.Join(user, "proj", ".git", "c.png"): true, // the owner opted this root in, hidden or not
		"store/5511@s.whatsapp.net/ABC_photo.jpg":    true, // forwarding received media needs no root
		filepath.Join(user, "report.pdf"):            false,
		filepath.Join(other, "x.pdf"):                false,
		filepath.Join(user, "proj-other.pdf"):        false,
		"store/whatsapp.db":                          false,
	} {
		if _, err := readOutboundMedia(path); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", path, err, ok)
		}
	}

	// A root that covers the store never opens the session files.
	work, _ := os.Getwd()
	t.Setenv("MEDIA_ALLOWED_ROOTS", work)
	if _, err := readOutboundMedia("store/whatsapp.db"); err == nil {
		t.Error("store/whatsapp.db accepted through an allowed root")
	}
}

func TestTightenStore(t *testing.T) {
	mediaFixture(t)
	os.Chmod("store", 0o755)
	os.Chmod("store/5511@s.whatsapp.net", 0o755)
	os.Chmod("store/whatsapp.db", 0o644)
	os.Chmod("store/5511@s.whatsapp.net/ABC_photo.jpg", 0o644)
	tightenStore()
	for path, want := range map[string]os.FileMode{
		"store": 0o700, "store/5511@s.whatsapp.net": 0o700,
		"store/whatsapp.db": 0o600, "store/5511@s.whatsapp.net/ABC_photo.jpg": 0o600,
	} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want && os.PathSeparator == '/' {
			t.Errorf("%s: mode %o, want %o", path, got, want)
		}
	}
}
