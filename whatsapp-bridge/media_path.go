package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// maxMediaBytes caps a file sent through media_path; the whole file is held in memory to upload.
const maxMediaBytes = 100 << 20

// qrImagePath is the pairing QR saved for convenience while pairing; removed once paired.
const qrImagePath = "store/whatsapp-qr.png"

// storeParts reports whether resolved lies inside the bridge's store directory and, if so, its
// path components below it. Directories are compared by identity (os.SameFile), not by name, so
// a different spelling of the same folder on a case-insensitive filesystem does not slip through.
func storeParts(resolved string) (parts []string, inside bool) {
	store, err := os.Stat("store")
	if err != nil {
		return nil, false
	}
	for dir := resolved; ; {
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, false
		}
		parts = append([]string{filepath.Base(dir)}, parts...)
		if fi, err := os.Stat(parent); err == nil && os.SameFile(fi, store) {
			return parts, true
		}
		dir = parent
	}
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// readOutboundMedia reads the file a caller asked to send (media_path). The caller is an API or
// MCP client, which may be relaying text it did not write, so the path is checked after resolving
// symlinks:
//   - inside store/ only a file directly in a chat folder (store/<jid>/<file>, where the bridge
//     saves downloaded media) may be sent; the session and message databases and the settings
//     never are, whatever else is configured;
//   - with MEDIA_ALLOWED_ROOTS set (a path list), the file must be inside one of those folders;
//   - without it, any path with a hidden component (a name starting with ".", such as ~/.ssh or
//     .env) is refused;
//   - regular files only, at most maxMediaBytes.
func readOutboundMedia(path string) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	if parts, inside := storeParts(resolved); inside {
		if len(parts) != 2 || !strings.Contains(parts[0], "@") {
			return nil, errors.New("files inside the bridge store cannot be sent, except downloaded media")
		}
	} else if roots := filepath.SplitList(os.Getenv("MEDIA_ALLOWED_ROOTS")); len(roots) > 0 {
		allowed := false
		for _, root := range roots {
			if r, err := filepath.EvalSymlinks(root); err == nil && filepath.IsAbs(r) && within(r, resolved) {
				allowed = true
			}
		}
		if !allowed {
			return nil, errors.New("path is outside MEDIA_ALLOWED_ROOTS")
		}
	} else {
		for _, part := range strings.Split(resolved, string(os.PathSeparator)) {
			if strings.HasPrefix(part, ".") {
				return nil, errors.New("path has a hidden component (a name starting with \".\"); set MEDIA_ALLOWED_ROOTS to allow a folder")
			}
		}
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if fi.Size() > maxMediaBytes {
		return nil, fmt.Errorf("file is larger than %d MB", maxMediaBytes>>20)
	}
	f, err := os.Open(resolved)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxMediaBytes+1))
	if err == nil && len(data) > maxMediaBytes {
		err = fmt.Errorf("file is larger than %d MB", maxMediaBytes>>20)
	}
	return data, err
}

// tightenStore makes the store readable by the bridge's user only (folders 0700, files 0600),
// including what an older version created with wider modes. It holds the session keys, the
// message history and downloaded media.
func tightenStore() {
	root, err := filepath.EvalSymlinks("store")
	if err != nil {
		return
	}
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			os.Chmod(path, 0o700)
		} else if d.Type().IsRegular() {
			os.Chmod(path, 0o600)
		}
		return nil
	})
}
