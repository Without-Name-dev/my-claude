package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
	"github.com/google/uuid"
)

// handleEnqueueMessage handles POST /api/v2/sessions/:id/queue
func (a *App) handleEnqueueMessage(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	sessionID := r.PathValue("id")

	var req struct {
		Content     string   `json:"content"`
		Attachments []string `json:"attachments,omitempty"`
		Source      string   `json:"source"` // terminal, ios, web
	}

	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.Fail(w, httpx.BadRequest("Invalid request body"))
		return
	}

	if req.Content == "" {
		httpx.Fail(w, httpx.BadRequest("Content is required"))
		return
	}

	if req.Source == "" {
		req.Source = "web" // default
	}

	msg := store.QueueMessage{
		ID:          uuid.New().String(),
		SessionID:   sessionID,
		Content:     req.Content,
		Attachments: req.Attachments,
		Status:      "queued",
		Source:      req.Source,
		CreatedAt:   time.Now(),
	}

	if err := a.db.EnqueueMessage(ctx, msg); err != nil {
		a.log.Error("failed to enqueue message", "error", err)
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "Failed to enqueue message"))
		return
	}


	// Publish queue event
	frame, err := newEnvelope(sessionID, 0, "queue.message_added", map[string]any{
		"message": msg,
	})
	if err == nil {
		a.broker.Publish(sessionID, frame)
	}

	httpx.JSON(w, http.StatusCreated, msg)
}

// handleGetQueue handles GET /api/v2/sessions/:id/queue
func (a *App) handleGetQueue(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	sessionID := r.PathValue("id")

	// Support offline recovery: ?since=seq
	sinceSeq := int64(0)
	if since := r.URL.Query().Get("since"); since != "" {
		if seq, err := strconv.ParseInt(since, 10, 64); err == nil {
			sinceSeq = seq
		}
	}

	messages, err := a.db.GetQueueMessages(ctx, sessionID, sinceSeq)
	if err != nil {
		a.log.Error("failed to get queue", "error", err)
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "Failed to get queue"))
		return
	}

	if messages == nil {
		messages = []store.QueueMessage{}
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"messages": messages,
	})
}

// handleCancelQueuedMessage handles DELETE /api/v2/sessions/:id/queue/:msgId
func (a *App) handleCancelQueuedMessage(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	sessionID := r.PathValue("id")
	msgID := r.PathValue("msgId")

	if err := a.db.CancelQueuedMessage(ctx, msgID); err != nil {
		a.log.Error("failed to cancel message", "error", err)
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "Failed to cancel message"))
		return
	}

	// Publish queue event
	frame, err := newEnvelope(sessionID, 0, "queue.message_cancelled", map[string]any{
		"messageId": msgID,
	})
	if err == nil {
		a.broker.Publish(sessionID, frame)
	}

	w.WriteHeader(http.StatusNoContent)
}

// handleUpdateQueueMessage handles PATCH /api/v2/sessions/:id/queue/:msgId
func (a *App) handleUpdateQueueMessage(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	sessionID := r.PathValue("id")
	msgID := r.PathValue("msgId")

	var req struct {
		Status       string `json:"status"`
		ErrorMessage string `json:"errorMessage,omitempty"`
	}

	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.Fail(w, httpx.BadRequest("Invalid request body"))
		return
	}

	if err := a.db.UpdateMessageStatus(ctx, msgID, req.Status, req.ErrorMessage); err != nil {
		a.log.Error("failed to update message status", "error", err)
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "Failed to update message status"))
		return
	}


	// Publish queue event
	eventType := "queue.message_status_changed"
	if req.Status == "completed" {
		eventType = "queue.message_completed"
	}

	frame, err := newEnvelope(sessionID, 0, eventType, map[string]any{
		"messageId":    msgID,
		"status":       req.Status,
		"errorMessage": req.ErrorMessage,
	})
	if err == nil {
		a.broker.Publish(sessionID, frame)
	}

	w.WriteHeader(http.StatusNoContent)
}
