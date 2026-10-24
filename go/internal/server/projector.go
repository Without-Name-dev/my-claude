package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// deltaFlushInterval is how often a running answer is persisted. The phone
// still updates smoothly because turn.end always writes the final text.
const deltaFlushInterval = 300 * time.Millisecond

// deltaPublishInterval is how often streamed text reaches open sockets. The
// clients reveal text between frames, so a finer cadence is what keeps the
// typing continuous rather than arriving in one visible step.
const deltaPublishInterval = 120 * time.Millisecond

// turnRegistry keeps the text accumulated per running turn.
//
// The connector streams deltas, but a timeline item holds the whole message, so
// the server reassembles it. Keeping this in memory means a restart loses the
// partial text of a turn that is still running; the turn's final text arrives
// with turn.end and heals it.
type turnRegistry struct {
	mu    sync.Mutex
	turns map[string]*turnState
}

type turnState struct {
	mu      sync.Mutex
	text    strings.Builder
	chunks  int
	session string
	itemID  string
	// lastWrite throttles persistence: the plugin emits a delta every ~120ms,
	// and rewriting a growing payload that often is wasted work. Clients still
	// see the stream, just in slightly coarser steps.
	lastWrite   time.Time
	lastPublish time.Time
	lastSeq     int64
	orderSeq    int64
	createdAt   int64
}

func newTurnRegistry() *turnRegistry {
	return &turnRegistry{turns: map[string]*turnState{}}
}

func (r *turnRegistry) begin(turnID, sessionID, itemID string) *turnState {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := &turnState{session: sessionID, itemID: itemID}
	r.turns[turnID] = state
	return state
}

func (r *turnRegistry) get(turnID string) *turnState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.turns[turnID]
}

func (r *turnRegistry) end(turnID string) *turnState {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := r.turns[turnID]
	delete(r.turns, turnID)
	return state
}

// approveRegistry holds the questions a connector is waiting on.
type approveRegistry struct {
	mu     sync.Mutex
	byID   map[string]protocol.ApprovalRequest
	connOf map[string]string
}

func newApproveRegistry() *approveRegistry {
	return &approveRegistry{byID: map[string]protocol.ApprovalRequest{}, connOf: map[string]string{}}
}

func (r *approveRegistry) add(noticeID, connectorID string, req protocol.ApprovalRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[noticeID] = req
	r.connOf[noticeID] = connectorID
}

// takeFor removes a notice only when it belongs to the named session and
// connector. A mismatched request leaves it pending: another session's id must
// not be able to consume (and thereby approve or deny) it.
func (r *approveRegistry) takeFor(
	noticeID, sessionID, connectorID string,
) (protocol.ApprovalRequest, string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.byID[noticeID]
	if !ok || req.SessionID != sessionID || r.connOf[noticeID] != connectorID {
		return protocol.ApprovalRequest{}, "", false
	}
	delete(r.byID, noticeID)
	delete(r.connOf, noticeID)
	return req, connectorID, true
}

func (r *approveRegistry) remove(noticeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.byID, noticeID)
	delete(r.connOf, noticeID)
}

// onTurnBegin starts an assistant message item.
func (a *App) onTurnBegin(ctx context.Context, conn *connectorConn, msg protocol.TurnBegin) {
	session, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID)
	if err != nil {
		a.log.Warn("turn.begin for an unknown session", "session", msg.SessionID)
		return
	}
	itemID := assistantItemID(msg.TurnID)
	state := a.turns.begin(msg.TurnID, msg.SessionID, itemID)
	orderSeq := store.NowMS()
	state.mu.Lock()
	state.orderSeq = orderSeq
	state.createdAt = orderSeq
	state.mu.Unlock()

	seq, err := a.writeTimeline(ctx, session.ID, func(tx *sql.Tx, seq int64) error {
		return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID:  session.ID,
			ID:         itemID,
			Type:       "message",
			Status:     "running",
			Role:       "assistant",
			OrderSeq:   orderSeq,
			UpdatedSeq: seq,
			Payload:    messageContent(""),
		}, store.NowMS())
	})
	if err != nil {
		a.log.Warn("could not open the assistant message", "error", err)
		return
	}
	a.publishItem(ctx, session.ID, itemID, seq, true)
	a.setSessionStatus(ctx, session.ID, "running")
}

// onTurnDelta appends streamed text to the assistant message.
func (a *App) onTurnDelta(ctx context.Context, conn *connectorConn, msg protocol.TurnDelta) {
	state := a.turns.get(msg.TurnID)
	if state == nil {
		return
	}
	now := time.Now()
	state.mu.Lock()
	state.text.WriteString(msg.Text)
	state.chunks++
	text := state.text.String()
	persist := now.Sub(state.lastWrite) >= deltaFlushInterval
	publish := now.Sub(state.lastPublish) >= deltaPublishInterval
	state.mu.Unlock()

	// Two different costs, so two different clocks. Writing the whole growing
	// payload to SQLite every 120ms is wasteful; sending it over the socket is
	// cheap by comparison, and the clients animate between frames anyway.
	if persist {
		seq, err := a.writeTimeline(ctx, msg.SessionID, func(tx *sql.Tx, seq int64) error {
			return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
				SessionID:  msg.SessionID,
				ID:         state.itemID,
				Type:       "message",
				Status:     "running",
				Role:       "assistant",
				OrderSeq:   state.orderSeq,
				UpdatedSeq: seq,
				Payload:    messageContent(text),
			}, store.NowMS())
		})
		if err != nil {
			a.log.Warn("could not append streamed text", "error", err)
			return
		}
		state.mu.Lock()
		state.lastWrite = now
		state.lastSeq = seq
		state.mu.Unlock()
		publish = true
	}

	if !publish {
		return
	}
	state.mu.Lock()
	state.lastPublish = now
	seq := state.lastSeq
	orderSeq := state.orderSeq
	createdAt := state.createdAt
	state.mu.Unlock()

	a.publishStreamingView(ctx, msg.SessionID, state.itemID, seq, orderSeq, createdAt, text)
}

// publishStreamingView sends a running message without reading it back.
//
// The row was just written or will be within the next tick, and re-reading it
// per frame would double the database work for no new information.
func (a *App) publishStreamingView(
	ctx context.Context, sessionID, itemID string, seq, orderSeq, createdAt int64, text string,
) {
	payload := messageContent(text)
	role := "assistant"
	view := ItemView{
		ID:          itemID,
		SessionID:   sessionID,
		Type:        "message",
		Status:      "running",
		Role:        &role,
		Content:     payload,
		Source:      SourceView{Runtime: "claude"},
		OrderSeq:    orderSeq,
		Revision:    seq,
		ContentHash: contentHash(payload),
		CreatedAt:   isoTime(createdAt),
		UpdatedAt:   isoTime(time.Now().UnixMilli()),
		UpdatedSeq:  seq,
	}
	frame, err := newEnvelope(sessionID, seq, EventItemUpdated, map[string]any{"item": view})
	if err != nil {
		return
	}
	a.broker.Publish(sessionID, frame)
}

// onTurnEnd closes the assistant message and returns the session to idle.
func (a *App) onTurnEnd(ctx context.Context, conn *connectorConn, msg protocol.TurnEnd) {
	state := a.turns.end(msg.TurnID)
	itemID := assistantItemID(msg.TurnID)
	if state != nil {
		itemID = state.itemID
	}
	text := msg.Text
	if text == "" && state != nil {
		state.mu.Lock()
		text = state.text.String()
		state.mu.Unlock()
	}
	status := "done"
	switch msg.Status {
	case "aborted":
		status = "cancelled"
	case "error":
		status = "failed"
	}

	// A failed or cancelled turn with no text would render as an empty row the
	// person cannot interpret, so the reason becomes the row's text.
	if strings.TrimSpace(text) == "" {
		switch status {
		case "failed":
			text = "本轮失败：" + firstNonEmptyText(msg.Error, "连接器没有完成这一轮")
		case "cancelled":
			text = "已中断"
		}
	}
	seq, err := a.writeTimeline(ctx, msg.SessionID, func(tx *sql.Tx, seq int64) error {
		payload := messageContent(text)
		if msg.Error != "" {
			payload = messageContentWithError(text, msg.Error)
		}
		return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID:  msg.SessionID,
			ID:         itemID,
			Type:       "message",
			Status:     status,
			Role:       "assistant",
			OrderSeq:   store.NowMS(),
			UpdatedSeq: seq,
			Payload:    payload,
		}, store.NowMS())
	})
	if err != nil {
		a.log.Warn("could not close the assistant message", "error", err)
		return
	}
	a.publishItem(ctx, msg.SessionID, itemID, seq, false)

	nextStatus := "idle"
	if status == "failed" {
		nextStatus = "error"
	}
	a.setSessionStatus(ctx, msg.SessionID, nextStatus)
}

// onToolBegin records a tool call as its own timeline item.
func (a *App) onToolBegin(ctx context.Context, conn *connectorConn, msg protocol.ToolBegin) {
	if _, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID); err != nil {
		return
	}
	itemID := toolItemID(msg.ToolUseID)
	payload := toolContent(msg.Tool, msg.Input, nil, "")
	seq, err := a.writeTimeline(ctx, msg.SessionID, func(tx *sql.Tx, seq int64) error {
		return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID:  msg.SessionID,
			ID:         itemID,
			Type:       "tool",
			Status:     "running",
			Role:       "tool",
			OrderSeq:   store.NowMS(),
			UpdatedSeq: seq,
			Payload:    payload,
		}, store.NowMS())
	})
	if err != nil {
		a.log.Warn("could not record the tool call", "error", err)
		return
	}
	a.publishItem(ctx, msg.SessionID, itemID, seq, true)
}

// onToolEnd fills in the tool result.
func (a *App) onToolEnd(ctx context.Context, conn *connectorConn, msg protocol.ToolEnd) {
	sessionID := msg.SessionID
	if sessionID == "" {
		return
	}
	itemID := toolItemID(msg.ToolUseID)
	status := "done"
	if !msg.OK {
		status = "failed"
	}
	seq, err := a.writeTimeline(ctx, sessionID, func(tx *sql.Tx, seq int64) error {
		existing, err := a.db.TimelineItemByID(ctx, tx, sessionID, itemID)
		if err != nil {
			return err
		}
		payload := mergeToolOutput(existing.Payload, msg.Output, msg.Error)
		return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID:  sessionID,
			ID:         itemID,
			Type:       "tool",
			Status:     status,
			Role:       "tool",
			OrderSeq:   existing.OrderSeq,
			UpdatedSeq: seq,
			Payload:    payload,
		}, store.NowMS())
	})
	if err != nil {
		a.log.Warn("could not record the tool result", "error", err)
		return
	}
	a.publishItem(ctx, sessionID, itemID, seq, false)
}

// onApprovalRequest turns a permission question into a blocking notice.
func (a *App) onApprovalRequest(ctx context.Context, conn *connectorConn, msg protocol.ApprovalRequest) {
	if _, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID); err != nil {
		return
	}
	noticeID := noticeIDFor(msg.ToolUseID)
	a.approvals.add(noticeID, conn.id, msg)
	a.setSessionStatus(ctx, msg.SessionID, "waiting_approval")

	frame, err := newEnvelope(msg.SessionID, a.currentSeq(ctx, msg.SessionID), EventNoticeUpdated, map[string]any{
		"notice": a.approvalNotice(noticeID, msg),
	})
	if err != nil {
		return
	}
	a.broker.Publish(msg.SessionID, frame)
}

// onApprovalCancel retracts a question.
func (a *App) onApprovalCancel(ctx context.Context, conn *connectorConn, msg protocol.ApprovalCancel) {
	noticeID := noticeIDFor(msg.ToolUseID)
	a.approvals.remove(noticeID)
	req, _ := json.Marshal(msg)
	_ = req
}

// ownedSessionForConnector loads a session and proves the connector owns it.
func (a *App) ownedSessionForConnector(ctx context.Context, conn *connectorConn, sessionID string) (store.Session, error) {
	session, err := a.db.SessionByIDAny(ctx, a.db.Read(), sessionID)
	if err != nil {
		return store.Session{}, err
	}
	if session.ConnectorID != conn.id {
		return store.Session{}, errNotFound
	}
	return session, nil
}

// writeTimeline runs one timeline change under the session lock and allocates
// its revision.
func (a *App) writeTimeline(ctx context.Context, sessionID string, fn func(tx *sql.Tx, seq int64) error) (int64, error) {
	var seq int64
	err := a.db.WithSession(sessionID, func() error {
		return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			allocated, err := store.AllocateSeq(ctx, tx, sessionID, store.NowMS())
			if err != nil {
				return err
			}
			seq = allocated
			return fn(tx, allocated)
		})
	})
	return seq, err
}

// publishItem sends one item event to the session's sockets.
func (a *App) publishItem(ctx context.Context, sessionID, itemID string, seq int64, created bool) {
	item, err := a.db.TimelineItemByID(ctx, a.db.Read(), sessionID, itemID)
	if err != nil {
		return
	}
	eventType := EventItemUpdated
	if created {
		eventType = EventItemCreated
	}
	frame, err := newEnvelope(sessionID, seq, eventType, map[string]any{"item": toView(item, "claude")})
	if err != nil {
		return
	}
	a.broker.Publish(sessionID, frame)
	a.notifySessionDashboard(ctx, sessionID)
}

// setSessionStatus records a status change and tells watchers.
func (a *App) setSessionStatus(ctx context.Context, sessionID, status string) {
	if err := a.db.SetSessionStatus(ctx, sessionID, status); err != nil {
		a.log.Debug("could not set session status", "error", err)
		return
	}
	session, err := a.db.SessionByIDAny(ctx, a.db.Read(), sessionID)
	if err != nil {
		return
	}
	// Two projections of the same change: the session summary drives the list,
	// the runtime state drives the composer's busy/idle affordance.
	if frame, err := newEnvelope(sessionID, session.UpdatedSeq, EventSessionMeta, map[string]any{
		"session": a.sessionView(ctx, session),
	}); err == nil {
		a.broker.Publish(sessionID, frame)
	}
	if frame, err := newEnvelope(sessionID, session.UpdatedSeq, EventRuntimeState, map[string]any{
		"state": a.runtimeState(ctx, session),
	}); err == nil {
		a.broker.Publish(sessionID, frame)
	}
	a.notifySessionDashboard(ctx, sessionID)
}

func (a *App) currentSeq(ctx context.Context, sessionID string) int64 {
	seq, err := a.db.CurrentSeq(ctx, sessionID)
	if err != nil {
		return 0
	}
	return seq
}

func assistantItemID(turnID string) string { return "msg_" + turnID + "_assistant" }
func toolItemID(toolUseID string) string   { return "tool_" + toolUseID }
func noticeIDFor(toolUseID string) string  { return "ntc_" + toolUseID }

// messageContent is the shape the clients render for a message.
func messageContent(text string) json.RawMessage {
	body, _ := json.Marshal(map[string]any{"text": text})
	return body
}

func messageContentWithError(text, message string) json.RawMessage {
	body, _ := json.Marshal(map[string]any{"text": text, "error": message})
	return body
}

func toolContent(tool string, input, output json.RawMessage, errText string) json.RawMessage {
	content := map[string]any{"tool": tool}
	if len(input) > 0 {
		var decoded any
		if json.Unmarshal(input, &decoded) == nil {
			content["input"] = decoded
		}
	}
	if len(output) > 0 {
		var decoded any
		if json.Unmarshal(output, &decoded) == nil {
			content["output"] = decoded
		}
	}
	if errText != "" {
		content["error"] = errText
	}
	body, _ := json.Marshal(content)
	return body
}

func mergeToolOutput(payload, output json.RawMessage, errText string) json.RawMessage {
	var content map[string]any
	if err := json.Unmarshal(payload, &content); err != nil {
		content = map[string]any{}
	}
	if len(output) > 0 {
		var decoded any
		if json.Unmarshal(output, &decoded) == nil {
			content["output"] = decoded
		}
	}
	if errText != "" {
		content["error"] = errText
	}
	body, _ := json.Marshal(content)
	return body
}

// approvalNotice renders a pending approval the way the phone draws it.
func (a *App) approvalNotice(noticeID string, msg protocol.ApprovalRequest) map[string]any {
	var input any
	if len(msg.Input) > 0 {
		_ = json.Unmarshal(msg.Input, &input)
	}
	return map[string]any{
		"noticeId":         noticeID,
		"type":             "interaction",
		"sessionId":        msg.SessionID,
		"source":           map[string]any{"runtime": "claude"},
		"title":            approvalTitle(msg.Tool),
		"message":          nil,
		"severity":         "info",
		"status":           "open",
		"interactionType":  "approval",
		"blocking":         map[string]any{"scope": "session", "targetId": msg.SessionID},
		"responseRequired": true,
		"actions": []map[string]any{
			{"actionId": "allow", "label": "Allow", "style": "primary",
				"input": map[string]any{"required": false, "schema": nil, "uiSchema": nil}},
			{"actionId": "deny", "label": "Reject", "style": "danger",
				"input": map[string]any{"required": false, "schema": nil, "uiSchema": nil}},
		},
		"context":    map[string]any{"tool": msg.Tool, "input": input, "kind": msg.Kind},
		"metadata":   map[string]any{},
		"expiresAt":  nil,
		"revision":   1,
		"createdAt":  timestampNow(),
		"resolvedAt": nil,
	}
}

func approvalTitle(tool string) string {
	if tool == "" {
		return "Permission needed"
	}
	return "Allow " + tool + "?"
}

// firstNonEmptyText returns the first non-empty candidate.
func firstNonEmptyText(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
