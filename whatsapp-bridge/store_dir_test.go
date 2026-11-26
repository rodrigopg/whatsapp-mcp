package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/store/sqlstore"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// A WHATSAPP_STORE_DIR exported in the developer's shell must never point the tests at a real store.
func TestMain(m *testing.M) {
	if err := os.Unsetenv("WHATSAPP_STORE_DIR"); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func writeStoreFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestStorePathDefaultKeepsLayout(t *testing.T) {
	if got := storePath(); got != "store" {
		t.Errorf("storePath() = %q, want store", got)
	}
	if got := storeDSN("messages.db", "_foreign_keys=on"); got != "file:store/messages.db?_foreign_keys=on" {
		t.Errorf("storeDSN = %q", got)
	}
	for got, want := range map[string]string{
		qrImagePath(): filepath.Join("store", "whatsapp-qr.png"), settingsPath(): filepath.Join("store", "settings.json"),
		loggedOutMarker(): filepath.Join("store", "logged_out"),
	} {
		if got != want {
			t.Errorf("path = %q, want %q", got, want)
		}
	}
}

// Databases, settings, the logout marker, the pairing QR and chat media folders all live in the
// configured store, and nothing is created in the working directory's store/.
func TestStoreDirHoldsEveryStoreFile(t *testing.T) {
	external := filepath.Join(t.TempDir(), "external [data] #%")
	if runtime.GOOS != "windows" {
		external += "?"
	}
	for _, configured := range []string{"", external} {
		t.Run(configured, func(t *testing.T) {
			t.Setenv("WHATSAPP_STORE_DIR", configured)
			s := setupChatStore(t)
			root := configured
			if root == "" {
				root = "store"
			}
			if _, err := os.Stat(filepath.Join(root, "messages.db")); err != nil {
				t.Fatalf("messages.db not in the store: %v", err)
			}
			if err := s.EnsureChat("probe@s.whatsapp.net", time.Now()); err != nil {
				t.Fatal(err)
			}
			read, err := openUnaccentMessagesDB()
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			var jid string
			if err := read.QueryRow("SELECT jid FROM chats").Scan(&jid); err != nil || jid != "probe@s.whatsapp.net" {
				t.Fatalf("messages read connection: %q, %v", jid, err)
			}

			container, err := sqlstore.New(context.Background(), "sqlite3", storeDSN("whatsapp.db", "_foreign_keys=on"), waLog.Noop)
			if err != nil {
				t.Fatal(err)
			}
			defer container.Close()
			session, err := openStoreDBReadOnly()
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if err := session.QueryRow("SELECT COUNT(*) FROM whatsmeow_device").Scan(new(int)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(root, "whatsapp.db")); err != nil {
				t.Fatalf("whatsapp.db not in the store: %v", err)
			}

			st, err := loadSettings(settingsPath())
			if err != nil {
				t.Fatal(err)
			}
			if err := st.update(func(s *settings) { s.ReadOnly = true }); err != nil {
				t.Fatal(err)
			}
			markLoggedOut()
			for _, name := range []string{"settings.json", "logged_out"} {
				if _, err := os.Stat(filepath.Join(root, name)); err != nil {
					t.Errorf("%s not in the store: %v", name, err)
				}
			}
			if loggedOutAt().IsZero() {
				t.Error("logout marker not read back from the store")
			}
			clearLoggedOut()
			if got := qrImagePath(); got != filepath.Join(root, "whatsapp-qr.png") {
				t.Errorf("qrImagePath() = %q", got)
			}
			if dir, err := chatMediaDir("5511@s.whatsapp.net"); err != nil || dir != filepath.Join(root, "5511@s.whatsapp.net") {
				t.Errorf("chatMediaDir = %q, %v", dir, err)
			}
			if configured != "" {
				if _, err := os.Stat("store"); !os.IsNotExist(err) {
					t.Fatalf("default store created: %v", err)
				}
			}
		})
	}
}

func TestStoreDirRejectsRelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, root := range []string{"relative data", "~/literal", "../outside"} {
		t.Run(root, func(t *testing.T) {
			t.Setenv("WHATSAPP_STORE_DIR", root)
			s, err := NewMessageStore()
			if s != nil {
				s.Close()
			}
			if err == nil || !strings.Contains(err.Error(), "WHATSAPP_STORE_DIR must be an absolute path") {
				t.Fatalf("relative store accepted: %v", err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Errorf("relative store created: %v", err)
			}
		})
	}
}

// LID media folders are found and moved inside the configured store, even when its path holds
// glob metacharacters.
func TestStoreDirLIDMedia(t *testing.T) {
	root := filepath.Join(t.TempDir(), "external [store]")
	t.Setenv("WHATSAPP_STORE_DIR", root)
	s := setupChatStore(t)
	writeStoreFile(t, filepath.Join(root, testLID, "msg_audio.ogg"), "media")
	migrateLIDChats(lidClient(map[string]string{"999": "222"}), s, waLog.Noop)
	if data, err := os.ReadFile(filepath.Join(root, testPN, "msg_audio.ogg")); err != nil || string(data) != "media" {
		t.Fatalf("LID media not moved: %q, %v", data, err)
	}
}

// /api/download/file serves media from the configured store.
func TestStoreDirDownloadFile(t *testing.T) {
	root := filepath.Join(t.TempDir(), "data")
	t.Setenv("WHATSAPP_STORE_DIR", root)
	s := setupChatStore(t)
	const chat, id = "5511@s.whatsapp.net", "ABC123"
	seedMedia(t, s, chat, id, "photo.jpg")
	writeStoreFile(t, filepath.Join(root, chat, id+"_photo.jpg"), "received")
	h := downloadFileHandler(func(id, chat string) (bool, string, string, string, error) {
		return downloadMedia(nil, s, id, chat)
	})
	w := httptest.NewRecorder()
	h(w, downloadRequest(id, chat))
	if w.Code != http.StatusOK || w.Body.String() != "received" {
		t.Fatalf("status %d body %q", w.Code, w.Body)
	}
}

func TestStoreDirTighten(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix modes")
	}
	root := filepath.Join(t.TempDir(), "data")
	t.Setenv("WHATSAPP_STORE_DIR", root)
	media := writeStoreFile(t, filepath.Join(root, "5511@s.whatsapp.net", "a.jpg"), "x")
	os.Chmod(root, 0o755)
	os.Chmod(filepath.Dir(media), 0o755)
	os.Chmod(media, 0o644)
	tightenStore()
	for path, want := range map[string]os.FileMode{root: 0o700, filepath.Dir(media): 0o700, media: 0o600} {
		if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v, %v; want %o", path, fi, err, want)
		}
	}
}

// The media send policy treats the configured store as the store: downloaded media may be sent,
// the databases and settings never, even through MEDIA_ALLOWED_ROOTS.
func TestStoreDirMediaPolicy(t *testing.T) {
	mediaFixture(t)
	root := filepath.Join(t.TempDir(), "data")
	t.Setenv("WHATSAPP_STORE_DIR", root)
	media := writeStoreFile(t, filepath.Join(root, "5511@s.whatsapp.net", "ABC_photo.jpg"), "received")
	secrets := []string{
		writeStoreFile(t, filepath.Join(root, "whatsapp.db"), "session keys"),
		writeStoreFile(t, filepath.Join(root, "messages.db-wal"), "history"),
		writeStoreFile(t, filepath.Join(root, "settings.json"), "{}"),
	}
	for _, allowed := range []string{"", root, filepath.Dir(root)} {
		t.Setenv("MEDIA_ALLOWED_ROOTS", allowed)
		if data, err := readOutboundMedia(media); err != nil || string(data) != "received" {
			t.Errorf("roots %q: downloaded media = %q, %v", allowed, data, err)
		}
		for _, path := range secrets {
			if _, err := readOutboundMedia(path); err == nil {
				t.Errorf("roots %q: %s accepted", allowed, path)
			}
		}
	}
}

// The transcription sweep runs transcribe.py from its own folder, so it must hand over the store
// as an absolute path. Run in a child process: the sweep goroutine cannot be stopped.
func TestStoreDirSweepPassesAbsolutePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake Python executable")
	}
	if os.Getenv("WHATSAPP_TEST_SWEEP") == "1" {
		root := t.TempDir()
		bridge := filepath.Join(root, "whatsapp-bridge")
		bin := filepath.Join(root, "whatsapp-mcp-server", ".venv", "bin")
		for _, dir := range []string{bridge, bin} {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		t.Chdir(bridge)
		t.Setenv("TMPDIR", root)
		probe := filepath.Join(root, "probe")
		t.Setenv("WHATSAPP_TEST_PROBE", probe)
		configured := os.Getenv("WHATSAPP_TEST_SWEEP_STORE")
		t.Setenv("WHATSAPP_STORE_DIR", configured)
		for path, data := range map[string]string{
			filepath.Join(bin, "python3"):                               "#!/bin/sh\nprintf '%s' \"$WHATSAPP_STORE_DIR\" > \"$WHATSAPP_TEST_PROBE\"\n",
			filepath.Join(root, "whatsapp-mcp-server", "transcribe.py"): "",
		} {
			if err := os.WriteFile(path, []byte(data), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		want := configured
		if want == "" {
			want = filepath.Join(bridge, "store")
		}
		startTranscriptionSweep(20 * time.Millisecond)
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if data, err := os.ReadFile(probe); err == nil && string(data) == want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		data, err := os.ReadFile(probe)
		t.Fatalf("sweep store = %q, %v; want %q", data, err, want)
	}

	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, configured := range []string{"", filepath.Join(t.TempDir(), "external")} {
		t.Run(configured, func(t *testing.T) {
			// #nosec G204 -- re-runs this test binary
			cmd := exec.Command(binary, "-test.run=^TestStoreDirSweepPassesAbsolutePath$")
			cmd.Env = append(os.Environ(), "WHATSAPP_TEST_SWEEP=1", "WHATSAPP_TEST_SWEEP_STORE="+configured)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("child sweep: %v\n%s", err, out)
			}
		})
	}
}

// TestMain clears an inherited WHATSAPP_STORE_DIR, so a test never writes into it.
func TestStoreDirIgnoresInheritedEnvironment(t *testing.T) {
	if os.Getenv("WHATSAPP_TEST_STORE_ISOLATION") == "1" {
		setupChatStore(t)
		return
	}
	external := t.TempDir()
	marker := writeStoreFile(t, filepath.Join(external, "keep"), "untouched")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// #nosec G204 -- re-runs this test binary
	cmd := exec.Command(binary, "-test.run=^TestStoreDirIgnoresInheritedEnvironment$")
	cmd.Env = append(os.Environ(), "WHATSAPP_STORE_DIR="+external, "WHATSAPP_TEST_STORE_ISOLATION=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child test: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(external)
	if err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatalf("inherited store was touched: %v, %v", entries, err)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "untouched" {
		t.Fatalf("marker changed: %q, %v", data, err)
	}
}
