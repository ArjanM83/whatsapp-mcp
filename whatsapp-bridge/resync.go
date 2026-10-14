package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
)

// When the local copy of an app state collection no longer matches the
// server ("failed to verify patch vNN: mismatching LTHash"), every new patch
// (such as delete_for_me) is refused with a 409 conflict. A full sync throws
// the local state away and rebuilds it from the server snapshot. If a patch on
// the server itself does not verify, a full sync keeps failing; then
// "recovery": true asks the phone for a fresh snapshot of its own state (what
// WhatsApp Web does after a fatal mismatch). The phone answers with a peer
// message that whatsmeow processes by itself.

type ResyncAppStateRequest struct {
	Name     string `json:"name,omitempty"` // default regular_high
	Recovery bool   `json:"recovery,omitempty"`
}

func registerResyncHandler(client *whatsmeow.Client) {
	http.HandleFunc("/api/resync_app_state", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req ResyncAppStateRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		name := appstate.WAPatchName(req.Name)
		if name == "" {
			name = appstate.WAPatchRegularHigh
		}
		resp := SimpleResponse{true, fmt.Sprintf("Full sync of %s done", name)}
		if !client.IsConnected() {
			resp = SimpleResponse{false, "Not connected to WhatsApp"}
		} else if req.Recovery {
			if _, err := client.SendPeerMessage(context.Background(), whatsmeow.BuildAppStateRecoveryRequest(name)); err != nil {
				resp = SimpleResponse{false, fmt.Sprintf("Recovery request for %s failed: %v", name, err)}
			} else {
				resp = SimpleResponse{true, fmt.Sprintf("Recovery of %s requested from the phone", name)}
			}
		} else if err := client.FetchAppState(context.Background(), name, true, false); err != nil {
			resp = SimpleResponse{false, fmt.Sprintf("Full sync of %s failed: %v", name, err)}
		}
		w.Header().Set("Content-Type", "application/json")
		if !resp.Success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(resp)
	})
}
