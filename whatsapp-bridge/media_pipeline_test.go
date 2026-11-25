package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.mau.fi/whatsmeow/util/gcmutil"
	"go.mau.fi/whatsmeow/util/hkdfutil"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

type pipelineDownloader struct {
	data  []byte
	calls atomic.Int32
	write func(whatsmeow.File) error
}

func (d *pipelineDownloader) DownloadToFile(_ context.Context, _ whatsmeow.DownloadableMessage, f whatsmeow.File) error {
	d.calls.Add(1)
	if d.write != nil {
		return d.write(f)
	}
	_, err := f.Write(d.data)
	return err
}
func pipelineMedia(t *testing.T) (*MessageStore, string, string, []byte) {
	t.Helper()
	s := setupChatStore(t)
	chat := "111@s.whatsapp.net"
	data := []byte("complete media contents")
	hash := sha256.Sum256(data)
	if err := s.EnsureChat(chat, time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := s.StoreMessage("media", chat, "111", "", time.Unix(1, 0), false, "audio", "clip.ogg", "https://example.invalid/media", []byte("key"), hash[:], []byte("enc"), uint64(len(data))); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("store", chat, "media_clip.ogg")
	return s, chat, path, data
}
func TestMediaCacheIntegrity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cache []byte
		calls int32
	}{
		{"truncated", []byte("short"), 1},
		{"wrong hash", bytes.Repeat([]byte("x"), len("complete media contents")), 1},
		{"complete", []byte("complete media contents"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, chat, path, data := pipelineMedia(t)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, tc.cache, 0600); err != nil {
				t.Fatal(err)
			}
			d := &pipelineDownloader{data: data}
			ok, _, _, got, err := downloadMedia(d, s, "media", chat)
			if !ok || err != nil {
				t.Fatalf("download: %v %v", ok, err)
			}
			contents, err := os.ReadFile(got)
			if err != nil || !bytes.Equal(contents, data) || d.calls.Load() != tc.calls {
				t.Fatalf("cache=%q calls=%d err=%v", contents, d.calls.Load(), err)
			}
		})
	}
}
func TestMediaDownloadAtomicAndConcurrent(t *testing.T) {
	s, chat, path, data := pipelineMedia(t)
	started, finish := make(chan struct{}), make(chan struct{})
	d := &pipelineDownloader{data: data, write: func(f whatsmeow.File) error {
		if _, err := f.Write(data[:4]); err != nil {
			return err
		}
		close(started)
		<-finish
		_, err := f.Write(data[4:])
		return err
	}}
	done := make(chan error, 2)
	go func() { _, _, _, _, err := downloadMedia(d, s, "media", chat); done <- err }()
	select {
	case <-started:
	case err := <-done:
		t.Fatalf("streaming writer not used: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("download stalled")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("partial target is visible: %v", err)
	}
	go func() { _, _, _, _, err := downloadMedia(d, s, "media", chat); done <- err }()
	close(finish)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, data) || d.calls.Load() != 1 {
		t.Fatalf("result=%q calls=%d err=%v", got, d.calls.Load(), err)
	}
	info, _ := os.Stat(path)
	dir, _ := os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0600 || dir.Mode().Perm() != 0700 {
		t.Fatalf("permissions: %o %o", info.Mode().Perm(), dir.Mode().Perm())
	}
	files, _ := os.ReadDir(filepath.Dir(path))
	if len(files) != 1 {
		t.Fatalf("temporary files left: %v", files)
	}
}
func TestMediaDownloadFailureKeepsCache(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(map[bool]string{false: "network", true: "integrity"}[invalid], func(t *testing.T) {
			s, chat, path, data := pipelineMedia(t)
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			d := &pipelineDownloader{data: data, write: func(f whatsmeow.File) error {
				_, err := f.Write([]byte("partial"))
				if err != nil {
					return err
				}
				if invalid {
					return nil
				}
				return errors.New("network failed")
			}}
			_, _, _, _, err := downloadMedia(d, s, "media", chat)
			if err == nil {
				t.Fatal("expected failure")
			}
			got, _ := os.ReadFile(path)
			files, _ := os.ReadDir(filepath.Dir(path))
			if string(got) != "old" || len(files) != 1 {
				t.Fatalf("cache=%q files=%v", got, files)
			}
		})
	}
}

type pipelineRetryClient struct {
	calls atomic.Int32
	err   error
}

func (c *pipelineRetryClient) SendMediaRetryReceipt(context.Context, *types.MessageInfo, []byte) error {
	c.calls.Add(1)
	return c.err
}
func TestMediaRetryCooldown(t *testing.T) {
	old := mediaRetryCache
	mediaRetryCache = &retryCache{m: make(map[string]mediaRetryEntry)}
	t.Cleanup(func() { mediaRetryCache = old })
	s, chat, _, _ := pipelineMedia(t)
	client := &pipelineRetryClient{}
	if err := requestMediaRetry(client, s, "media", chat); err != nil {
		t.Fatal(err)
	}
	err := requestMediaRetry(client, s, "media", chat)
	if err == nil || !strings.Contains(err.Error(), "already") || client.calls.Load() != 1 {
		t.Fatalf("cooldown: calls=%d err=%v", client.calls.Load(), err)
	}
}
func TestMediaRetryCacheCleansOnInsert(t *testing.T) {
	c := &retryCache{m: make(map[string]mediaRetryEntry)}
	c.store("old", mediaRetryEntry{chatJID: "chat", storedAt: time.Now().Add(-2 * time.Hour)})
	c.store("new", mediaRetryEntry{chatJID: "chat", storedAt: time.Now()})
	if len(c.m) != 1 {
		t.Fatalf("expired pending entries: %d", len(c.m))
	}
}

func TestMediaRetryCooldownAfterResponseAndExpiry(t *testing.T) {
	c := &retryCache{m: make(map[string]mediaRetryEntry)}
	now := time.Now()
	entry := mediaRetryEntry{chatJID: "chat", storedAt: now, mediaKey: []byte("key")}
	if !c.store("id", entry) {
		t.Fatal("first request suppressed")
	}
	if _, ok := c.consume("chat", "id", anyEntry); !ok {
		t.Fatal("response entry missing")
	}
	if _, ok := c.consume("chat", "id", anyEntry); ok {
		t.Fatal("response handled twice")
	}
	entry.storedAt = now.Add(time.Hour - time.Nanosecond)
	if c.store("id", entry) {
		t.Fatal("response removed cooldown")
	}
	entry.storedAt = now.Add(time.Hour)
	if !c.store("id", entry) {
		t.Fatal("cooldown did not expire at one hour")
	}
	entry.chatJID = "other"
	if !c.store("id", entry) {
		t.Fatal("cooldown shared between different chats")
	}
}

func TestMediaRetryPendingExpiresBeforeCooldown(t *testing.T) {
	c := &retryCache{m: make(map[string]mediaRetryEntry)}
	now := time.Now()
	c.store("id", mediaRetryEntry{chatJID: "chat", storedAt: now})
	c.store("new", mediaRetryEntry{chatJID: "other", storedAt: now.Add(30 * time.Minute)})
	if _, ok := c.consume("chat", "id", anyEntry); ok {
		t.Fatal("pending crypto retained after expiry")
	}
	if c.store("id", mediaRetryEntry{chatJID: "chat", storedAt: now.Add(30 * time.Minute)}) {
		t.Fatal("pending cleanup removed cooldown")
	}
	c.store("later", mediaRetryEntry{chatJID: "other", storedAt: now.Add(2 * time.Hour)})
	if len(c.m) != 1 || len(c.requested) != 1 {
		t.Fatalf("stale cache: pending=%d cooldown=%d", len(c.m), len(c.requested))
	}
}

func TestMediaRetryConcurrentAndSendFailure(t *testing.T) {
	old := mediaRetryCache
	mediaRetryCache = &retryCache{m: make(map[string]mediaRetryEntry)}
	t.Cleanup(func() { mediaRetryCache = old })
	s, chat, _, _ := pipelineMedia(t)
	client := &pipelineRetryClient{err: errors.New("offline")}
	if err := requestMediaRetry(client, s, "media", chat); err == nil {
		t.Fatal("send error discarded")
	}
	if len(mediaRetryCache.m) != 0 || len(mediaRetryCache.requested) != 0 {
		t.Fatal("failed send retained cache")
	}
	client.err = nil
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			err := requestMediaRetry(client, s, "media", chat)
			if err != nil && !errors.Is(err, errMediaRetryAlreadyRequested) {
				t.Errorf("retry: %v", err)
			}
		})
	}
	wg.Wait()
	if client.calls.Load() != 2 {
		t.Fatalf("sent %d receipts (including failed send)", client.calls.Load())
	}
}

func TestMediaRetryWritesAtomically(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			old := mediaRetryCache
			mediaRetryCache = &retryCache{m: make(map[string]mediaRetryEntry)}
			t.Cleanup(func() { mediaRetryCache = old })
			s, chat, path, data := pipelineMedia(t)
			if err := requestMediaRetry(&pipelineRetryClient{}, s, "media", chat); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("old"), 0600); err != nil {
				t.Fatal(err)
			}
			notification, err := proto.Marshal(&waMmsRetry.MediaRetryNotification{Result: waMmsRetry.MediaRetryNotification_SUCCESS.Enum(), DirectPath: proto.String("/fresh")})
			if err != nil {
				t.Fatal(err)
			}
			iv := make([]byte, 12)
			key := hkdfutil.SHA256([]byte("key"), nil, []byte("WhatsApp Media Retry Notification"), 32)
			ciphertext, err := gcmutil.Encrypt(key, iv, notification, []byte("media"))
			if err != nil {
				t.Fatal(err)
			}
			d := &pipelineDownloader{write: func(file whatsmeow.File) error {
				if _, err := file.Write(data[:4]); err != nil {
					return err
				}
				contents, err := os.ReadFile(path)
				if err != nil || string(contents) != "old" {
					t.Errorf("target changed during download: %q %v", contents, err)
				}
				info, err := file.Stat()
				if err != nil {
					return err
				}
				if info.Mode().Perm() != 0600 {
					t.Errorf("temp permissions: %o", info.Mode().Perm())
				}
				if fail {
					return errors.New("interrupted")
				}
				_, err = file.Write(data[4:])
				return err
			}}
			handleMediaRetry(d, s, &events.MediaRetry{MessageID: "media", ChatID: types.NewJID("111", types.DefaultUserServer), Ciphertext: ciphertext, IV: iv}, waLog.Noop)
			contents, err := os.ReadFile(path)
			want := data
			if fail {
				want = []byte("old")
			}
			if err != nil || !bytes.Equal(contents, want) || d.calls.Load() != 1 {
				t.Fatalf("retry file: %q, calls=%d, err=%v", contents, d.calls.Load(), err)
			}
			files, _ := os.ReadDir(filepath.Dir(path))
			if len(files) != 1 {
				t.Fatalf("temporary files left: %v", files)
			}
		})
	}
}

func anyEntry(mediaRetryEntry) bool { return true }

func TestMediaRetrySameIDInTwoChats(t *testing.T) {
	c := &retryCache{m: make(map[string]mediaRetryEntry)}
	now := time.Now()
	c.store("id", mediaRetryEntry{chatJID: "a@s.whatsapp.net", storedAt: now, mediaKey: []byte("ka")})
	c.store("id", mediaRetryEntry{chatJID: "b@s.whatsapp.net", storedAt: now, mediaKey: []byte("kb")})
	keyIs := func(k string) func(mediaRetryEntry) bool {
		return func(e mediaRetryEntry) bool { return string(e.mediaKey) == k }
	}
	// The phone names the chat by its LID: the entry whose key decrypts the response wins.
	if e, ok := c.consume("999@lid", "id", keyIs("kb")); !ok || e.chatJID != "b@s.whatsapp.net" {
		t.Fatalf("LID-named response took %+v, %v", e, ok)
	}
	if _, ok := c.consume("999@lid", "id", keyIs("kb")); ok {
		t.Fatal("response handled twice")
	}
	c.forget("b@s.whatsapp.net", "id")
	if e, ok := c.consume("a@s.whatsapp.net", "id", keyIs("ka")); !ok || e.chatJID != "a@s.whatsapp.net" {
		t.Fatalf("a failed request in one chat dropped the other: %+v, %v", e, ok)
	}
}

func TestMediaDownloadNextHostStartsOver(t *testing.T) {
	data := []byte("complete media contents")
	hash := sha256.Sum256(data)
	path := filepath.Join(t.TempDir(), "media.ogg")
	// whatsmeow copies each host's body with io.Copy from a TeeReader and does not rewind between hosts.
	err := writeMediaFile(path, uint64(len(data)), hash[:], func(f whatsmeow.File) error {
		if _, err := io.Copy(f, io.TeeReader(bytes.NewReader([]byte("corrupt body from the first host")), io.Discard)); err != nil {
			return err
		}
		_, err := io.Copy(f, io.TeeReader(bytes.NewReader(data), io.Discard))
		return err
	})
	if got, _ := os.ReadFile(path); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("second host's download: %q, %v", got, err)
	}
}
