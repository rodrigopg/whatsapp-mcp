package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
)

// When the local copy of an app-state collection drifts from WhatsApp's ("failed to verify patch vNN:
// mismatching LTHash" in the log), every patch the bridge sends (archive, mark read, delete chat, ...)
// is rejected with a 500 and nothing recovers by itself. whatsmeow's own repair is a full sync of that
// collection: drop the cached state and fetch it again from the server.
var appStateNames = map[string]appstate.WAPatchName{
	"regular":              appstate.WAPatchRegular,
	"regular_low":          appstate.WAPatchRegularLow,
	"regular_high":         appstate.WAPatchRegularHigh,
	"critical_block":       appstate.WAPatchCriticalBlock,
	"critical_unblock_low": appstate.WAPatchCriticalUnblockLow,
}

func parseAppStateName(s string) (appstate.WAPatchName, bool) {
	n, ok := appStateNames[s]
	return n, ok
}

type AppStateResyncRequest struct {
	Name string `json:"name"`
}

// handleAppStateResync returns the handler for POST /api/appstate/resync {"name": "regular_low"}.
func handleAppStateResync(client *whatsmeow.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSONError(w, http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		var req AppStateResyncRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "Invalid request format")
			return
		}
		name, ok := parseAppStateName(req.Name)
		if !ok {
			writeJSONError(w, http.StatusBadRequest, "name must be one of: regular, regular_low, regular_high, critical_block, critical_unblock_low")
			return
		}
		if client == nil || !client.IsConnected() {
			writeJSONError(w, http.StatusServiceUnavailable, "WhatsApp client not connected")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := client.FetchAppState(ctx, name, true, false); err != nil {
			writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("full sync of %s failed: %v", req.Name, err))
			return
		}
		writeJSON(w, MarkChatResponse{Success: true, Message: fmt.Sprintf("App state %s fully re-synced", req.Name)})
	}
}
