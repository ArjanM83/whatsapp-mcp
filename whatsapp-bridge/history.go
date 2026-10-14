package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waMmsRetry"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// The bridge only knows the history the phone sent at pairing time. These two
// endpoints fill the gaps for a single chat:
//
//   - /api/request_history asks the phone for older messages than the oldest
//     one in messages.db (an on-demand history sync). The answer arrives as a
//     normal events.HistorySync and is stored by handleHistorySync.
//   - /api/media_retry asks the phone to upload the media of old messages
//     again, because WhatsApp's CDN links expire after a few weeks. The answer
//     arrives as events.MediaRetry; handleMediaRetry then stores the fresh
//     link in messages.url, after which the normal download works again.

type RequestHistoryRequest struct {
	ChatJID string `json:"chat_jid"`
	Count   int    `json:"count,omitempty"`
}

type MediaRetryRequest struct {
	ChatJID    string   `json:"chat_jid"`
	MessageIDs []string `json:"message_ids"`
}

type SimpleResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// senderJID turns the sender column into a full JID. Live messages store only
// the user part, history sync stores the full JID (see participantForIndex).
func senderJID(ctx context.Context, client *whatsmeow.Client, sender string) types.JID {
	if strings.Contains(sender, "@") {
		if jid, err := types.ParseJID(sender); err == nil {
			return jid
		}
	}
	lid := types.NewJID(sender, types.HiddenUserServer)
	if pn, err := client.Store.LIDs.GetPNForLID(ctx, lid); err == nil && !pn.IsEmpty() {
		return lid
	}
	return types.NewJID(sender, types.DefaultUserServer)
}

// messageInfoFromStore rebuilds the MessageInfo whatsmeow needs from a row in
// messages.db.
func messageInfoFromStore(ctx context.Context, client *whatsmeow.Client, messageStore *MessageStore, chatJID, id string) (*types.MessageInfo, []byte, error) {
	chat, err := types.ParseJID(chatJID)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid chat JID: %w", err)
	}
	var sender string
	var fromMe bool
	var ts time.Time
	var mediaKey []byte
	err = messageStore.db.QueryRow(
		"SELECT sender, is_from_me, timestamp, media_key FROM messages WHERE id = ? AND chat_jid = ?",
		id, chatJID,
	).Scan(&sender, &fromMe, &ts, &mediaKey)
	if err != nil {
		return nil, nil, fmt.Errorf("message not found in local store: %w", err)
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chat,
			IsFromMe: fromMe,
			IsGroup:  chat.Server == types.GroupServer,
		},
		ID:        id,
		Timestamp: ts,
	}
	if fromMe {
		info.Sender = client.Store.GetJID().ToNonAD()
	} else if info.IsGroup {
		info.Sender = senderJID(ctx, client, sender)
	} else {
		info.Sender = chat
	}
	return info, mediaKey, nil
}

func requestHistory(client *whatsmeow.Client, messageStore *MessageStore, req RequestHistoryRequest) SimpleResponse {
	if !client.IsConnected() {
		return SimpleResponse{false, "Not connected to WhatsApp"}
	}
	count := req.Count
	if count <= 0 || count > 50 {
		count = 50
	}
	var oldestID string
	err := messageStore.db.QueryRow(
		"SELECT id FROM messages WHERE chat_jid = ? ORDER BY timestamp ASC LIMIT 1", req.ChatJID,
	).Scan(&oldestID)
	if err != nil {
		return SimpleResponse{false, fmt.Sprintf("No messages known for this chat: %v", err)}
	}
	ctx := context.Background()
	info, _, err := messageInfoFromStore(ctx, client, messageStore, req.ChatJID, oldestID)
	if err != nil {
		return SimpleResponse{false, err.Error()}
	}
	if _, err := client.SendPeerMessage(ctx, client.BuildHistorySyncRequest(info, count)); err != nil {
		return SimpleResponse{false, fmt.Sprintf("Sending history request failed: %v", err)}
	}
	return SimpleResponse{true, fmt.Sprintf("Requested %d messages before %s (%s)", count, oldestID, info.Timestamp.Format(time.RFC3339))}
}

func requestMediaRetry(client *whatsmeow.Client, messageStore *MessageStore, req MediaRetryRequest) SimpleResponse {
	if !client.IsConnected() {
		return SimpleResponse{false, "Not connected to WhatsApp"}
	}
	ctx := context.Background()
	sent, failed := 0, []string{}
	for _, id := range req.MessageIDs {
		info, mediaKey, err := messageInfoFromStore(ctx, client, messageStore, req.ChatJID, id)
		if err == nil && len(mediaKey) == 0 {
			err = fmt.Errorf("no media key")
		}
		if err == nil {
			err = client.SendMediaRetryReceipt(ctx, info, mediaKey)
		}
		if err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", id, err))
			continue
		}
		sent++
	}
	msg := fmt.Sprintf("Sent %d media retry requests", sent)
	if len(failed) > 0 {
		msg += "; failed: " + strings.Join(failed, "; ")
	}
	return SimpleResponse{len(failed) == 0, msg}
}

// handleMediaRetry stores the new link the phone sends after a media retry.
func handleMediaRetry(messageStore *MessageStore, evt *events.MediaRetry, logger waLog.Logger) {
	var mediaKey []byte
	err := messageStore.db.QueryRow(
		"SELECT media_key FROM messages WHERE id = ? AND chat_jid = ?", evt.MessageID, evt.ChatID.String(),
	).Scan(&mediaKey)
	if err != nil {
		logger.Warnf("MediaRetry for unknown message %s in %s: %v", evt.MessageID, evt.ChatID, err)
		return
	}
	notif, err := whatsmeow.DecryptMediaRetryNotification(evt, mediaKey)
	if err != nil {
		logger.Warnf("MediaRetry %s: %v", evt.MessageID, err)
		return
	}
	if notif.GetResult() != waMmsRetry.MediaRetryNotification_SUCCESS || notif.GetDirectPath() == "" {
		logger.Warnf("MediaRetry %s: result %s", evt.MessageID, notif.GetResult())
		return
	}
	url := "https://mmg.whatsapp.net" + notif.GetDirectPath()
	if _, err := messageStore.db.Exec(
		"UPDATE messages SET url = ? WHERE id = ? AND chat_jid = ?", url, evt.MessageID, evt.ChatID.String(),
	); err != nil {
		logger.Warnf("MediaRetry %s: storing new URL failed: %v", evt.MessageID, err)
		return
	}
	logger.Infof("MediaRetry %s: new media URL stored", evt.MessageID)
}

func registerHistoryHandlers(client *whatsmeow.Client, messageStore *MessageStore) {
	http.HandleFunc("/api/request_history", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req RequestHistoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" {
			http.Error(w, "chat_jid is required", http.StatusBadRequest)
			return
		}
		resp := requestHistory(client, messageStore, req)
		w.Header().Set("Content-Type", "application/json")
		if !resp.Success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(resp)
	})

	http.HandleFunc("/api/media_retry", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var req MediaRetryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ChatJID == "" || len(req.MessageIDs) == 0 {
			http.Error(w, "chat_jid and message_ids are required", http.StatusBadRequest)
			return
		}
		resp := requestMediaRetry(client, messageStore, req)
		w.Header().Set("Content-Type", "application/json")
		if !resp.Success {
			w.WriteHeader(http.StatusInternalServerError)
		}
		json.NewEncoder(w).Encode(resp)
	})
}
