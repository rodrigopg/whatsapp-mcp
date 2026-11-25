package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// maxMediaBytes caps a file sent through media_path; the whole file is held in memory to upload.
const maxMediaBytes = 100 << 20

// storeParts reports whether resolved lies inside the bridge's store directory and, if so, its
// path components below it. Directories are compared by identity (os.SameFile), not by name, so
// a different spelling of the same folder on a case-insensitive filesystem does not slip through.
func storeParts(resolved string) (parts []string, inside bool) {
	store, err := os.Stat(storePath())
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

// mediaRefusal is a reason safe to return to the API caller: it never carries a path.
type mediaRefusal string

func (e mediaRefusal) Error() string { return string(e) }

// defaultMediaRoots lists the folders files may be sent from when MEDIA_ALLOWED_ROOTS is unset:
// the temp folder (where the MCP server writes converted and synthesized audio) and the user's
// Downloads folder. A variable so tests can point it away from the real folders.
var defaultMediaRoots = func() []string {
	roots := []string{os.TempDir()}
	if runtime.GOOS != "windows" {
		roots = append(roots, "/tmp") // macOS: TempDir is a per-user folder, other programs still use /tmp
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return roots
	}
	roots = append(roots, filepath.Join(home, "Downloads"))
	// Linux desktops may name the folder otherwise (xdg-user-dirs).
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(home, ".config")
	}
	data, _ := os.ReadFile(filepath.Join(cfg, "user-dirs.dirs"))
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "XDG_DOWNLOAD_DIR="); ok {
			dir := filepath.Clean(strings.Replace(strings.Trim(v, `"`), "$HOME", home, 1))
			// "$HOME/" is what xdg-user-dirs writes for a missing folder; never open the whole home.
			if filepath.IsAbs(dir) && !within(dir, home) {
				roots = append(roots, dir)
			}
		}
	}
	return roots
}

// openNoLinks opens base/parts... for reading, one component at a time, and refuses any component
// that is a symlink or that changed between being looked at and being opened. The caller passes a
// path it already resolved and checked, so the file returned is the one that was checked: swapping
// a component for a symlink afterwards gets a refusal, not a different file. Walking into the
// bridge's store from an outer base is refused as well, and so are hidden names unless hiddenOK.
func openNoLinks(base string, parts []string, hiddenOK bool) (*os.File, error) {
	const changed = mediaRefusal("the path changed while it was being read")
	store, storeErr := os.Stat(storePath())
	dir, err := os.OpenRoot(base)
	if err != nil {
		return nil, err
	}
	defer func() { dir.Close() }()
	for i, name := range parts {
		if !hiddenOK && strings.HasPrefix(name, ".") {
			return nil, mediaRefusal("path has a hidden component (a name starting with \".\"); set MEDIA_ALLOWED_ROOTS to allow a folder")
		}
		want, err := dir.Lstat(name)
		if err != nil {
			return nil, err
		}
		if i == len(parts)-1 {
			if !want.Mode().IsRegular() {
				return nil, mediaRefusal("not a regular file")
			}
			f, err := dir.Open(name)
			if err != nil {
				return nil, err
			}
			if got, err := f.Stat(); err != nil || !os.SameFile(want, got) {
				f.Close()
				return nil, changed
			}
			return f, nil
		}
		if !want.IsDir() {
			return nil, changed
		}
		if storeErr == nil && os.SameFile(want, store) {
			return nil, mediaRefusal("files inside the bridge store cannot be sent, except downloaded media")
		}
		next, err := dir.OpenRoot(name)
		if err != nil {
			return nil, err
		}
		got, err := next.Stat(".")
		if err != nil || !os.SameFile(want, got) {
			next.Close()
			return nil, changed
		}
		dir.Close()
		dir = next
	}
	return nil, mediaRefusal("not a regular file")
}

// readOutboundMedia reads the file a caller asked to send (media_path). The caller is an API or
// MCP client, which may be relaying text it did not write, so the path is checked after resolving
// symlinks:
//   - inside store/ only a file directly in a chat folder (store/<jid>/<file>, where the bridge
//     saves downloaded media) may be sent; the session and message databases and the settings
//     never are, whatever else is configured;
//   - with MEDIA_ALLOWED_ROOTS set (a path list), the file must be inside one of those folders;
//   - without it, the file must be inside a default folder (defaultMediaRoots) and no component
//     below that folder may be hidden (a name starting with "."): temp and Downloads folders hold
//     other programs' dot-files too;
//   - regular files only, at most maxMediaBytes.
//
// The file is then opened through openNoLinks and checked on the open descriptor. Errors carry
// the reason only, never a path of this machine.
func readOutboundMedia(path string) ([]byte, error) {
	data, err := readCheckedMedia(path)
	var refusal mediaRefusal
	switch {
	case err == nil || errors.As(err, &refusal):
		return data, err
	case errors.Is(err, fs.ErrNotExist):
		return nil, errors.New("file not found")
	case errors.Is(err, fs.ErrPermission):
		return nil, errors.New("the bridge has no permission to read the file")
	}
	return nil, errors.New("the file could not be read")
}

func readCheckedMedia(path string) ([]byte, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	var f *os.File
	if parts, inside := storeParts(resolved); inside {
		if len(parts) != 2 || !strings.Contains(parts[0], "@") {
			return nil, mediaRefusal("files inside the bridge store cannot be sent, except downloaded media")
		}
		f, err = openNoLinks(storePath(), parts, true)
	} else {
		roots, outside := filepath.SplitList(os.Getenv("MEDIA_ALLOWED_ROOTS")), mediaRefusal("path is outside MEDIA_ALLOWED_ROOTS")
		configured := len(roots) > 0
		if !configured {
			roots = defaultMediaRoots()
			outside = "files can be sent only from the Downloads folder, the temp folder or downloaded media; move the file there or set MEDIA_ALLOWED_ROOTS to allow its folder"
		}
		err = outside
		for _, root := range roots {
			r, rerr := filepath.EvalSymlinks(root)
			if rerr != nil || !filepath.IsAbs(r) || !within(r, resolved) {
				continue
			}
			rel, _ := filepath.Rel(r, resolved)
			// A hidden name is refused in the default folders only; a configured root is the owner's choice.
			if f, err = openNoLinks(r, strings.Split(rel, string(os.PathSeparator)), configured); err == nil {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil {
		return nil, err
	} else if fi.Size() > maxMediaBytes {
		return nil, mediaRefusal(fmt.Sprintf("file is larger than %d MB", maxMediaBytes>>20))
	}
	data, err := io.ReadAll(io.LimitReader(f, maxMediaBytes+1))
	if err == nil && len(data) > maxMediaBytes {
		err = mediaRefusal(fmt.Sprintf("file is larger than %d MB", maxMediaBytes>>20))
	}
	return data, err
}

// tightenStore makes the store readable by the bridge's user only (folders 0700, files 0600),
// including what an older version created with wider modes. It holds the session keys, the
// message history and downloaded media.
func tightenStore() {
	root, err := filepath.EvalSymlinks(storePath())
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
