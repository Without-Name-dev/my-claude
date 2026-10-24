package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"sync"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// historyCooldown keeps a client that keeps refetching an empty session from
// asking the connector every few hundred milliseconds.
const historyCooldown = 20 * time.Second

// maybeRequestHistory asks the connector for a transcript when the session's
// timeline is empty but the session came from a local one.
func (a *App) maybeRequestHistory(ctx context.Context, session store.Session) {
	if session.ExternalID == "" || !a.connectors.Online(session.ConnectorID) {
		return
	}
	if !a.history.allow(session.ID, historyCooldown) {
		return
	}
	a.connectors.Send(session.ConnectorID, protocol.TypeHistoryRequest, protocol.HistoryRequest{
		SessionID:  session.ID,
		ExternalID: session.ExternalID,
		Limit:      maxHistoryRequest,
	})
}

// maxHistoryRequest mirrors the connector's own cap.
const maxHistoryRequest = 400

// historyLimiter tracks the last request per session. Several sockets may ask
// for the same empty session at once, so the map is guarded.
type historyLimiter struct {
	mu   sync.Mutex
	last map[string]time.Time
}

func newHistoryLimiter() *historyLimiter {
	return &historyLimiter{last: map[string]time.Time{}}
}

func (l *historyLimiter) allow(sessionID string, cooldown time.Duration) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if at, ok := l.last[sessionID]; ok && now.Sub(at) < cooldown {
		return false
	}
	l.last[sessionID] = now
	return true
}

// applyHistory stores a transcript and replaces what the session shows.
//
// The whole import shares one revision: it is a single state change, and one
// snapshot event tells every open client to replace its timeline in one go.
func (a *App) applyHistory(ctx context.Context, conn *connectorConn, msg protocol.History) {
	session, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID)
	if err != nil {
		a.log.Warn("history for an unknown session", "session", msg.SessionID)
		return
	}
	if len(msg.Items) == 0 {
		return
	}

	var seq int64
	err = a.db.WithSession(session.ID, func() error {
		return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			allocated, err := store.AllocateSeq(ctx, tx, session.ID, store.NowMS())
			if err != nil {
				return err
			}
			seq = allocated
			for _, item := range msg.Items {
				if err := store.UpsertTimelineItem(ctx, tx, historyItemRow(session.ID, item, allocated), store.NowMS()); err != nil {
					return err
				}
			}
			return nil
		})
	})
	if err != nil {
		a.log.Warn("could not store history", "error", err)
		return
	}

	items, err := a.db.TimelineItems(ctx, a.db.Read(), session.ID, 500)
	if err != nil {
		return
	}
	views := make([]ItemView, 0, len(items))
	for _, item := range items {
		views = append(views, toView(item, session.Runtime))
	}
	frame, err := newEnvelope(session.ID, seq, EventTimelineReset, map[string]any{
		"items": views,
	})
	if err != nil {
		return
	}
	a.broker.Publish(session.ID, frame)
	a.notifySessionDashboard(ctx, session.ID)
	a.log.Info("imported transcript", "session", session.ID, "items", len(views))
}

// historyItemRow maps one transcript item onto the stored shape.
func historyItemRow(sessionID string, item protocol.HistoryItem, seq int64) store.TimelineItem {
	itemTime := item.Timestamp
	if itemTime == 0 {
		itemTime = store.NowMS()
	}
	var payload json.RawMessage
	if item.Type == "tool" {
		payload = toolContent(item.Tool, item.Input, item.Output, "")
	} else {
		payload = messageContent(item.Text)
	}
	return store.TimelineItem{
		SessionID:  sessionID,
		ID:         item.ID,
		Type:       item.Type,
		Status:     item.Status,
		Role:       item.Role,
		OrderSeq:   itemTime,
		UpdatedSeq: seq,
		ItemTime:   &itemTime,
		Payload:    payload,
	}
}
