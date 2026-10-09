package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/appstate"
	"go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// DeleteForMeRequest asks the bridge to "delete for me" one or more messages
// in a single chat. This is the same action as "Verwijderen voor mij" on the
// phone: it is synced to all linked devices through the app state, so the
// message (and, with DeleteMedia, its photo or video) disappears from the
// phone as well. Nobody else in the chat is affected.
type DeleteForMeRequest struct {
	ChatJID     string   `json:"chat_jid"`
	MessageIDs  []string `json:"message_ids"`
	DeleteMedia *bool    `json:"delete_media,omitempty"`
}

type DeleteForMeResult struct {
	MessageID string `json:"message_id"`
	Success   bool   `json:"success"`
	Message   string `json:"message"`
}

type DeleteForMeResponse struct {
	Success bool                `json:"success"`
	Message string              `json:"message"`
	Results []DeleteForMeResult `json:"results"`
}

// Messages per app state patch. WhatsApp accepts larger patches, but small
// batches keep a failure contained.
const deleteForMeBatchSize = 25

// participantForIndex returns the fifth element of the message index: "0"
// for your own messages and for 1-on-1 chats, otherwise the full JID of the
// sender in the group. The bridge only stores the user part of the sender, so
// the server is restored here: a sender that is known as a LID gets "@lid",
// anything else is treated as a phone number.
func participantForIndex(ctx context.Context, client *whatsmeow.Client, chat types.JID, sender string, fromMe bool) string {
	if fromMe || chat.Server != types.GroupServer || sender == "" {
		return "0"
	}
	return senderJID(ctx, client, sender).String()
}

func deleteMessagesForMe(client *whatsmeow.Client, messageStore *MessageStore, req DeleteForMeRequest) DeleteForMeResponse {
	resp := DeleteForMeResponse{Results: []DeleteForMeResult{}}
	if !client.IsConnected() {
		resp.Message = "Not connected to WhatsApp"
		return resp
	}
	chat, err := types.ParseJID(req.ChatJID)
	if err != nil {
		resp.Message = fmt.Sprintf("Invalid chat JID: %v", err)
		return resp
	}
	deleteMedia := true
	if req.DeleteMedia != nil {
		deleteMedia = *req.DeleteMedia
	}

	ctx := context.Background()
	var pending []appstate.MutationInfo
	var pendingIDs []string
	ok := 0

	flush := func() {
		if len(pending) == 0 {
			return
		}
		patch := appstate.PatchInfo{Type: appstate.WAPatchRegularHigh, Mutations: pending}
		err := client.SendAppState(ctx, patch)
		for _, id := range pendingIDs {
			if err != nil {
				resp.Results = append(resp.Results, DeleteForMeResult{id, false, fmt.Sprintf("Sending app state failed: %v", err)})
			} else {
				resp.Results = append(resp.Results, DeleteForMeResult{id, true, "Deleted for me"})
				ok++
			}
		}
		pending, pendingIDs = nil, nil
	}

	for _, id := range req.MessageIDs {
		var sender string
		var fromMe bool
		var ts time.Time
		err := messageStore.db.QueryRow(
			"SELECT sender, is_from_me, timestamp FROM messages WHERE id = ? AND chat_jid = ?",
			id, req.ChatJID,
		).Scan(&sender, &fromMe, &ts)
		if err != nil {
			resp.Results = append(resp.Results, DeleteForMeResult{id, false, fmt.Sprintf("Message not found in local store: %v", err)})
			continue
		}
		isFromMe := "0"
		if fromMe {
			isFromMe = "1"
		}
		pending = append(pending, appstate.MutationInfo{
			Index:   []string{appstate.IndexDeleteMessageForMe, chat.String(), id, isFromMe, participantForIndex(ctx, client, chat, sender, fromMe)},
			Version: 3,
			Value: &waSyncAction.SyncActionValue{
				DeleteMessageForMeAction: &waSyncAction.DeleteMessageForMeAction{
					DeleteMedia:      proto.Bool(deleteMedia),
					MessageTimestamp: proto.Int64(ts.Unix()),
				},
			},
		})
		pendingIDs = append(pendingIDs, id)
		if len(pending) >= deleteForMeBatchSize {
			flush()
		}
	}
	flush()

	resp.Success = ok == len(req.MessageIDs)
	resp.Message = fmt.Sprintf("Deleted %d of %d messages for me", ok, len(req.MessageIDs))
	return resp
}

func registerDeleteForMeHandler(client *whatsmeow.Client, messageStore *MessageStore) {
	http.HandleFunc("/api/delete_for_me", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req DeleteForMeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Invalid request format", http.StatusBadRequest)
			return
		}
		if req.ChatJID == "" || len(req.MessageIDs) == 0 {
			http.Error(w, "chat_jid and message_ids are required", http.StatusBadRequest)
			return
		}
		resp := deleteMessagesForMe(client, messageStore, req)
		w.Header().Set("Content-Type", "application/json")
		if !resp.Success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(resp)
	})
}

// logDeleteForMe prints a delete-for-me that arrives from another device (the
// phone), so the exact index format WhatsApp uses can be compared with what
// the bridge sends.
func logDeleteForMe(evt *events.DeleteForMe, logger waLog.Logger) {
	logger.Infof("DeleteForMe from app state: chat=%s id=%s fromMe=%v sender=%s deleteMedia=%v msgTs=%d fullSync=%v",
		evt.ChatJID, evt.MessageID, evt.IsFromMe, evt.SenderJID,
		evt.Action.GetDeleteMedia(), evt.Action.GetMessageTimestamp(), evt.FromFullSync)
}
