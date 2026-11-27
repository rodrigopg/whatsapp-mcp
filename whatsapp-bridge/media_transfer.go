package main

// Media as bytes over the API, so an MCP server on another machine never needs a path on the
// bridge's disk: it uploads the file to /api/send/file and gets downloaded media from
// /api/download/file. /api/send (media_path) and /api/download stay for local clients.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

// outboundFile is a file to send: its name (extension picks the media type, documents show it)
// and its bytes, held in memory like readOutboundMedia does.
type outboundFile struct {
	name string
	data []byte
}

// checkUploadName refuses a file name that is not a single plain component; it is never cleaned.
func checkUploadName(name string) error {
	if name == "" || name == "." || name == ".." || len(name) > 255 || strings.ContainsAny(name, `/\`) ||
		strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return errors.New("filename must be a plain file name")
	}
	return nil
}

// sendFileHandler serves POST /api/send/file?recipient=&filename=[&message=] with the file as the
// raw body (no multipart: net/http would spill large parts to the temp folder). The body is capped
// at maxMediaBytes and never written to disk.
func sendFileHandler(send func(ctx context.Context, recipient, message string, f *outboundFile) (string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		q := r.URL.Query()
		recipient, name := q.Get("recipient"), q.Get("filename")
		if recipient == "" {
			http.Error(w, "Recipient is required", http.StatusBadRequest)
			return
		}
		if err := checkUploadName(name); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMediaBytes))
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			http.Error(w, fmt.Sprintf("file is larger than %d MB", maxMediaBytes>>20), http.StatusRequestEntityTooLarge)
			return
		case err != nil:
			http.Error(w, "could not read the uploaded file", http.StatusBadRequest)
			return
		case len(data) == 0:
			http.Error(w, "the uploaded file is empty", http.StatusBadRequest)
			return
		}
		fmt.Printf("Received request to send file %q (%d bytes) to %q\n", name, len(data), recipient)
		message, err := send(r.Context(), recipient, q.Get("message"), &outboundFile{name: name, data: data})
		if writeSendRateError(w, err) {
			return
		}
		success := err == nil
		if !success {
			message = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		if !success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(SendMessageResponse{Success: success, Message: message})
	}
}

// downloadFileHandler serves POST /api/download/file {message_id, chat_jid}: the media the bridge
// stored for that message (downloading it first if needed), as bytes. The file is the one
// downloadMedia names from the database, opened without following links, and only if it sits
// directly in a chat folder (store/<jid>/<file>). Errors are fixed texts.
func downloadFileHandler(download func(messageID, chatJID string) (bool, string, string, string, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req DownloadMediaRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.MessageID == "" || req.ChatJID == "" {
			http.Error(w, "Message ID and Chat JID are required", http.StatusBadRequest)
			return
		}
		fail := func(err error) {
			fmt.Printf("Download of %q in %q failed: %v\n", req.MessageID, req.ChatJID, err)
			resp := DownloadMediaResponse{Success: false, Message: "Failed to download media"}
			if mediaGoneOnCDN(err) {
				resp.Reason = downloadReasonMediaGone
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(resp)
		}
		ok, mediaType, filename, path, err := download(req.MessageID, req.ChatJID)
		if err == nil && !ok {
			err = errors.New("download failed")
		}
		if err != nil {
			fail(err)
			return
		}
		folder := filepath.Base(filepath.Dir(path))
		if !strings.Contains(folder, "@") {
			fail(errors.New("not a chat media file"))
			return
		}
		f, err := openNoLinks("store", []string{folder, filepath.Base(path)}, true)
		if err != nil {
			fail(err)
			return
		}
		defer f.Close()
		fi, err := f.Stat()
		if err != nil {
			fail(err)
			return
		}
		h := w.Header()
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", "attachment")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		h.Set("X-Media-Filename", url.PathEscape(filename))
		h.Set("X-Media-Type", mediaType)
		io.Copy(w, f)
	}
}
