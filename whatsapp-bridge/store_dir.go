package main

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
)

// storePath names a file or folder inside the bridge's store: WHATSAPP_STORE_DIR when set,
// otherwise store/ in the working directory (the layout of every existing install).
func storePath(parts ...string) string {
	root := os.Getenv("WHATSAPP_STORE_DIR")
	if root == "" {
		root = "store"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

// storeDSN is the SQLite URI of a database in the store. The path is escaped, so a store folder
// whose name holds "?", "#" or "%" still opens the right file.
func storeDSN(name, query string) string {
	path := &url.URL{Path: filepath.ToSlash(storePath(name))}
	return "file:" + path.EscapedPath() + "?" + query
}

// checkStoreDir refuses a relative WHATSAPP_STORE_DIR: it would depend on the folder each program
// starts in, so the bridge and the Python scripts could open different stores.
func checkStoreDir() error {
	if root := os.Getenv("WHATSAPP_STORE_DIR"); root != "" && !filepath.IsAbs(root) {
		return errors.New("WHATSAPP_STORE_DIR must be an absolute path")
	}
	return nil
}

// Files the bridge keeps in the store besides the databases and the chat media folders.
func qrImagePath() string     { return storePath("whatsapp-qr.png") } // pairing QR, removed once paired
func settingsPath() string    { return storePath("settings.json") }
func loggedOutMarker() string { return storePath("logged_out") }
