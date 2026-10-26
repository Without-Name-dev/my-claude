package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	connID  string
	itemID  string
	// rev is the last delta revision applied; deltas arrive numbered from one
	// and a replay must not be appended twice.
	rev int
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

func (r *turnRegistry) begin(turnID, sessionID, connID, itemID string) *turnState {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := &turnState{session: sessionID, connID: connID, itemID: itemID}
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

// hasItem reports whether a running turn owns a timeline item. A history
// import consults it so a call that is still streaming keeps the live row.
func (r *turnRegistry) hasItem(itemID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, state := range r.turns {
		if state.itemID == itemID {
			return true
		}
	}
	return false
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

// take removes one notice and returns it, so the caller can publish the
// resolved state.
func (r *approveRegistry) take(noticeID string) (protocol.ApprovalRequest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.byID[noticeID]
	if !ok {
		return protocol.ApprovalRequest{}, false
	}
	delete(r.byID, noticeID)
	delete(r.connOf, noticeID)
	return req, true
}

// takeSession removes every notice for one session and returns them.
func (r *approveRegistry) takeSession(sessionID string) []pendingApproval {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := make([]pendingApproval, 0)
	for noticeID, req := range r.byID {
		if req.SessionID != sessionID {
			continue
		}
		removed = append(removed, pendingApproval{noticeID: noticeID, request: req})
		delete(r.byID, noticeID)
		delete(r.connOf, noticeID)
	}
	return removed
}

// takeForConnector retracts every question one connector was waiting on.
//
// The connector is gone, so nobody can answer them any more; leaving the cards
// pending kept the session blocked on a decision nothing was listening for.
func (r *approveRegistry) takeForConnector(connectorID string) []pendingApproval {
	r.mu.Lock()
	defer r.mu.Unlock()
	removed := make([]pendingApproval, 0)
	for noticeID, id := range r.connOf {
		if id != connectorID {
			continue
		}
		removed = append(removed, pendingApproval{noticeID: noticeID, request: r.byID[noticeID]})
		delete(r.byID, noticeID)
		delete(r.connOf, noticeID)
	}
	return removed
}

// pendingApproval is one question retracted in bulk.
type pendingApproval struct {
	noticeID string
	request  protocol.ApprovalRequest
}

// expireSessionApprovals retracts the questions a finished turn can no longer
// answer. Leaving them pending would keep the session blocked on a decision the
// connector is not waiting for anymore.
// expireConnectorApprovals retracts the questions a lost connector will never
// answer.
func (a *App) expireConnectorApprovals(ctx context.Context, connectorID string) {
	for _, pending := range a.approvals.takeForConnector(connectorID) {
		a.publishNoticeUpdate(ctx, pending.request.SessionID, pending.noticeID, pending.request, "expired", 0)
	}
}

func (a *App) expireSessionApprovals(ctx context.Context, sessionID string, seq int64) {
	for _, pending := range a.approvals.takeSession(sessionID) {
		a.publishNoticeUpdate(ctx, sessionID, pending.noticeID, pending.request, "expired", seq)
	}
}

// publishNoticeUpdate tells clients a pending question changed state.
//
// A resolution is a new revision of the same notice, so clients replace the
// card they drew instead of leaving it open forever.
func (a *App) publishNoticeUpdate(
	ctx context.Context, sessionID, noticeID string,
	request protocol.ApprovalRequest, status string, seq int64,
) {
	if seq == 0 {
		seq = a.currentSeq(ctx, sessionID)
	}
	notice := a.approvalNotice(noticeID, request)
	notice["status"] = status
	notice["responseRequired"] = false
	notice["revision"] = 2
	notice["updatedSeq"] = seq
	notice["updatedAt"] = timestampNow()
	notice["resolvedAt"] = timestampNow()
	frame, err := newEnvelope(sessionID, seq, EventNoticeUpdated, map[string]any{"notice": notice})
	if err != nil {
		return
	}
	a.broker.Publish(sessionID, frame)
}

// onTurnBegin starts an assistant message item.
func (a *App) onTurnBegin(ctx context.Context, conn *connectorConn, msg protocol.TurnBegin) {
	session, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID)
	if err != nil {
		a.log.Warn("turn.begin for an unknown session", "session", msg.SessionID)
		return
	}
	itemID := assistantItemID(msg.TurnID)
	state := a.turns.begin(msg.TurnID, msg.SessionID, conn.id, itemID)
	orderSeq := store.NowMS()
	state.mu.Lock()
	state.orderSeq = orderSeq
	state.createdAt = orderSeq
	state.mu.Unlock()

	userItemID := ""
	seq, err := a.writeTimeline(ctx, session.ID, func(tx *sql.Tx, seq int64) error {
		if err := store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID:  session.ID,
			ID:         itemID,
			Type:       "message",
			Status:     "running",
			Role:       "assistant",
			OrderSeq:   orderSeq,
			UpdatedSeq: seq,
			Payload:    messageContent(""),
		}, store.NowMS()); err != nil {
			return err
		}
		userItemID, err = a.updateSubmittedMessageStatus(ctx, tx, session.ID, msg.RequestID, "running", seq)
		return err
	})
	if err != nil {
		a.log.Warn("could not open the assistant message", "error", err)
		return
	}
	a.publishItem(ctx, session.ID, itemID, seq, true)
	if userItemID != "" {
		a.publishItem(ctx, session.ID, userItemID, seq, false)
	}
	a.setSessionStatus(ctx, session.ID, "running")
}

// updateSubmittedMessageStatus records queue progress on the matching user
// bubble. RequestID is the submit id, independent of the runtime's turn id.
func (a *App) updateSubmittedMessageStatus(
	ctx context.Context, tx *sql.Tx, sessionID, requestID, status string, seq int64,
) (string, error) {
	if requestID == "" {
		return "", nil
	}
	itemID := "msg_" + requestID + "_user"
	item, err := a.db.TimelineItemByID(ctx, tx, sessionID, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if item.Type != "message" || item.Role != "user" || item.Status == status {
		return "", nil
	}
	if item.Status == "done" || item.Status == "failed" || item.Status == "cancelled" {
		return "", nil
	}
	item.Status = status
	item.UpdatedSeq = seq
	if err := store.UpsertTimelineItem(ctx, tx, item, store.NowMS()); err != nil {
		return "", err
	}
	return itemID, nil
}

// onTurnDelta appends streamed text to the assistant message.
func (a *App) onTurnDelta(ctx context.Context, conn *connectorConn, msg protocol.TurnDelta) {
	state := a.turns.get(msg.TurnID)
	if state == nil {
		return
	}
	now := time.Now()
	state.mu.Lock()
	// The turn belongs to the connector that opened it. Another connector must
	// not be able to append text to (or close) somebody else's message.
	if state.connID != conn.id || state.session != msg.SessionID {
		state.mu.Unlock()
		return
	}
	// Rev is one past the revision already applied. A lower value is a replay
	// after a reconnect and must not be appended again. A higher value means
	// one delta never arrived: the missing span is unrecoverable, but refusing
	// the rest of the turn left the phone frozen on the last complete chunk
	// until turn.end, so the gap is accepted and the stream continues.
	if msg.Rev != 0 && msg.Rev <= state.rev {
		state.mu.Unlock()
		return
	}
	if msg.Rev != 0 {
		state.rev = msg.Rev
	}
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
	// Check ownership before touching the registry: otherwise a connector could
	// end another connector's running turn by guessing its turn id.
	if _, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID); err != nil {
		a.log.Warn("turn.end for an unknown session", "session", msg.SessionID)
		return
	}
	if state := a.turns.get(msg.TurnID); state != nil {
		state.mu.Lock()
		owned := state.connID == conn.id && state.session == msg.SessionID
		state.mu.Unlock()
		if !owned {
			return
		}
	}
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
	userItemID := ""
	seq, err := a.writeTimeline(ctx, msg.SessionID, func(tx *sql.Tx, seq int64) error {
		// The turn keeps the position it was given when it began. Restamping it
		// here would move the answer below anything that arrived while it ran.
		orderSeq := store.NowMS()
		if state != nil {
			state.mu.Lock()
			orderSeq = state.orderSeq
			state.mu.Unlock()
		} else if existing, lookupErr := a.db.TimelineItemByID(ctx, tx, msg.SessionID, itemID); lookupErr == nil {
			// The in-memory turn was lost to a restart; the row it already
			// wrote still remembers where the message belongs.
			orderSeq = existing.OrderSeq
		}
		payload := messageContent(text)
		if msg.Error != "" {
			payload = messageContentWithError(text, msg.Error)
		}
		if err := store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID:  msg.SessionID,
			ID:         itemID,
			Type:       "message",
			Status:     status,
			Role:       "assistant",
			OrderSeq:   orderSeq,
			UpdatedSeq: seq,
			Payload:    payload,
		}, store.NowMS()); err != nil {
			return err
		}
		messageStatus := "done"
		if status == "failed" || status == "cancelled" {
			messageStatus = status
		}
		updatedUserItemID, updateErr := a.updateSubmittedMessageStatus(ctx, tx, msg.SessionID, msg.RequestID, messageStatus, seq)
		userItemID = updatedUserItemID
		return updateErr
	})
	if err != nil {
		a.log.Warn("could not close the assistant message", "error", err)
		return
	}
	a.publishItem(ctx, msg.SessionID, itemID, seq, false)
	if userItemID != "" {
		a.publishItem(ctx, msg.SessionID, userItemID, seq, false)
	}
	a.expireSessionApprovals(ctx, msg.SessionID, seq)

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
	if _, err := a.ownedSessionForConnector(ctx, conn, sessionID); err != nil {
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
	if _, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID); err != nil {
		return
	}
	noticeID := noticeIDFor(msg.ToolUseID)
	request, ok := a.approvals.take(noticeID)
	if !ok {
		return
	}
	a.publishNoticeUpdate(ctx, msg.SessionID, noticeID, request, "cancelled", 0)
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
	// This write allocated a revision, which also advanced the session's seq,
	// sort key and last-activity time. Those are list fields (unread dot,
	// ordering, relative time), so an item creation or completion is an
	// inventory change even though the payload is a timeline item. Streamed
	// deltas do not come through here: they use publishStreamingView and stay
	// quiet.
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

// messageContentWithAttachments is the stored form of a person's message. The
// attachment list is what every client draws under the bubble; the bytes stay
// in the session attachment store and are fetched by fileId.
func messageContentWithAttachments(text string, attachments []map[string]any) json.RawMessage {
	body := map[string]any{"text": text}
	if len(attachments) > 0 {
		body["attachments"] = attachments
	}
	raw, _ := json.Marshal(body)
	return raw
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
