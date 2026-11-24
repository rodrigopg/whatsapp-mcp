package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mediaFixture builds a bridge working directory (with store/) and a separate user directory whose
// Downloads and tmp folders stand in for the default roots (the real ones would cover t.TempDir()).
func mediaFixture(t *testing.T) (user string) {
	t.Helper()
	t.Setenv("MEDIA_ALLOWED_ROOTS", "")
	user = t.TempDir()
	old := defaultMediaRoots
	defaultMediaRoots = func() []string {
		return []string{filepath.Join(user, "Downloads"), filepath.Join(user, "tmp"), filepath.Join(user, "missing")}
	}
	t.Cleanup(func() { defaultMediaRoots = old })
	// The default install lives in a hidden folder (~/.whatsapp-mcp); received media must still be sendable.
	work := filepath.Join(t.TempDir(), ".whatsapp-mcp", "whatsapp-bridge")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Chdir(work)
	for path, body := range map[string]string{
		"store/whatsapp.db":                                         "session keys",
		"store/messages.db":                                         "history",
		"store/settings.json":                                       "{}",
		"store/5511@s.whatsapp.net/ABC_photo.jpg":                   "received",
		"store/5511@s.whatsapp.net/deeper/x.jpg":                    "not a media file",
		"store/backup/whatsapp.db":                                  "copy",
		filepath.Join(user, "report.pdf"):                           "ordinary file",
		filepath.Join(user, "Documents", "x.pdf"):                   "private document",
		filepath.Join(user, "Downloads", "a.pdf"):                   "downloaded",
		filepath.Join(user, "Downloads", "d", "b"):                  "downloaded, nested",
		filepath.Join(user, "Downloads", ".env"):                    "SECRET=2",
		filepath.Join(user, "tmp", "tmpx.ogg"):                      "converted audio",
		filepath.Join(user, "tmp", ".cache", "k"):                   "hidden in temp",
		filepath.Join(user, "other-bridge", "store", "whatsapp.db"): "someone else's session",
		filepath.Join(user, "other-bridge", "transcription.env"):    "KEY=1",
		filepath.Join(user, ".ssh", "id_ed25519"):                   "private key",
		filepath.Join(user, ".env"):                                 "SECRET=1",
		filepath.Join(user, "proj", ".git", "c.png"):                "hidden parent",
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
	big := filepath.Join(user, "Downloads", "big.bin")
	if f, err := os.Create(big); err != nil || f.Truncate(maxMediaBytes+1) != nil || f.Close() != nil {
		t.Fatal("cannot create the sparse file", err)
	}

	for _, c := range []struct {
		name, path string
		ok         bool
	}{
		{"file in Downloads", filepath.Join(user, "Downloads", "a.pdf"), true},
		{"file in a Downloads subfolder", filepath.Join(user, "Downloads", "d", "b"), true},
		{"file in the temp folder", filepath.Join(user, "tmp", "tmpx.ogg"), true},
		{"symlink elsewhere to a Downloads file", link(filepath.Join(user, "Downloads", "a.pdf"), "alias2.pdf"), true},
		{"ordinary file outside the default folders", filepath.Join(user, "report.pdf"), false},
		{"Documents", filepath.Join(user, "Documents", "x.pdf"), false},
		{"another bridge's session database", filepath.Join(user, "other-bridge", "store", "whatsapp.db"), false},
		{"another bridge's transcription.env", filepath.Join(user, "other-bridge", "transcription.env"), false},
		{"Downloads symlink to Documents", link(filepath.Join(user, "Documents", "x.pdf"), filepath.Join("Downloads", "x.pdf")), false},
		{"hidden file in Downloads", filepath.Join(user, "Downloads", ".env"), false},
		{"hidden folder in temp", filepath.Join(user, "tmp", ".cache", "k"), false},
		{"a default root itself", filepath.Join(user, "Downloads"), false},
		{"received media, absolute (what download_media returns)", abs("store/5511@s.whatsapp.net/ABC_photo.jpg"), true},
		{"received media, relative", "store/5511@s.whatsapp.net/ABC_photo.jpg", true},
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
		// The caller gets the reason, never a path of this machine.
		if err != nil && (strings.Contains(err.Error(), user) || strings.Contains(err.Error(), "store"+string(os.PathSeparator))) {
			t.Errorf("%s: error text carries a path: %v", c.name, err)
		}
	}
	if _, err := readOutboundMedia(filepath.Join(user, "Documents", "x.pdf")); err == nil || !strings.Contains(err.Error(), "MEDIA_ALLOWED_ROOTS") {
		t.Errorf("refusal does not name MEDIA_ALLOWED_ROOTS: %v", err)
	}
	if runtime.GOOS == "linux" {
		if _, err := readOutboundMedia("/proc/self/environ"); err == nil {
			t.Error("/proc/self/environ accepted")
		}
	}
}

func TestDefaultMediaRoots(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	roots := defaultMediaRoots()
	for _, want := range []string{os.TempDir(), filepath.Join(home, "Downloads")} {
		if !slices.Contains(roots, want) {
			t.Errorf("default roots %v lack %s", roots, want)
		}
	}
	if runtime.GOOS != "windows" && !slices.Contains(roots, "/tmp") {
		t.Errorf("default roots %v lack /tmp", roots)
	}
	writeDirs := func(line string) {
		os.MkdirAll(filepath.Join(home, "cfg"), 0o700)
		os.WriteFile(filepath.Join(home, "cfg", "user-dirs.dirs"), []byte("# comment\nXDG_DESKTOP_DIR=\"$HOME/Desktop\"\n"+line+"\n"), 0o600)
	}
	writeDirs(`XDG_DOWNLOAD_DIR="$HOME/Transferências"`)
	if roots := defaultMediaRoots(); !slices.Contains(roots, filepath.Join(home, "Transferências")) {
		t.Errorf("XDG download folder not honoured: %v", roots)
	}
	// xdg-user-dirs writes "$HOME/" for a folder that does not exist: that must not open the whole home.
	writeDirs(`XDG_DOWNLOAD_DIR="$HOME/"`)
	for _, r := range defaultMediaRoots() {
		if r != os.TempDir() && r != "/tmp" && within(r, home) {
			t.Errorf("default root %s covers the home folder", r)
		}
	}
}

// A path component that is a symlink at open time is refused, whatever it points to: the path was
// already resolved, so a link there means it was swapped after the check.
func TestOpenNoLinks(t *testing.T) {
	user := mediaFixture(t)
	dl := filepath.Join(user, "Downloads")
	if err := os.Symlink(filepath.Join(dl, "a.pdf"), filepath.Join(dl, "link.pdf")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	os.Symlink(filepath.Join(dl, "d"), filepath.Join(dl, "dlink"))
	for _, parts := range [][]string{{"link.pdf"}, {"dlink", "b"}, {"d"}, {"nope"}} {
		if f, err := openNoLinks(dl, parts, false); err == nil {
			f.Close()
			t.Errorf("%v: opened", parts)
		}
	}
	work, _ := os.Getwd()
	if f, err := openNoLinks(work, []string{"store", "whatsapp.db"}, false); err == nil {
		f.Close()
		t.Error("walked into the store from an outer root")
	}
	f, err := openNoLinks(dl, []string{"d", "b"}, false)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
}

// While a file in an allowed folder keeps being swapped for a symlink to the session database, no
// read may ever return the database.
func TestReadOutboundMediaSwapRace(t *testing.T) {
	user := mediaFixture(t)
	secret, _ := filepath.Abs("store/whatsapp.db")
	target := filepath.Join(user, "Downloads", "photo.jpg")
	tmp := filepath.Join(user, "Downloads", "swap")
	if err := os.Symlink(secret, tmp); err != nil {
		t.Skip("no symlinks here:", err)
	}
	os.Remove(tmp)
	var stop atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for !stop.Load() {
			os.WriteFile(tmp, []byte("harmless"), 0o600)
			os.Rename(tmp, target)
			os.Symlink(secret, tmp)
			os.Rename(tmp, target)
		}
	}()
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if data, err := readOutboundMedia(target); err == nil && string(data) != "harmless" {
			t.Errorf("read %q through a swapped symlink", data)
			break
		}
	}
	stop.Store(true)
	<-done
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
