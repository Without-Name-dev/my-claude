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

// historyCooldown keeps a client that keeps refetching an empty session from
// asking the connector every few hundred milliseconds.
const historyCooldown = 20 * time.Second

// historyRefreshInterval is how often a live conversation's transcript is
// pulled in.
//
// A prompt typed on the desktop never passes through the phone, so the
// transcript is the only place the server can learn about it. That copy used to
// be imported once, when the session was first opened, and the phone never saw
// anything typed afterwards. The connector skips a transcript whose size and
// mtime have not changed, so a quiet session costs one stat.
const historyRefreshInterval = 15 * time.Second

// historyQuietWindow is how long a transcript must have been untouched before
// a periodic refresh imports it.
const historyQuietWindow = 10 * time.Second

// historyRefreshLimit bounds one periodic import.
const historyRefreshLimit = 80

// historyItemPublishLimit is the number of imported rows above which a full
// timeline reset is published instead of one event per item.
//
// It is as large as one periodic import can be, so a refresh never resets the
// client's list — a reset discards the reader's loaded window and reorders the
// conversation under them. Only the first import of a conversation, which
// fetches a whole page, still resets.
const historyItemPublishLimit = historyRefreshLimit

// refreshLiveHistories keeps the server's timeline in step with every live
// conversation's transcript.
func (a *App) refreshLiveHistories(ctx context.Context) {
	ticker := time.NewTicker(historyRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for connectorID, live := range a.connectors.LiveByConnector() {
			if !a.connectors.Online(connectorID) {
				continue
			}
			sessions, err := a.db.SessionsForConnector(ctx, connectorID)
			if err != nil {
				continue
			}
			for _, session := range sessions {
				if session.ExternalID == "" || session.Status == "running" {
					// A turn this server is following writes its own rows.
					continue
				}
				if _, ok := live[session.ExternalID]; !ok {
					continue
				}
				// Only the tail is needed: the refresh exists to pick up what
				// was typed since the last look, and a smaller window keeps a
				// long conversation cheap to re-import.
				a.requestHistory(ctx, session, historyRefreshLimit)
			}
		}
	}
}

// maybeRequestHistory asks the connector for a transcript when the session's
// timeline is empty but the session came from a local one.
func (a *App) maybeRequestHistory(ctx context.Context, session store.Session) {
	a.requestHistory(ctx, session, maxHistoryRequest)
}

// requestHistory sends one transcript request with an explicit window.
func (a *App) requestHistory(ctx context.Context, session store.Session, limit int) {
	if session.ExternalID == "" || !a.connectors.Online(session.ConnectorID) {
		return
	}
	if !a.history.allow(session.ID, historyCooldown) {
		return
	}
	a.connectors.Send(session.ConnectorID, protocol.TypeHistoryRequest, protocol.HistoryRequest{
		SessionID:  session.ID,
		ExternalID: session.ExternalID,
		Limit:      limit,
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
// knownMessages collects the role and normalised text of every message the
// session already shows from a source other than a transcript import.
func (a *App) knownMessages(ctx context.Context, sessionID string) map[string]bool {
	known := map[string]bool{}
	items, err := a.db.TimelineItemsLatest(ctx, a.db.Read(), sessionID, 300)
	if err != nil {
		return known
	}
	for _, item := range items {
		if strings.HasPrefix(item.ID, "hist_") {
			continue
		}
		if role, text, ok := messageIdentity(item.Role, item.Payload); ok {
			known[role+"|"+text] = true
		}
	}
	return known
}

// messageIdentity renders a message row as the key the importer compares.
// Whitespace is normalised so a re-wrapped transcript still matches.
func messageIdentity(role string, payload json.RawMessage) (string, string, bool) {
	if role == "" || len(payload) == 0 {
		return "", "", false
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(payload, &decoded) != nil {
		return "", "", false
	}
	text := strings.Join(strings.Fields(decoded.Text), " ")
	if text == "" {
		return "", "", false
	}
	return role, text, true
}

func (a *App) applyHistory(ctx context.Context, conn *connectorConn, msg protocol.History) {
	session, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID)
	if err != nil {
		a.log.Warn("history for an unknown session", "session", msg.SessionID)
		return
	}
	if len(msg.Items) == 0 {
		return
	}
	// A transcript that is still being written belongs to a turn in progress.
	// Importing it now would add a half-streamed answer beside the one the live
	// turn is already showing, so the refresh waits for the file to settle.
	if msg.UpdatedAt > 0 && time.Since(time.UnixMilli(msg.UpdatedAt)) < historyQuietWindow {
		return
	}

	// Messages the server already recorded from the phone (or from a turn it
	// followed) must not be imported again: the transcript copy has a different
	// id, so only the role and the text identify it.
	known := a.knownMessages(ctx, session.ID)
	var seq int64
	changed := 0
	written := make([]store.TimelineItem, 0, len(msg.Items))
	created := make([]bool, 0, len(msg.Items))
	err = a.db.WithSession(session.ID, func() error {
		return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			rows := make([]store.TimelineItem, 0, len(msg.Items))
			for _, item := range msg.Items {
				row := historyItemRow(session.ID, item, 0)
				if role, text, ok := messageIdentity(item.Role, row.Payload); ok && known[role+"|"+text] {
					continue
				}
				existing, err := a.db.TimelineItemByID(ctx, tx, session.ID, item.ID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				isNew := errors.Is(err, store.ErrNotFound)
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
				created = append(created, isNew)
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
				written = append(written, row)
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

	// A periodic refresh usually brings one or two messages. Publishing those
	// as item events keeps the phone's list intact; a full timeline reset for
	// every import made the page rebuild and jump while the reader was
	// scrolling. A first import (a whole page) still resets, which is cheaper
	// than hundreds of events and is the one case where the list is new anyway.
	if changed <= historyItemPublishLimit {
		for i, row := range written {
			a.publishItem(ctx, session.ID, row.ID, row.UpdatedSeq, created[i])
		}
		a.log.Info("imported transcript", "session", session.ID, "items", changed, "mode", "items")
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
