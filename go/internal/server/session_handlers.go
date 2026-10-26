package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// sessionCursor is the opaque pagination token for the session list.
type sessionCursor struct {
	Version int    `json:"v"`
	Pinned  int    `json:"p"`
	SortAt  int64  `json:"s"`
	Order   int64  `json:"o"`
	Updated int64  `json:"u"`
	ID      string `json:"i"`
}

func encodeCursor(c sessionCursor) string {
	body, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(body)
}

func decodeCursor(raw string) (sessionCursor, error) {
	body, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return sessionCursor{}, errors.New("invalid session cursor")
	}
	var c sessionCursor
	if err := json.Unmarshal(body, &c); err != nil || c.Version != 1 {
		return sessionCursor{}, errors.New("invalid session cursor")
	}
	return c, nil
}

// handleListSessions returns one page of the dashboard list.
func (a *App) handleListSessions(w http.ResponseWriter, r *http.Request, user store.User) {
	query := r.URL.Query()
	limit := 100
	if raw := query.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			limit = n
		}
	}
	archived := query.Get("archived") == "true"
	var cursor *store.SessionCursor
	if raw := query.Get("cursor"); raw != "" {
		decoded, err := decodeCursor(raw)
		if err != nil {
			httpx.Fail(w, httpx.BadRequest("invalid session cursor"))
			return
		}
		cursor = &store.SessionCursor{
			Pinned:             decoded.Pinned,
			SortAt:             decoded.SortAt,
			LatestItemOrderSeq: decoded.Order,
			UpdatedSeq:         decoded.Updated,
			ID:                 decoded.ID,
		}
	}

	sessions, err := a.db.ListSessions(r.Context(), user.ID, archived, limit+1, cursor)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	hasMore := len(sessions) > limit
	if hasMore {
		sessions = sessions[:limit]
	}
	views := make([]SessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, a.sessionView(r.Context(), s))
	}
	var nextCursor any
	if hasMore && len(sessions) > 0 {
		last := sessions[len(sessions)-1]
		pinned := 0
		if last.Pinned {
			pinned = 1
		}
		nextCursor = encodeCursor(sessionCursor{
			Version: 1, Pinned: pinned, SortAt: last.SortAt,
			Order: last.LatestItemOrderSeq, Updated: last.UpdatedSeq, ID: last.ID,
		})
	}
	httpx.OK(w, map[string]any{
		"sessions":   views,
		"hasMore":    hasMore,
		"nextCursor": nextCursor,
		"serverTime": timestampNow(),
	})
}

// handleListAllSessions returns the whole inventory, archived included.
func (a *App) handleListAllSessions(w http.ResponseWriter, r *http.Request, user store.User) {
	sessions, err := a.db.ListAllSessions(r.Context(), user.ID, false)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	archived, err := a.db.ListAllSessions(r.Context(), user.ID, true)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	sessions = append(sessions, archived...)
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].SortAt > sessions[j].SortAt })

	views := make([]SessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, a.sessionView(r.Context(), s))
	}
	httpx.OK(w, map[string]any{"sessions": views, "serverTime": timestampNow()})
}

// handleSessionMeta returns one session.
func (a *App) handleSessionMeta(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	httpx.OK(w, map[string]any{"session": a.sessionView(r.Context(), session), "serverTime": timestampNow()})
}

// handlePatchSession edits title, pinned and archived.
func (a *App) handlePatchSession(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Title    *string `json:"title"`
		Pinned   *bool   `json:"pinned"`
		Archived *bool   `json:"archived"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if body.Title != nil {
		title := strings.TrimSpace(*body.Title)
		if len(title) < 1 || len(title) > 255 {
			httpx.Fail(w, httpx.BadRequest("title must be 1..255 characters"))
			return
		}
		if err := a.db.SetSessionTitle(r.Context(), session.ID, title); err != nil {
			httpx.Fail(w, err)
			return
		}
	}
	if body.Pinned != nil || body.Archived != nil {
		if err := a.db.Write(r.Context(), func(ctx context.Context, tx *sql.Tx) error {
			if body.Pinned != nil {
				if _, err := tx.ExecContext(ctx,
					`UPDATE sessions SET pinned = ? WHERE id = ?`,
					boolInt(*body.Pinned), session.ID); err != nil {
					return err
				}
			}
			if body.Archived != nil {
				if _, err := tx.ExecContext(ctx,
					`UPDATE sessions SET archived = ?, archived_at = ? WHERE id = ?`,
					boolInt(*body.Archived), archivedAt(*body.Archived, store.NowMS()), session.ID); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			httpx.Fail(w, err)
			return
		}
	}
	updated, err := a.db.SessionByIDAny(r.Context(), a.db.Read(), session.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	// The edit is only visible to another open window if it arrives as an
	// event; the dashboard alone would not update the session being read.
	a.publishSessionMeta(r.Context(), updated)
	httpx.OK(w, map[string]any{"session": a.sessionView(r.Context(), updated), "serverTime": timestampNow()})
}

// handleTimeline returns items in one of the three supported modes.
func (a *App) handleTimeline(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	query := r.URL.Query()
	mode := query.Get("mode")
	if mode == "" {
		mode = "latest"
	}
	limit := 100
	if raw := query.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}

	var items []store.TimelineItem
	var err error
	hasMore := false
	switch mode {
	case "changes":
		after, _ := strconv.ParseInt(query.Get("afterSeq"), 10, 64)
		items, hasMore, err = a.timelineChangesPage(r.Context(), session.ID, after, limit)
	case "history":
		raw := query.Get("beforeOrderSeq")
		before, parseErr := strconv.ParseInt(raw, 10, 64)
		if raw == "" || parseErr != nil || before < 1 {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
				"beforeOrderSeq is required for history mode"))
			return
		}
		items, hasMore, err = a.timelineOlderPage(r.Context(), session.ID, before, limit)
	default:
		items, hasMore, err = a.timelineOlderPage(r.Context(), session.ID, 0, limit)
	}
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	views := make([]ItemView, 0, len(items))
	for _, it := range items {
		views = append(views, toView(it, session.Runtime))
	}
	nextSeq := session.Seq
	if mode == "changes" && hasMore && len(items) > 0 {
		// A paginated reader continues from the last revision it received;
		// session.Seq would skip the rest of this page.
		nextSeq = items[len(items)-1].UpdatedSeq
	}
	httpx.OK(w, map[string]any{
		"sessionId":  session.ID,
		"items":      views,
		"nextSeq":    nextSeq,
		"hasMore":    hasMore,
		"serverTime": timestampNow(),
	})
}

// timelineOlderPage returns the newest items below beforeOrderSeq (zero means
// the session head), in display order.
//
// A page stops at an order_seq boundary. The pagination cursor is a single
// number, so a page that split a group of rows sharing one order_seq would make
// the next request skip the rest of the group. Carrying the whole group with
// the page keeps those rows readable and the next cursor safe; only a single
// group larger than the store's window can still be cut.
func (a *App) timelineOlderPage(ctx context.Context, sessionID string, before int64, limit int) ([]store.TimelineItem, bool, error) {
	want := limit + 1
	for {
		var (
			items []store.TimelineItem
			err   error
		)
		if before > 0 {
			items, err = a.db.TimelineItemsBefore(ctx, a.db.Read(), sessionID, before, want)
		} else {
			items, err = a.db.TimelineItemsLatest(ctx, a.db.Read(), sessionID, want)
		}
		if err != nil || len(items) <= limit {
			return items, false, err
		}
		group := items[len(items)-limit].OrderSeq
		start := len(items) - limit
		for start > 0 && items[start-1].OrderSeq == group {
			start--
		}
		if start == 0 && len(items) >= want {
			if want >= 2000 {
				return items, true, nil
			}
			want = nextTimelinePageSize(want)
			continue
		}
		return items[start:], start > 0, nil
	}
}

// timelineChangesPage returns the items changed after a revision, oldest
// change first, stopping at a revision boundary for the same reason: the
// afterSeq cursor is a single revision, so a page must not split one.
func (a *App) timelineChangesPage(ctx context.Context, sessionID string, after int64, limit int) ([]store.TimelineItem, bool, error) {
	want := limit + 1
	for {
		items, err := a.db.TimelineItemsAfter(ctx, a.db.Read(), sessionID, after, want)
		if err != nil || len(items) <= limit {
			return items, false, err
		}
		group := items[limit-1].UpdatedSeq
		end := limit
		for end < len(items) && items[end].UpdatedSeq == group {
			end++
		}
		if end == len(items) && len(items) >= want {
			if want >= 2000 {
				return items, true, nil
			}
			want = nextTimelinePageSize(want)
			continue
		}
		return items[:end], end < len(items), nil
	}
}

// nextTimelinePageSize doubles a page request, capped by the store's own
// maximum window.
func nextTimelinePageSize(want int) int {
	if want >= 1000 {
		return 2000
	}
	return want * 2
}

// handleSnapshot is the first hydration a client does.
func (a *App) handleSnapshot(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	items, hasMore, err := a.timelineOlderPage(r.Context(), session.ID, 0, limit)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	views := make([]ItemView, 0, len(items))
	for _, it := range items {
		views = append(views, toView(it, session.Runtime))
	}

	// Refresh populated sessions too: Claude Code may have added transcript
	// messages since this client last opened the conversation.
	a.maybeRequestHistory(r.Context(), session)

	httpx.OK(w, map[string]any{
		"session":               a.sessionView(r.Context(), session),
		"state":                 a.runtimeState(r.Context(), session),
		"timeline":              map[string]any{"items": views, "nextSeq": session.Seq, "hasMore": hasMore},
		"approvals":             []any{},
		"notices":               a.pendingNotices(r.Context(), session.ID),
		"effectiveCapabilities": a.capabilitySet(session),
		"runtimeCapabilities":   a.capabilitySet(session),
		"catalogs":              map[string]any{},
		"eventCursor":           Cursor(session.Seq),
		"serverTime":            timestampNow(),
	})
}

// handleEvents replays what a client missed.
func (a *App) handleEvents(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	after := int64(0)
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, ok := ParseCursor(raw)
		if !ok {
			httpx.Fail(w, httpx.BadRequest("invalid event cursor"))
			return
		}
		after = parsed
	}
	current := session.Seq
	if after > current {
		httpx.OK(w, map[string]any{
			"events":           []any{},
			"nextCursor":       Cursor(current),
			"snapshotRequired": true,
			"serverTime":       timestampNow(),
		})
		return
	}

	items, err := a.db.TimelineItemsAfter(r.Context(), a.db.Read(), session.ID, after, 500)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	events := make([]json.RawMessage, 0, len(items)*2)
	for _, it := range items {
		eventType := EventItemUpdated
		if it.UpdatedSeq <= 1 {
			eventType = EventItemCreated
		}
		frame, err := newEnvelope(session.ID, it.UpdatedSeq, eventType, map[string]any{
			"item": toView(it, session.Runtime),
		})
		if err != nil {
			continue
		}
		var decoded json.RawMessage = frame
		events = append(events, decoded)
	}
	httpx.OK(w, map[string]any{
		"events":           events,
		"nextCursor":       Cursor(current),
		"snapshotRequired": len(items) >= 500,
		"serverTime":       timestampNow(),
	})
}

// handleSendMessage queues a prompt on the connector.
func (a *App) handleSendMessage(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Content         string            `json:"content"`
		ClientMessageID *string           `json:"clientMessageId"`
		Attachments     []json.RawMessage `json:"attachments"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	files, apiErr := a.resolveSendAttachments(r.Context(), session.ID, body.Attachments)
	if apiErr != nil {
		httpx.Fail(w, apiErr)
		return
	}
	if !a.connectors.Online(session.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}

	messageID := ""
	if body.ClientMessageID != nil {
		messageID = *body.ClientMessageID
	}
	if messageID == "" {
		messageID = store.NewID("msg")
	}

	if _, err := a.deliverMessage(r.Context(), session, body.Content, messageID, "prompt", files); err != nil {
		if errors.Is(err, errSessionNotLive) {
			failSessionNotLive(w, "the conversation is not live on the desktop; open it there to continue")
			return
		}
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"ok": true, "result": map[string]any{"messageId": messageID}, "error": nil})
}

// handleSteer delivers text into a turn that is already running. The Go
// connector forwards it as a submit with action "steer"; the plugin queues it
// like a normal prompt, which is the closest behaviour this stack offers.
func (a *App) handleSteer(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Content         string            `json:"content"`
		ClientMessageID *string           `json:"clientMessageId"`
		Attachments     []json.RawMessage `json:"attachments"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	files, apiErr := a.resolveSendAttachments(r.Context(), session.ID, body.Attachments)
	if apiErr != nil {
		httpx.Fail(w, apiErr)
		return
	}
	if strings.TrimSpace(body.Content) == "" {
		httpx.Fail(w, httpx.BadRequest("content is required"))
		return
	}
	if !a.connectors.Online(session.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	if !a.connectors.CanDeliver(session.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("no live Claude session on the connector"))
		return
	}
	requestID := ""
	if body.ClientMessageID != nil {
		requestID = *body.ClientMessageID
	}
	if requestID == "" {
		requestID = store.NewID("msg")
	}
	if !a.connectors.Send(session.ConnectorID, protocol.TypeSubmit, protocol.Submit{
		RequestID:         requestID,
		SessionID:         session.ID,
		ExternalSessionID: session.ExternalID,
		Text:              body.Content,
		Action:            "steer",
		Attachments:       submitAttachments(session.ID, files),
	}) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	httpx.OK(w, map[string]any{"ok": true, "result": nil, "error": nil})
}

// deliverMessage records a person's message and hands it to the connector.
//
// The transcript entry is written first, so the phone shows what it typed even
// if the desktop takes a while to pick it up. A conversation with no live
// plugin is opened in a terminal on the desktop first: the message is queued
// either way, but without the terminal the plugin that should run it does not
// exist. If neither frame can be delivered, the entry is marked failed and the
// session status returns to what it was, so a reload does not show a phantom
// delivered prompt.
func (a *App) deliverMessage(
	ctx context.Context, session store.Session, content, messageID, action string, files []messageAttachment,
) (store.Session, error) {
	if action == "" {
		action = "prompt"
	}
	previousStatus := session.Status
	if current, err := a.db.SessionByIDAny(ctx, a.db.Read(), session.ID); err == nil {
		previousStatus = current.Status
	}

	userItemID := "msg_" + messageID + "_user"
	orderSeq := store.NowMS()
	payload := messageContentWithAttachments(content, timelineAttachments(files))
	seq, err := a.writeTimeline(ctx, session.ID, func(tx *sql.Tx, seq int64) error {
		return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID:  session.ID,
			ID:         userItemID,
			Type:       "message",
			Status:     "pending",
			Role:       "user",
			OrderSeq:   orderSeq,
			UpdatedSeq: seq,
			Payload:    payload,
		}, store.NowMS())
	})
	if err != nil {
		return session, err
	}
	a.publishItem(ctx, session.ID, userItemID, seq, true)
	a.setSessionStatus(ctx, session.ID, "running")

	session, opened, err := a.sessionOpenForDelivery(ctx, session)
	if err != nil {
		a.failMessageDelivery(ctx, session, userItemID, payload, orderSeq, previousStatus)
		return session, err
	}
	if !opened {
		a.failMessageDelivery(ctx, session, userItemID, payload, orderSeq, previousStatus)
		return session, errSessionNotLive
	}

	if !a.connectors.Send(session.ConnectorID, protocol.TypeSubmit, protocol.Submit{
		RequestID:         messageID,
		SessionID:         session.ID,
		ExternalSessionID: session.ExternalID,
		Text:              content,
		Action:            action,
		Attachments:       submitAttachments(session.ID, files),
	}) {
		a.failMessageDelivery(ctx, session, userItemID, payload, orderSeq, previousStatus)
		return session, httpx.Conflict("connector is offline")
	}
	return session, nil
}

// failMessageDelivery rewrites a just-written user entry as failed and puts the
// session back into the state it had before the send.
func (a *App) failMessageDelivery(
	ctx context.Context, session store.Session, userItemID string, payload json.RawMessage, orderSeq int64, previousStatus string,
) {
	if failSeq, err := a.writeTimeline(ctx, session.ID, func(tx *sql.Tx, seq int64) error {
		return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID:  session.ID,
			ID:         userItemID,
			Type:       "message",
			Status:     "failed",
			Role:       "user",
			OrderSeq:   orderSeq,
			UpdatedSeq: seq,
			Payload:    payload,
		}, store.NowMS())
	}); err == nil {
		a.publishItem(ctx, session.ID, userItemID, failSeq, false)
	}
	a.setSessionStatus(ctx, session.ID, previousStatus)
}

// handleInterrupt stops the running turn.
func (a *App) handleInterrupt(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	if !a.connectors.Online(session.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	sent := a.connectors.Send(session.ConnectorID, protocol.TypeAbort, protocol.Abort{
		RequestID: store.NewID("abort"),
		SessionID: session.ID,
	})
	if !sent {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	// Only a turn that is actually running has something to stop. Writing
	// "stopping" over an idle session left it blocked there until a reconnect,
	// because nothing else ever clears that status.
	if current, err := a.db.SessionByIDAny(r.Context(), a.db.Read(), session.ID); err == nil &&
		current.Status == "running" {
		a.setSessionStatus(r.Context(), session.ID, "stopping")
		sessionID := session.ID
		time.AfterFunc(interruptSettleTimeout, func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			stored, err := a.db.SessionByIDAny(ctx, a.db.Read(), sessionID)
			if err != nil || stored.Status != "stopping" {
				return
			}
			// The connector dropped or finished the turn without answering.
			a.setSessionStatus(ctx, sessionID, "idle")
		})
	}
	httpx.OK(w, map[string]any{"ok": true, "result": nil, "error": nil})
}

// interruptSettleTimeout is how long a stop may stay unanswered before the
// session is released. A connector that accepted the abort ends the turn long
// before this.
const interruptSettleTimeout = 20 * time.Second

// handleNoticeRespond delivers an approval decision.
func (a *App) handleNoticeRespond(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	noticeID := r.PathValue("notice_id")
	var body struct {
		ActionID string         `json:"actionId"`
		Input    map[string]any `json:"input"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	request, connectorID, found := a.approvals.takeFor(noticeID, session.ID, session.ConnectorID)
	if !found {
		httpx.OK(w, map[string]any{
			"ok":     false,
			"result": nil,
			"error":  map[string]any{"code": "notice_not_found", "message": "the approval is no longer pending"},
		})
		return
	}
	decision := "deny"
	if strings.EqualFold(body.ActionID, "allow") || strings.EqualFold(body.ActionID, "approve") {
		decision = "allow"
	}
	var answers map[string]string
	if raw, ok := body.Input["answers"].(map[string]any); ok {
		answers = map[string]string{}
		for k, v := range raw {
			if text, ok := v.(string); ok {
				answers[k] = text
			}
		}
	}
	if !a.connectors.Send(connectorID, protocol.TypeApprovalAnswer, protocol.ApprovalAnswer{
		RequestID: store.NewID("ans"),
		ToolUseID: request.ToolUseID,
		Decision:  decision,
		Answers:   answers,
	}) {
		// The decision never reached the connector; keep the notice pending so
		// the person can try again once the connector is back.
		a.approvals.add(noticeID, connectorID, request)
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	a.setSessionStatus(r.Context(), session.ID, "running")
	// The decision is resolved for everyone, not only for the tab that sent
	// it; the other open sessions keep the card open until this event.
	a.publishNoticeUpdate(r.Context(), session.ID, noticeID, request, "resolved", 0)
	httpx.OK(w, map[string]any{"ok": true, "result": nil, "error": nil})
}

// ownedSession resolves the path session for the signed-in person.
func (a *App) ownedSession(w http.ResponseWriter, r *http.Request, user store.User) (store.Session, bool) {
	session, err := a.db.SessionByID(r.Context(), a.db.Read(), r.PathValue("session_id"), user.ID)
	if err != nil {
		httpx.Fail(w, httpx.NotFound("session not found"))
		return store.Session{}, false
	}
	return session, true
}

// runtimeState renders the session's runtime state.
func (a *App) runtimeState(ctx context.Context, session store.Session) map[string]any {
	runtime := session.Runtime
	var external any
	if session.ExternalID != "" {
		external = session.ExternalID
	}
	return map[string]any{
		"sessionId":         session.ID,
		"runtime":           runtime,
		"runtimeId":         session.RuntimeID,
		"runtimeType":       runtime,
		"externalSessionId": external,
		"status":            session.Status,
		"selections":        a.selectionState(ctx, session.ID),
		"statusReason":      nil,
		"error":             nil,
		"metadata":          map[string]any{},
		"updatedSeq":        session.UpdatedSeq,
		"createdAt":         isoTime(session.CreatedAt),
		"updatedAt":         isoTime(session.UpdatedAt),
	}
}

// selectionState reads the model and permission choices stored on the session.
func (a *App) selectionState(ctx context.Context, sessionID string) map[string]any {
	out := map[string]any{}
	var model, permission sql.NullString
	if err := a.db.Read().QueryRowContext(ctx,
		`SELECT model_selection_id, permission_selection_id FROM sessions WHERE id = ?`,
		sessionID).Scan(&model, &permission); err != nil {
		return out
	}
	if model.Valid && model.String != "" {
		out["model"] = model.String
	}
	if permission.Valid && permission.String != "" {
		out["permission"] = permission.String
	}
	return out
}

// capabilitySet advertises what this connector can do in this session.
//
// The clients hide controls whose capability is missing, so the list has to
// match what the Go connector actually implements.
func (a *App) capabilitySet(session store.Session) map[string]any {
	ids := []string{
		"session.send_message",
		"session.interrupt",
		"session.steer",
		"session.commands",
		"session.interaction.approval",
		// Images and files a person attaches travel with the message: the
		// server stores them and the connector hands the desktop local paths.
		"runtime.attachment",
	}
	caps := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		caps = append(caps, map[string]any{
			"capabilityId":      id,
			"version":           "1",
			"scope":             "session",
			"runtime":           session.Runtime,
			"sessionId":         session.ID,
			"supported":         true,
			"available":         true,
			"allowed":           true,
			"unavailableReason": nil,
			"parameters":        map[string]any{},
		})
	}
	return map[string]any{"revision": session.UpdatedSeq, "capabilities": caps}
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func archivedAt(archived bool, now int64) any {
	if !archived {
		return nil
	}
	return now
}

// handleCreateSession registers a session the connector can already see, or an
// empty one the person is about to start.
func (a *App) handleCreateSession(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		ConnectorID       string            `json:"connectorId"`
		ProjectID         *string           `json:"projectId"`
		Runtime           string            `json:"runtime"`
		RuntimeID         *string           `json:"runtimeId"`
		ExternalSessionID *string           `json:"externalSessionId"`
		Title             *string           `json:"title"`
		CWD               *string           `json:"cwd"`
		Selections        map[string]string `json:"selections"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	connector, err := a.db.ConnectorByID(r.Context(), body.ConnectorID)
	if err != nil || connector.UserID != user.ID || connector.Revoked != 0 {
		httpx.Fail(w, httpx.NotFound("connector not found"))
		return
	}

	runtime := body.Runtime
	if runtime == "" {
		runtime = "claude"
	}
	runtimeID := runtime
	if body.RuntimeID != nil && *body.RuntimeID != "" {
		runtimeID = *body.RuntimeID
	}
	session, err := a.db.CreateSession(r.Context(), store.Session{
		ConnectorID: body.ConnectorID,
		ProjectID:   deref(body.ProjectID),
		Runtime:     runtime,
		RuntimeID:   runtimeID,
		ExternalID:  deref(body.ExternalSessionID),
		Title:       deref(body.Title),
		CWD:         deref(body.CWD),
		Origin:      "platform",
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	// A session created on one device is inventory for every other device. The
	// dashboard socket is not tied to this request, and the unified stream only
	// re-lists on its slow resync tick, so tell the list now.
	a.dashboards.notify(user.ID)
	httpx.OK(w, map[string]any{
		"session":         a.sessionView(r.Context(), session),
		"connectorResult": nil,
	})
}

// handleCreateAndStart creates a session and immediately sends the first
// message, which is what the phone's "new conversation" button does.
func (a *App) handleCreateAndStart(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		ConnectorID     string             `json:"connectorId"`
		ProjectID       *string            `json:"projectId"`
		Runtime         string             `json:"runtime"`
		RuntimeID       *string            `json:"runtimeId"`
		Title           *string            `json:"title"`
		CWD             *string            `json:"cwd"`
		Content         string             `json:"content"`
		Selections      map[string]*string `json:"selections"`
		Attachments     []json.RawMessage  `json:"attachments"`
		ClientMessageID *string            `json:"clientMessageId"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if len(body.Attachments) > 0 {
		// Uploads are session-scoped, and this session does not exist yet.
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
			"attachments cannot accompany the first message; create the conversation first"))
		return
	}
	connector, err := a.db.ConnectorByID(r.Context(), body.ConnectorID)
	if err != nil || connector.UserID != user.ID || connector.Revoked != 0 {
		httpx.Fail(w, httpx.NotFound("connector not found"))
		return
	}
	if !a.connectors.Online(body.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}

	runtime := body.Runtime
	if runtime == "" {
		runtime = "claude"
	}
	runtimeID := runtime
	if body.RuntimeID != nil && *body.RuntimeID != "" {
		runtimeID = *body.RuntimeID
	}
	session, err := a.db.CreateSession(r.Context(), store.Session{
		ConnectorID: body.ConnectorID,
		ProjectID:   deref(body.ProjectID),
		Runtime:     runtime,
		RuntimeID:   runtimeID,
		Title:       deref(body.Title),
		CWD:         deref(body.CWD),
		Origin:      "platform",
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	// The row exists now, even if the first message below fails; the list on
	// every device (not just the caller's response) has to learn about it.
	a.dashboards.notify(user.ID)
	if err := a.applySelections(r.Context(), session.ID, body.Selections); err != nil {
		httpx.Fail(w, err)
		return
	}
	messageID := ""
	if body.ClientMessageID != nil {
		messageID = *body.ClientMessageID
	}
	if messageID == "" {
		messageID = store.NewID("msg")
	}
	session, err = a.deliverMessage(r.Context(), session, body.Content, messageID, "prompt", nil)
	if err != nil {
		a.setSessionStatus(r.Context(), session.ID, "error")
		if errors.Is(err, errSessionNotLive) {
			failSessionNotLive(w, "the conversation is not live on the desktop; open it there to continue")
			return
		}
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"session":         a.sessionView(r.Context(), session),
		"connectorResult": nil,
		"attachments":     []any{},
	})
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// handleTakeover lets the phone drive the session.
func (a *App) handleTakeover(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	updated, err := a.db.SetSessionTakeover(r.Context(), session.ID, true)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	a.publishSessionMeta(r.Context(), updated)
	httpx.OK(w, map[string]any{"session": a.sessionView(r.Context(), updated)})
}

// handleReleaseTakeover returns control to the desk.
func (a *App) handleReleaseTakeover(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	updated, err := a.db.SetSessionTakeover(r.Context(), session.ID, false)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	a.publishSessionMeta(r.Context(), updated)
	httpx.OK(w, map[string]any{"session": a.sessionView(r.Context(), updated)})
}

// publishSessionMeta pushes a session change to its watchers.
func (a *App) publishSessionMeta(ctx context.Context, session store.Session) {
	frame, err := newEnvelope(session.ID, session.UpdatedSeq, EventSessionMeta, map[string]any{
		"session": a.sessionView(ctx, session),
	})
	if err != nil {
		return
	}
	a.broker.Publish(session.ID, frame)
	a.notifySessionDashboard(ctx, session.ID)
}

// handleBulkRead marks sessions as read. The body is a bare JSON array, which
// is what the shipped clients send.
func (a *App) handleBulkRead(w http.ResponseWriter, r *http.Request, user store.User) {
	ids, err := decodeIDArray(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	views, missing, err := a.applyBulk(r.Context(), user.ID, ids, func(ids []string) error {
		return a.db.MarkSessionsRead(r.Context(), user.ID, ids)
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"sessions": views, "notFound": missing, "serverTime": timestampNow()})
}

// handleBulkArchive sets the archived flag for a list of sessions.
func (a *App) handleBulkArchive(w http.ResponseWriter, r *http.Request, user store.User) {
	archived := r.URL.Path == "/api/v2/sessions/archive"
	ids, err := decodeIDArray(r)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	views, missing, err := a.applyBulk(r.Context(), user.ID, ids, func(ids []string) error {
		return a.db.SetSessionsArchived(r.Context(), user.ID, ids, archived)
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"sessions": views, "notFound": missing, "serverTime": timestampNow()})
}

// applyBulk runs a bulk mutation and renders the sessions that changed.
func (a *App) applyBulk(
	ctx context.Context, userID string, ids []string, mutate func([]string) error,
) ([]SessionView, []string, error) {
	if err := mutate(ids); err != nil {
		return nil, nil, err
	}
	views := make([]SessionView, 0, len(ids))
	missing := make([]string, 0)
	for _, id := range ids {
		session, err := a.db.SessionByID(ctx, a.db.Read(), id, userID)
		if err != nil {
			missing = append(missing, id)
			continue
		}
		views = append(views, a.sessionView(ctx, session))
		// A bulk edit is still a change per session: open windows and the
		// dashboard have to learn about it, not just the caller's response.
		a.publishSessionMeta(ctx, session)
	}
	return views, missing, nil
}

// decodeIDArray reads the bare JSON array these bulk endpoints take, and
// enforces the 1..200 bound the clients already assume.
func decodeIDArray(r *http.Request) ([]string, error) {
	var ids []string
	if err := httpx.Decode(r, &ids); err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > 200 {
		return nil, httpx.BadRequest("expected 1..200 session ids")
	}
	return ids, nil
}

// handleSessionCapabilities reports what this session's connector can do.
func (a *App) handleSessionCapabilities(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	httpx.OK(w, map[string]any{
		"connectorId":   session.ConnectorID,
		"capabilitySet": a.capabilitySet(session),
		"serverTime":    timestampNow(),
	})
}

// handleSessionNotices returns the questions waiting on this session.
func (a *App) handleSessionNotices(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	httpx.OK(w, map[string]any{
		"notices":    a.pendingNotices(r.Context(), session.ID),
		"serverTime": timestampNow(),
	})
}

// pendingNotices renders the approvals the connector is blocked on.
func (a *App) pendingNotices(ctx context.Context, sessionID string) []map[string]any {
	a.approvals.mu.Lock()
	defer a.approvals.mu.Unlock()

	out := make([]map[string]any, 0)
	for noticeID, request := range a.approvals.byID {
		if request.SessionID != sessionID {
			continue
		}
		out = append(out, a.approvalNotice(noticeID, request))
	}
	return out
}

// handleSessionSync asks the connector to reconcile a session with what the
// desktop currently believes. Nothing to do when it is already in step.
func (a *App) handleSessionSync(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	if !a.connectors.Online(session.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	a.maybeRequestHistory(r.Context(), session)
	httpx.OK(w, map[string]any{"ok": true, "result": map[string]any{"queued": true}, "error": nil})
}
