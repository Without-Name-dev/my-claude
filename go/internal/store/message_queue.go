package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// QueueMessage represents a message in the queue
type QueueMessage struct {
	ID            string    `json:"id"`
	SessionID     string    `json:"sessionId"`
	Content       string    `json:"content"`
	Attachments   []string  `json:"attachments,omitempty"`
	Status        string    `json:"status"` // queued, running, completed, error, cancelled
	Source        string    `json:"source"` // terminal, ios, web
	ErrorMessage  string    `json:"errorMessage,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
	StartedAt     *time.Time `json:"startedAt,omitempty"`
	CompletedAt   *time.Time `json:"completedAt,omitempty"`
	Seq           int64     `json:"seq"`
}

// EnqueueMessage adds a message to the queue
func (s *Store) EnqueueMessage(ctx context.Context, msg QueueMessage) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// Allocate sequence number
		var lastSeq int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO queue_sequences (session_id, last_seq) VALUES (?, 0)
			 ON CONFLICT(session_id) DO UPDATE SET last_seq = last_seq + 1
			 RETURNING last_seq`, msg.SessionID).Scan(&lastSeq)
		if err != nil {
			return err
		}

		msg.Seq = lastSeq
		attachmentsJSON, _ := json.Marshal(msg.Attachments)

		_, err = tx.ExecContext(ctx, `
			INSERT INTO message_queue (id, session_id, content, attachments, status, source,
				error_message, created_at, started_at, completed_at, seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			msg.ID, msg.SessionID, msg.Content, string(attachmentsJSON), msg.Status, msg.Source,
			msg.ErrorMessage, msg.CreatedAt.UnixMilli(), timeToMilli(msg.StartedAt),
			timeToMilli(msg.CompletedAt), msg.Seq)
		return err
	})
}

// GetQueueMessages retrieves queue messages, optionally since a sequence number
func (s *Store) GetQueueMessages(ctx context.Context, sessionID string, sinceSeq int64) ([]QueueMessage, error) {
	var messages []QueueMessage

	rows, err := s.Read().QueryContext(ctx, `
		SELECT id, session_id, content, attachments, status, source, error_message,
			created_at, started_at, completed_at, seq
		FROM message_queue
		WHERE session_id = ? AND seq > ?
		ORDER BY seq ASC`, sessionID, sinceSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var msg QueueMessage
		var attachmentsJSON string
		var createdAt, startedAt, completedAt sql.NullInt64

		err := rows.Scan(&msg.ID, &msg.SessionID, &msg.Content, &attachmentsJSON,
			&msg.Status, &msg.Source, &msg.ErrorMessage, &createdAt, &startedAt,
			&completedAt, &msg.Seq)
		if err != nil {
			return nil, err
		}

		json.Unmarshal([]byte(attachmentsJSON), &msg.Attachments)
		msg.CreatedAt = time.UnixMilli(createdAt.Int64)
		if startedAt.Valid {
			t := time.UnixMilli(startedAt.Int64)
			msg.StartedAt = &t
		}
		if completedAt.Valid {
			t := time.UnixMilli(completedAt.Int64)
			msg.CompletedAt = &t
		}

		messages = append(messages, msg)
	}

	return messages, rows.Err()
}

// UpdateMessageStatus updates the status of a queued message
func (s *Store) UpdateMessageStatus(ctx context.Context, id, status, errorMsg string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().UnixMilli()

		query := `UPDATE message_queue SET status = ?, error_message = ?`
		args := []interface{}{status, errorMsg}

		if status == "running" {
			query += `, started_at = ?`
			args = append(args, now)
		} else if status == "completed" || status == "error" || status == "cancelled" {
			query += `, completed_at = ?`
			args = append(args, now)
		}

		query += ` WHERE id = ?`
		args = append(args, id)

		_, err := tx.ExecContext(ctx, query, args...)
		return err
	})
}

// DequeueNextMessage gets the next queued message for processing
func (s *Store) DequeueNextMessage(ctx context.Context, sessionID string) (*QueueMessage, error) {
	var msg QueueMessage
	var attachmentsJSON string
	var createdAt sql.NullInt64

	err := s.Read().QueryRowContext(ctx, `
		SELECT id, session_id, content, attachments, status, source, created_at, seq
		FROM message_queue
		WHERE session_id = ? AND status = 'queued'
		ORDER BY seq ASC
		LIMIT 1`, sessionID).Scan(&msg.ID, &msg.SessionID, &msg.Content,
			&attachmentsJSON, &msg.Status, &msg.Source, &createdAt, &msg.Seq)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	json.Unmarshal([]byte(attachmentsJSON), &msg.Attachments)
	msg.CreatedAt = time.UnixMilli(createdAt.Int64)

	return &msg, nil
}

// CancelQueuedMessage cancels a queued message
func (s *Store) CancelQueuedMessage(ctx context.Context, id string) error {
	return s.UpdateMessageStatus(ctx, id, "cancelled", "")
}

func timeToMilli(t *time.Time) interface{} {
	if t == nil {
		return nil
	}
	return t.UnixMilli()
}
