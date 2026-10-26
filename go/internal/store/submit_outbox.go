package store

import (
	"context"
	"database/sql"
	"errors"
)

var ErrSubmitRequestConflict = errors.New("submit request id was reused with different content")

// SubmitOutboxItem is one server-side submit awaiting a durable connector ack.
type SubmitOutboxItem struct {
	SessionID         string
	RequestID         string
	ConnectorID       string
	ExternalSessionID string
	Payload           []byte
	PayloadHash       string
	State             string
	Error             string
	PreviousStatus    string
	// EnqueueSeq stores the outbox row's rowid, preserving durable insert order.
	EnqueueSeq    int64
	Attempts      int
	LastAttemptAt *int64
	CreatedAt     int64
	UpdatedAt     int64
}

// SubmitOutboxByID reads a submit by its stable client identity.
func (s *Store) SubmitOutboxByID(ctx context.Context, q Querier, sessionID, requestID string) (SubmitOutboxItem, error) {
	return scanSubmitOutbox(q.QueryRowContext(ctx, `
		SELECT session_id, request_id, connector_id, external_session_id,
		       coalesce(payload_json, ''), payload_hash, state, coalesce(error, ''),
		       previous_status, enqueue_seq, attempts, last_attempt_at, created_at, updated_at
		FROM submit_outbox WHERE session_id = ? AND request_id = ?`, sessionID, requestID))
}

// InsertSubmitOutbox inserts the outbox row in the caller's transaction.
func InsertSubmitOutbox(ctx context.Context, tx *sql.Tx, item SubmitOutboxItem) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO submit_outbox
			(session_id, request_id, connector_id, external_session_id, payload_json,
			 payload_hash, state, previous_status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
		item.SessionID, item.RequestID, item.ConnectorID, item.ExternalSessionID,
		string(item.Payload), item.PayloadHash, item.PreviousStatus, item.CreatedAt, item.UpdatedAt)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE submit_outbox SET enqueue_seq = rowid
		WHERE session_id = ? AND request_id = ?`, item.SessionID, item.RequestID)
	return err
}

// PendingSubmitOutbox returns submissions that are due for another delivery attempt.
func (s *Store) PendingSubmitOutbox(ctx context.Context, connectorID string, before int64, limit int) ([]SubmitOutboxItem, error) {
	if limit <= 0 {
		limit = 256
	}
	rows, err := s.read.QueryContext(ctx, `
		SELECT session_id, request_id, connector_id, external_session_id,
		       coalesce(payload_json, ''), payload_hash, state, coalesce(error, ''),
		       previous_status, enqueue_seq, attempts, last_attempt_at, created_at, updated_at
		FROM submit_outbox
		WHERE connector_id = ? AND state = 'pending'
		  AND (last_attempt_at IS NULL OR last_attempt_at <= ?)
		  AND NOT EXISTS (
			SELECT 1 FROM submit_outbox earlier
			WHERE earlier.connector_id = submit_outbox.connector_id
			  AND earlier.session_id = submit_outbox.session_id
			  AND earlier.state = 'pending'
			  AND (earlier.enqueue_seq < submit_outbox.enqueue_seq
				OR (earlier.enqueue_seq = submit_outbox.enqueue_seq
					AND earlier.request_id < submit_outbox.request_id))
		  )
		ORDER BY session_id, enqueue_seq, request_id LIMIT ?`, connectorID, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]SubmitOutboxItem, 0)
	for rows.Next() {
		item, err := scanSubmitOutbox(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// MarkSubmitAttempt records a delivery attempt before it enters the socket queue.
func (s *Store) MarkSubmitAttempt(ctx context.Context, sessionID, requestID string, now int64) (bool, error) {
	result, err := s.write.ExecContext(ctx, `
		UPDATE submit_outbox
		SET attempts = attempts + 1, last_attempt_at = ?, updated_at = ?
		WHERE session_id = ? AND request_id = ? AND state = 'pending'`,
		now, now, sessionID, requestID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n > 0, err
}

// ResolveSubmitAck commits a connector's durable-acceptance or permanent-rejection ack.
func ResolveSubmitAck(ctx context.Context, tx *sql.Tx, connectorID, sessionID, externalSessionID, requestID string, accepted bool, message string, now int64) (SubmitOutboxItem, bool, error) {
	item, err := scanSubmitOutbox(tx.QueryRowContext(ctx, `
		SELECT session_id, request_id, connector_id, external_session_id,
		       coalesce(payload_json, ''), payload_hash, state, coalesce(error, ''),
		       previous_status, enqueue_seq, attempts, last_attempt_at, created_at, updated_at
		FROM submit_outbox
		WHERE connector_id = ? AND session_id = ? AND request_id = ?`,
		connectorID, sessionID, requestID))
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return SubmitOutboxItem{}, false, nil
	}
	if err != nil {
		return SubmitOutboxItem{}, false, err
	}
	if item.State != "pending" || (externalSessionID != "" && item.ExternalSessionID != externalSessionID) {
		return item, false, nil
	}
	state := "accepted"
	if !accepted {
		state = "failed"
		item.Error = message
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE submit_outbox
		SET state = ?, error = ?, payload_json = NULL, updated_at = ?
		WHERE connector_id = ? AND session_id = ? AND request_id = ? AND state = 'pending'`,
		state, nullableString(message, accepted), now, connectorID, sessionID, requestID)
	if err != nil {
		return SubmitOutboxItem{}, false, err
	}
	item.State = state
	item.Payload = nil
	item.UpdatedAt = now
	return item, true, nil
}

func scanSubmitOutbox(row interface{ Scan(...any) error }) (SubmitOutboxItem, error) {
	var item SubmitOutboxItem
	var payload string
	err := row.Scan(&item.SessionID, &item.RequestID, &item.ConnectorID,
		&item.ExternalSessionID, &payload, &item.PayloadHash, &item.State, &item.Error,
		&item.PreviousStatus, &item.EnqueueSeq, &item.Attempts, &item.LastAttemptAt, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return SubmitOutboxItem{}, ErrNotFound
	}
	if err != nil {
		return SubmitOutboxItem{}, err
	}
	item.Payload = []byte(payload)
	return item, nil
}

func nullableString(value string, accepted bool) any {
	if accepted || value == "" {
		return nil
	}
	return value
}
