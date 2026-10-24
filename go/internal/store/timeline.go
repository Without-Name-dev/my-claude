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
	itemTime := item.OrderSeq
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

// TimelineItems returns a session's items in display order.
func (s *Store) TimelineItems(ctx context.Context, q Querier, sessionID string, limit int) ([]TimelineItem, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := q.QueryContext(ctx, `
		SELECT `+timelineColumns+` FROM timeline_items
		WHERE session_id = ?
		ORDER BY coalesce(item_time, 0) ASC, order_seq ASC
		LIMIT ?`, sessionID, limit)
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
