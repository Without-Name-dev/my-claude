package store

import (
	"context"
	"database/sql"
	"time"
)

// ConversationBranch represents a conversation branch.
type ConversationBranch struct {
	SessionID          string
	BranchID           string
	ParentBranchID     string
	BranchPointItemID  string
	BranchPointOrderSeq int64
	Name               string
	CreatedAt          int64
	UpdatedAt          int64
	LastActivityAt     *int64
}

const branchColumns = `session_id, branch_id, coalesce(parent_branch_id, ''),
	branch_point_item_id, branch_point_order_seq, coalesce(name, ''),
	created_at, updated_at, last_activity_at`

func scanBranch(row interface{ Scan(...any) error }) (ConversationBranch, error) {
	var b ConversationBranch
	err := row.Scan(&b.SessionID, &b.BranchID, &b.ParentBranchID,
		&b.BranchPointItemID, &b.BranchPointOrderSeq, &b.Name,
		&b.CreatedAt, &b.UpdatedAt, &b.LastActivityAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return ConversationBranch{}, ErrNotFound
		}
		return ConversationBranch{}, err
	}
	return b, nil
}

// CreateBranch creates a new conversation branch from a specific message.
func (s *Store) CreateBranch(ctx context.Context, tx *sql.Tx, b ConversationBranch) error {
	now := time.Now().UnixMilli()
	if b.BranchID == "" {
		b.BranchID = NewID("br")
	}
	b.CreatedAt = now
	b.UpdatedAt = now
	b.LastActivityAt = &now

	var parentID, name any
	if b.ParentBranchID != "" {
		parentID = b.ParentBranchID
	}
	if b.Name != "" {
		name = b.Name
	}

	_, err := tx.ExecContext(ctx, `
		INSERT INTO conversation_branches
			(session_id, branch_id, parent_branch_id, branch_point_item_id,
			 branch_point_order_seq, name, created_at, updated_at, last_activity_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.SessionID, b.BranchID, parentID, b.BranchPointItemID,
		b.BranchPointOrderSeq, name, b.CreatedAt, b.UpdatedAt, b.LastActivityAt)
	return err
}

// GetBranch retrieves a specific branch.
func (s *Store) GetBranch(ctx context.Context, q Querier, sessionID, branchID string) (ConversationBranch, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+branchColumns+` FROM conversation_branches
		WHERE session_id = ? AND branch_id = ?`,
		sessionID, branchID)
	return scanBranch(row)
}

// ListBranches returns all branches for a session, ordered by creation time.
func (s *Store) ListBranches(ctx context.Context, q Querier, sessionID string) ([]ConversationBranch, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT `+branchColumns+` FROM conversation_branches
		WHERE session_id = ? ORDER BY created_at ASC`,
		sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var branches []ConversationBranch
	for rows.Next() {
		b, err := scanBranch(rows)
		if err != nil {
			return nil, err
		}
		branches = append(branches, b)
	}
	return branches, rows.Err()
}

// UpdateBranchActivity updates the last activity time for a branch.
func (s *Store) UpdateBranchActivity(ctx context.Context, tx *sql.Tx, sessionID, branchID string) error {
	now := time.Now().UnixMilli()
	_, err := tx.ExecContext(ctx, `
		UPDATE conversation_branches
		SET last_activity_at = ?, updated_at = ?
		WHERE session_id = ? AND branch_id = ?`,
		now, now, sessionID, branchID)
	return err
}

// DeleteBranch removes a branch and all its descendants.
func (s *Store) DeleteBranch(ctx context.Context, tx *sql.Tx, sessionID, branchID string) error {
	// CASCADE will handle descendants
	_, err := tx.ExecContext(ctx, `
		DELETE FROM conversation_branches
		WHERE session_id = ? AND branch_id = ?`,
		sessionID, branchID)
	return err
}

// SetActiveBranch sets the currently active branch for a session.
func (s *Store) SetActiveBranch(ctx context.Context, tx *sql.Tx, sessionID, branchID string) error {
	var bid any
	if branchID != "" {
		bid = branchID
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE sessions SET active_branch_id = ? WHERE id = ?`,
		bid, sessionID)
	return err
}

// CopyTimelineItemsToBranch copies timeline items up to branch point into the new branch.
func (s *Store) CopyTimelineItemsToBranch(ctx context.Context, tx *sql.Tx, sessionID, branchID string, upToOrderSeq int64) error {
	_, err := tx.ExecContext(ctx, `
		UPDATE timeline_items
		SET branch_id = ?
		WHERE session_id = ?
		  AND order_seq <= ?
		  AND (branch_id IS NULL OR branch_id = '')`,
		branchID, sessionID, upToOrderSeq)
	return err
}

// TimelineItemsForBranch returns timeline items for a specific branch.
func (s *Store) TimelineItemsForBranch(ctx context.Context, q Querier, sessionID, branchID string, limit int) ([]TimelineItem, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}

	var rows *sql.Rows
	var err error

	if branchID == "" {
		// Main branch: items with NULL or empty branch_id
		rows, err = q.QueryContext(ctx, `
			SELECT `+timelineColumns+` FROM timeline_items
			WHERE session_id = ? AND (branch_id IS NULL OR branch_id = '')
			ORDER BY coalesce(item_time, 0) DESC, order_seq DESC
			LIMIT ?`,
			sessionID, limit)
	} else {
		// Specific branch
		rows, err = q.QueryContext(ctx, `
			SELECT `+timelineColumns+` FROM timeline_items
			WHERE session_id = ? AND branch_id = ?
			ORDER BY coalesce(item_time, 0) DESC, order_seq DESC
			LIMIT ?`,
			sessionID, branchID, limit)
	}

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

	// Reverse to oldest-first display order
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
