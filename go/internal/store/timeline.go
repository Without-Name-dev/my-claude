package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

// TimelineItem is one entry in a session's conversation: a message, a tool
// call, an artifact, a marker or a system note.
//
// Payload is stored as JSON text rather than columns because the shapes differ
// by type and the clients already decode them as JSON.
type TimelineItem struct {
	SessionID  string
	ID         string
	Type       string
	Status     string
	Role       string
	OrderSeq   int64
	UpdatedSeq int64
	ItemTime   *int64
	Payload    json.RawMessage
}

// UpsertTimelineItem writes one item and returns the revision it landed on.
//
// The sequence for this change is allocated by the caller (see AllocateSeq) so
// several items written together share one revision. An item whose stored
// revision is newer is left alone: a late write from a reconnect must not
// rewind what clients have already applied.
func UpsertTimelineItem(ctx context.Context, tx *sql.Tx, item TimelineItem, now int64) error {
	var role any
	if item.Role != "" {
		role = item.Role
	}
	orderSeq := item.OrderSeq
	if orderSeq == 0 {
		orderSeq = now
	}
	// Fall back to the resolved sort key, not the raw field: an item written
	// without an order_seq would otherwise be stamped with time zero and sort
	// to the very top of the session.
	itemTime := orderSeq
	if item.ItemTime != nil {
		itemTime = *item.ItemTime
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO timeline_items
			(session_id, id, type, status, role, order_seq, updated_seq, item_time, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (session_id, id) DO UPDATE SET
			type        = excluded.type,
			status      = excluded.status,
			role        = excluded.role,
			order_seq   = excluded.order_seq,
			updated_seq = excluded.updated_seq,
			item_time   = excluded.item_time,
			payload_json = excluded.payload_json
		WHERE excluded.updated_seq >= timeline_items.updated_seq`,
		item.SessionID, item.ID, item.Type, item.Status, role, orderSeq,
		item.UpdatedSeq, itemTime, string(item.Payload))
	return err
}

const timelineColumns = `session_id, id, type, status, coalesce(role, ''), order_seq,
	updated_seq, item_time, payload_json`

func scanTimelineItem(row interface{ Scan(...any) error }) (TimelineItem, error) {
	var it TimelineItem
	var payload string
	err := row.Scan(&it.SessionID, &it.ID, &it.Type, &it.Status, &it.Role,
		&it.OrderSeq, &it.UpdatedSeq, &it.ItemTime, &payload)
	if err != nil {
		if err == sql.ErrNoRows {
			return TimelineItem{}, ErrNotFound
		}
		return TimelineItem{}, err
	}
	it.Payload = json.RawMessage(payload)
	return it, nil
}

// TimelineItems returns a session's newest limit items in display order.
//
// It backs the public share snapshot, which is capped: reading the oldest
// items instead would freeze a long conversation at its opening and hide
// everything the person actually shared.
func (s *Store) TimelineItems(ctx context.Context, q Querier, sessionID string, limit int) ([]TimelineItem, error) {
	return s.timelineWindow(ctx, q, sessionID, limit, 0, false)
}

// TimelineItemsLatest returns the newest limit items in display order.
//
// A long session opens on its current conversation, not on its first page: the
// rows before this window are read with TimelineItemsBefore.
func (s *Store) TimelineItemsLatest(ctx context.Context, q Querier, sessionID string, limit int) ([]TimelineItem, error) {
	return s.timelineWindow(ctx, q, sessionID, limit, 0, false)
}

// TimelineNonHistoryMessages returns every message not imported from a
// transcript. History reconciliation uses these rows to avoid re-importing
// prompts already recorded by the Server, even when they are older than the
// current display window.
func (s *Store) TimelineNonHistoryMessages(ctx context.Context, q Querier, sessionID string) ([]TimelineItem, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+timelineColumns+` FROM timeline_items
		WHERE session_id = ? AND type = 'message' AND substr(id, 1, 5) != 'hist_'
		ORDER BY order_seq`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TimelineItem
	for rows.Next() {
		item, err := scanTimelineItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// TimelineItemsBefore returns the newest limit items older than beforeOrderSeq,
// in display order. It backs mode=history pagination.
func (s *Store) TimelineItemsBefore(ctx context.Context, q Querier, sessionID string, beforeOrderSeq int64, limit int) ([]TimelineItem, error) {
	return s.timelineWindow(ctx, q, sessionID, limit, beforeOrderSeq, true)
}

func (s *Store) timelineWindow(
	ctx context.Context, q Querier, sessionID string, limit int,
	beforeOrderSeq int64, hasBefore bool,
) ([]TimelineItem, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	query := `SELECT ` + timelineColumns + ` FROM timeline_items WHERE session_id = ?`
	args := []any{sessionID}
	if hasBefore {
		query += ` AND order_seq < ? ORDER BY order_seq DESC LIMIT ?`
		args = append(args, beforeOrderSeq, limit)
	} else {
		query += ` ORDER BY coalesce(item_time, 0) DESC, order_seq DESC LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TimelineItem, 0, limit)
	for rows.Next() {
		it, err := scanTimelineItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The query reads newest first so LIMIT keeps the current window; clients
	// render the page oldest first.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// TimelineItemsAfter returns the items changed after a revision, oldest
// change first. Recovery reads this to catch a client up without a snapshot.
func (s *Store) TimelineItemsAfter(ctx context.Context, q Querier, sessionID string, after int64, limit int) ([]TimelineItem, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := q.QueryContext(ctx, `
		SELECT `+timelineColumns+` FROM timeline_items
		WHERE session_id = ? AND updated_seq > ?
		ORDER BY updated_seq ASC, order_seq ASC
		LIMIT ?`, sessionID, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []TimelineItem
	for rows.Next() {
		it, err := scanTimelineItem(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// TimelineItemByID returns one item.
func (s *Store) TimelineItemByID(ctx context.Context, q Querier, sessionID, id string) (TimelineItem, error) {
	row := q.QueryRowContext(ctx,
		`SELECT `+timelineColumns+` FROM timeline_items WHERE session_id = ? AND id = ?`,
		sessionID, id)
	return scanTimelineItem(row)
}

// NowMS is the timestamp every store write stamps rows with.
func NowMS() int64 { return time.Now().UnixMilli() }
