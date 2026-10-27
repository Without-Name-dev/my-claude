package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// handleEditTimelineItem edits a message and regenerates the response.
func (a *App) handleEditTimelineItem(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}

	itemID := r.PathValue("item_id")
	if itemID == "" {
		httpx.Fail(w, httpx.BadRequest("missing item_id"))
		return
	}

	var body struct {
		Content string `json:"content"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	if body.Content == "" {
		httpx.Fail(w, httpx.BadRequest("content cannot be empty"))
		return
	}

	// Fetch the existing timeline item
	item, err := a.db.TimelineItemByID(r.Context(), a.db.Read(), session.ID, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.Fail(w, httpx.NotFound("timeline item not found"))
			return
		}
		httpx.Fail(w, err)
		return
	}

	// Only allow editing user messages
	if item.Type != "message" || item.Role != "user" {
		httpx.Fail(w, httpx.BadRequest("only user messages can be edited"))
		return
	}

	// Parse the existing content
	var existingContent map[string]any
	if err := json.Unmarshal(item.Payload, &existingContent); err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "failed to parse message content"))
		return
	}

	now := store.NowMS()
	editSeq := 0

	err = a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
		// Save edit history
		if err := a.db.SaveMessageEdit(ctx, tx, session.ID, itemID, item.Payload, editSeq, now); err != nil {
			return err
		}

		// Update the message content
		existingContent["text"] = body.Content
		newPayload, err := json.Marshal(existingContent)
		if err != nil {
			return err
		}

		seq, err := store.AllocateSeq(ctx, tx, session.ID, now)
		if err != nil {
			return err
		}

		updatedItem := store.TimelineItem{
			SessionID:  session.ID,
			ID:         itemID,
			Type:       item.Type,
			Status:     item.Status,
			Role:       item.Role,
			OrderSeq:   item.OrderSeq,
			UpdatedSeq: seq,
			ItemTime:   item.ItemTime,
			Payload:    json.RawMessage(newPayload),
		}

		if err := store.UpsertTimelineItem(ctx, tx, updatedItem, seq); err != nil {
			return err
		}

		// Mark as edited
		_, err = tx.ExecContext(ctx, `
			UPDATE timeline_items 
			SET edited_at = ?, edit_count = COALESCE(edit_count, 0) + 1
			WHERE session_id = ? AND id = ?`,
			now, session.ID, itemID)
		if err != nil {
			return err
		}

		// Delete all timeline items after this message
		if err := a.db.DeleteTimelineItemsAfter(ctx, tx, session.ID, item.OrderSeq); err != nil {
			return err
		}

		return nil
	})

	if err != nil {
		httpx.Fail(w, err)
		return
	}

	// Publish timeline reset event via broker
	a.publishTimelineReset(r.Context(), session.ID)

	// Extract message ID from item ID (format: msg_<messageID>_user)
	messageID := extractMessageID(itemID)
	
	// Trigger regeneration by sending the edited message
	if messageID != "" && a.connectors.Online(session.ConnectorID) {
		files := []messageAttachment{} // No attachments on retry
		// A unique id per attempt: the deterministic "_retry" suffix made a
		// second edit collide with the first one's request (silent no-op or
		// 409) while the reply it was meant to regenerate stayed deleted.
		if _, err := a.deliverMessage(r.Context(), session, body.Content, messageID+"_"+store.NewID("retry"), "prompt", files); err != nil {
			a.log.Warn("failed to regenerate after edit", "session", session.ID, "error", err)
		}
	}

	httpx.OK(w, map[string]any{"ok": true})
}

// handleRetryMessage retries sending a failed or edited message.
func (a *App) handleRetryMessage(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}

	itemID := r.PathValue("item_id")
	if itemID == "" {
		httpx.Fail(w, httpx.BadRequest("missing item_id"))
		return
	}

	// Fetch the timeline item
	item, err := a.db.TimelineItemByID(r.Context(), a.db.Read(), session.ID, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.Fail(w, httpx.NotFound("timeline item not found"))
			return
		}
		httpx.Fail(w, err)
		return
	}

	// Only allow retrying user messages
	if item.Type != "message" || item.Role != "user" {
		httpx.Fail(w, httpx.BadRequest("only user messages can be retried"))
		return
	}

	// Parse content
	var content map[string]any
	if err := json.Unmarshal(item.Payload, &content); err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "failed to parse message content"))
		return
	}

	text, _ := content["text"].(string)
	if text == "" {
		httpx.Fail(w, httpx.BadRequest("message has no text content"))
		return
	}

	// Extract message ID and resend
	messageID := extractMessageID(itemID)
	if messageID == "" {
		messageID = store.NewID("msg")
	} else {
		// Unique per attempt, so retrying the same failed message twice does
		// not reuse the first retry's request id.
		messageID = messageID + "_" + store.NewID("retry")
	}

	files := []messageAttachment{} // TODO: Extract attachments from content if needed
	if _, err := a.deliverMessage(r.Context(), session, text, messageID, "prompt", files); err != nil {
		if errors.Is(err, errSessionNotLive) {
			failSessionNotLive(w, "the conversation is not live on the desktop; open it there to continue")
			return
		}
		httpx.Fail(w, err)
		return
	}

	httpx.OK(w, map[string]any{"ok": true, "result": map[string]any{"messageId": messageID}})
}

// extractMessageID extracts the message ID from a timeline item ID.
// Timeline item IDs are typically formatted as "msg_<messageID>_user" or "msg_<messageID>_assistant".
func extractMessageID(itemID string) string {
	// Simple extraction: remove "msg_" prefix and "_user"/"_assistant" suffix
	if len(itemID) > 4 && itemID[:4] == "msg_" {
		for i := len(itemID) - 1; i >= 0; i-- {
			if itemID[i] == '_' {
				return itemID[4:i]
			}
		}
	}
	return ""
}

// publishTimelineReset sends a timeline snapshot event to all connected clients.
func (a *App) publishTimelineReset(ctx context.Context, sessionID string) {
	session, err := a.db.SessionByIDAny(ctx, a.db.Read(), sessionID)
	if err != nil {
		return
	}
	items, err := a.db.TimelineItemsLatest(ctx, a.db.Read(), sessionID, 500)
	if err != nil {
		return
	}
	views := make([]ItemView, 0, len(items))
	for _, item := range items {
		views = append(views, toView(item, session.Runtime))
	}
	// One frame with 500 long messages can exceed the clients' 1 MiB message
	// limit; the receiver then tore the whole stream down and reconnected in a
	// loop. Clients merge snapshot frames, so publish it in bounded chunks.
	const chunkBytes = 512 << 10
	chunk := make([]ItemView, 0, 64)
	size := 0
	flush := func() {
		if len(chunk) == 0 {
			return
		}
		frame, err := newEnvelope(sessionID, session.Seq, EventTimelineReset, map[string]any{"items": chunk})
		if err != nil {
			return
		}
		a.broker.Publish(sessionID, frame)
		chunk = make([]ItemView, 0, 64)
		size = 0
	}
	for _, view := range views {
		encoded, err := json.Marshal(view)
		if err == nil && size+len(encoded) > chunkBytes {
			flush()
		}
		chunk = append(chunk, view)
		size += len(encoded)
	}
	flush()
}
