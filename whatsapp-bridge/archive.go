package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

// ArchiveChatRequest archives (or, with Archive=false, unarchives) one chat.
// Same action as swiping a chat to "Archief" on the phone: it is synced to
// all linked devices through the app state.
type ArchiveChatRequest struct {
	ChatJID string `json:"chat_jid"`
	Archive *bool  `json:"archive,omitempty"`
}

type ArchiveChatResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// lastMessageKey returns the timestamp and key of the newest message the
// bridge knows in the chat. WhatsApp uses it as the message range of the
// archive action; without one the phone may unarchive the chat again on the
// next message it already had. Zero values when the chat has no messages.
func lastMessageKey(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore, chat types.JID, chatJID string) (time.Time, *waCommon.MessageKey) {
	var id, sender string
	var fromMe bool
	var ts time.Time
	err := messageStore.db.QueryRow(
		"SELECT id, sender, is_from_me, timestamp FROM messages WHERE chat_jid = ? ORDER BY timestamp DESC LIMIT 1",
		chatJID,
	).Scan(&id, &sender, &fromMe, &ts)
	if err != nil {
		return time.Time{}, nil
	}
	key := &waCommon.MessageKey{
		RemoteJID: proto.String(chat.String()),
		FromMe:    proto.Bool(fromMe),
		ID:        proto.String(id),
	}
	if chat.Server == types.GroupServer && !fromMe && sender != "" {
		key.Participant = proto.String(senderJID(ctx, client, sender).String())
	}
	return ts, key
}

func archiveChat(client *whatsmeow.Client, messageStore *MessageStore, req ArchiveChatRequest) ArchiveChatResponse {
	if !client.IsConnected() {
		return ArchiveChatResponse{Message: "Not connected to WhatsApp"}
	}
	chat, err := types.ParseJID(req.ChatJID)
	if err != nil {
		return ArchiveChatResponse{Message: fmt.Sprintf("Invalid chat JID: %v", err)}
	}
	archive := true
	if req.Archive != nil {
		archive = *req.Archive
	}
	ctx := context.Background()
	ts, key := lastMessageKey(ctx, client, messageStore, chat, req.ChatJID)
	if err := client.SendAppState(ctx, appstate.BuildArchive(chat, archive, ts, key)); err != nil {
		return ArchiveChatResponse{Message: fmt.Sprintf("Sending app state failed: %v", err)}
	}
	if archive {
		return ArchiveChatResponse{Success: true, Message: "Chat archived"}
	}
	return ArchiveChatResponse{Success: true, Message: "Chat unarchived"}
}

func registerArchiveHandler(client *whatsmeow.Client, messageStore *MessageStore) {
	http.HandleFunc("/api/archive_chat", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req ArchiveChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.ChatJID == "" {
			http.Error(w, "chat_jid is required", http.StatusBadRequest)
			return
		}
		resp := archiveChat(client, messageStore, req)
		w.Header().Set("Content-Type", "application/json")
		if !resp.Success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(resp)
	})
}
