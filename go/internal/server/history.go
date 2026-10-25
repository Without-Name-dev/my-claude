package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// applyHistory reconciles a transcript with what the session already shows.
//
// The connector answers with a window of the conversation, not the whole
// transcript, so items missing from it are not deleted. Existing rows are only
// rewritten when the transcript really differs: an unchanged item keeps its
// revision, and a changed one keeps the position it already had, so a resync
// cannot reorder or duplicate a conversation the person is reading.
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
	changed := 0
	err = a.db.WithSession(session.ID, func() error {
		return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			rows := make([]store.TimelineItem, 0, len(msg.Items))
			for _, item := range msg.Items {
				row := historyItemRow(session.ID, item, 0)
				existing, err := a.db.TimelineItemByID(ctx, tx, session.ID, item.ID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				if err == nil {
					if historyItemUnchanged(existing, row) {
						continue
					}
					// A turn still streaming owns its row; its turn.end writes
					// the final text and a transcript copy must not race it.
					if a.turns.hasItem(item.ID) {
						continue
					}
					row.OrderSeq = existing.OrderSeq
					if existing.ItemTime != nil {
						row.ItemTime = existing.ItemTime
					}
				}
				rows = append(rows, row)
			}
			if len(rows) == 0 {
				return nil
			}
			allocated, err := store.AllocateSeq(ctx, tx, session.ID, store.NowMS())
			if err != nil {
				return err
			}
			seq = allocated
			for _, row := range rows {
				row.UpdatedSeq = allocated
				if err := store.UpsertTimelineItem(ctx, tx, row, store.NowMS()); err != nil {
					return err
				}
			}
			changed = len(rows)
			return nil
		})
	})
	if err != nil {
		a.log.Warn("could not store history", "error", err)
		return
	}
	if changed == 0 {
		a.log.Debug("history already in sync", "session", session.ID, "items", len(msg.Items))
		return
	}

	items, err := a.db.TimelineItemsLatest(ctx, a.db.Read(), session.ID, 500)
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
	// The import allocated a revision, and AllocateSeq also moves the session's
	// seq, sort key and last-activity time. Opening a discovered conversation
	// must therefore reorder the list and flip its unread flag, which only a
	// dashboard snapshot can express. A transcript import is an inventory
	// change even though its payload is a timeline reset.
	a.notifySessionDashboard(ctx, session.ID)
	a.log.Info("imported transcript", "session", session.ID, "items", len(views))
}

// historyItemUnchanged reports whether the stored row already carries the
// transcript's state, so a resync does not bump a revision nobody can see.
//
// Position is not compared: the stored row already has one, and a transcript
// timestamp only decides where a brand-new item goes.
func historyItemUnchanged(existing, incoming store.TimelineItem) bool {
	return existing.Type == incoming.Type &&
		existing.Status == incoming.Status &&
		existing.Role == incoming.Role &&
		contentHash(existing.Payload) == contentHash(incoming.Payload)
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
