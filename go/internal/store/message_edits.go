package store

import (
	"context"
	"database/sql"
	"encoding/json"
)

// MessageEditHistory represents one historical version of a message.
type MessageEditHistory struct {
	SessionID   string
	MessageID   string
	EditSeq     int
	ContentJSON json.RawMessage
	EditedAt    int64
}

// SaveMessageEdit records the previous content before editing a message.
func (s *Store) SaveMessageEdit(ctx context.Context, tx *sql.Tx, sessionID, messageID string, content json.RawMessage, editSeq int, now int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO message_edit_history (session_id, message_id, edit_seq, content_json, edited_at)
		VALUES (?, ?, ?, ?, ?)`,
		sessionID, messageID, editSeq, string(content), now)
	return err
}

// GetMessageEditHistory returns all edit versions for a message.
func (s *Store) GetMessageEditHistory(ctx context.Context, q Querier, sessionID, messageID string) ([]MessageEditHistory, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT session_id, message_id, edit_seq, content_json, edited_at
		FROM message_edit_history
		WHERE session_id = ? AND message_id = ?
		ORDER BY edit_seq ASC`,
		sessionID, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []MessageEditHistory
	for rows.Next() {
		var h MessageEditHistory
		var content string
		if err := rows.Scan(&h.SessionID, &h.MessageID, &h.EditSeq, &content, &h.EditedAt); err != nil {
			return nil, err
		}
		h.ContentJSON = json.RawMessage(content)
		history = append(history, h)
	}
	return history, rows.Err()
}

// DeleteTimelineItemsAfter removes all timeline items after a given orderSeq.
// Used when editing a message to truncate subsequent conversation.
func (s *Store) DeleteTimelineItemsAfter(ctx context.Context, tx *sql.Tx, sessionID string, afterOrderSeq int64) error {
	_, err := tx.ExecContext(ctx, `
		DELETE FROM timeline_items
		WHERE session_id = ? AND order_seq > ?`,
		sessionID, afterOrderSeq)
	return err
}
