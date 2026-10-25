package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// sessionArchiveBatch keeps each IN list well below SQLite's bind-variable
// limit while still archiving thousands of sessions.
const sessionArchiveBatch = 500

// SessionShare is one public read-only snapshot of a session.
//
// The snapshot lives in one JSON column because it must not change when the
// session keeps moving: a link someone pasted into a chat has to show the same
// conversation a week later.
type SessionShare struct {
	ID                 string
	UserID             string
	SessionID          string
	Scope              string
	SnapshotJSON       string
	AllowedFileIDsJSON string
	CreatedAt          int64
}

const sessionShareColumns = `id, user_id, session_id, scope, snapshot_json,
	allowed_file_ids_json, created_at`

func scanSessionShare(row interface{ Scan(...any) error }) (SessionShare, error) {
	var share SessionShare
	err := row.Scan(&share.ID, &share.UserID, &share.SessionID, &share.Scope,
		&share.SnapshotJSON, &share.AllowedFileIDsJSON, &share.CreatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionShare{}, ErrNotFound
		}
		return SessionShare{}, err
	}
	return share, nil
}

// CreateSessionShare stores one snapshot.
func (s *Store) CreateSessionShare(ctx context.Context, share SessionShare) (SessionShare, error) {
	if share.ID == "" {
		share.ID = NewID("shr")
	}
	if share.CreatedAt == 0 {
		share.CreatedAt = time.Now().UnixMilli()
	}
	if share.AllowedFileIDsJSON == "" {
		share.AllowedFileIDsJSON = "[]"
	}
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO session_shares
				(id, user_id, session_id, scope, snapshot_json, allowed_file_ids_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			share.ID, share.UserID, share.SessionID, share.Scope,
			share.SnapshotJSON, share.AllowedFileIDsJSON, share.CreatedAt)
		return err
	})
	if err != nil {
		return SessionShare{}, err
	}
	return share, nil
}

// SessionShareByID reads a snapshot. It is deliberately not scoped to an owner:
// the public endpoint is reachable by anyone who has the link.
func (s *Store) SessionShareByID(ctx context.Context, id string) (SessionShare, error) {
	row := s.Read().QueryRowContext(ctx,
		`SELECT `+sessionShareColumns+` FROM session_shares WHERE id = ?`, id)
	return scanSessionShare(row)
}

// ListSessionShares returns the caller's links for one session, newest first.
func (s *Store) ListSessionShares(ctx context.Context, sessionID, userID string) ([]SessionShare, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT `+sessionShareColumns+` FROM session_shares
		WHERE session_id = ? AND user_id = ?
		ORDER BY created_at DESC, id DESC`, sessionID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SessionShare, 0)
	for rows.Next() {
		share, err := scanSessionShare(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, share)
	}
	return out, rows.Err()
}

// DeleteSessionShare removes a link only when the caller owns it. A share that
// exists under somebody else's account is reported as missing, not forbidden,
// so the endpoint does not leak the link's existence.
func (s *Store) DeleteSessionShare(ctx context.Context, id, sessionID, userID string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM session_shares
			WHERE id = ? AND session_id = ? AND user_id = ?`, id, sessionID, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// ErrInvalidArchiveScope is returned for a scope the API does not define.
var ErrInvalidArchiveScope = errors.New("invalid archive scope")

// SetSessionsArchivedWithScope archives or restores a set of sessions for one
// user.
//
// connectorID narrows the set to one device; an empty value means every device
// the user owns. scope mirrors the client's "active"/"archived"/"all" filter so
// "archive all active" cannot also flip sessions the person already archived.
func (s *Store) SetSessionsArchivedWithScope(
	ctx context.Context, userID, connectorID string, archived bool, scope string,
) ([]Session, error) {
	scopeClause, err := archiveScopeClause(scope)
	if err != nil {
		return nil, err
	}

	var sessions []Session
	err = s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		query := `
			SELECT s.id FROM sessions s
			JOIN connectors c ON c.id = s.connector_id
			WHERE c.user_id = ?` + scopeClause
		args := []any{userID}
		if connectorID != "" {
			query += ` AND s.connector_id = ?`
			args = append(args, connectorID)
		}
		query += ` ORDER BY s.pinned DESC, s.sort_at DESC, s.created_at DESC`

		rows, err := tx.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		ids := make([]string, 0)
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}

		sessions = make([]Session, 0, len(ids))
		if len(ids) == 0 {
			return nil
		}

		// The ids were selected through the ownership join above, so the
		// updates do not need to repeat it. Both statements are set based:
		// archive-all can touch thousands of sessions while holding the single
		// write connection. They are chunked because one IN list would exceed
		// SQLite's bind-variable limit long before that.
		now := time.Now().UnixMilli()
		for start := 0; start < len(ids); start += sessionArchiveBatch {
			end := start + sessionArchiveBatch
			if end > len(ids) {
				end = len(ids)
			}
			batch := ids[start:end]
			placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")

			updateArgs := make([]any, 0, len(batch)+3)
			updateArgs = append(updateArgs, boolInt(archived), archivedAt(archived, now), now)
			for _, id := range batch {
				updateArgs = append(updateArgs, id)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE sessions SET archived = ?, archived_at = ?, updated_at = ?
				 WHERE id IN (`+placeholders+`)`, updateArgs...); err != nil {
				return err
			}

			selectArgs := make([]any, 0, len(batch))
			for _, id := range batch {
				selectArgs = append(selectArgs, id)
			}
			rows, err := tx.QueryContext(ctx,
				`SELECT `+sessionColumns+` FROM sessions
				 WHERE id IN (`+placeholders+`)`, selectArgs...)
			if err != nil {
				return err
			}
			for rows.Next() {
				session, err := scanSession(rows)
				if err != nil {
					rows.Close()
					return err
				}
				sessions = append(sessions, session)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
		}

		// The chunks come back in arbitrary order; restore the list order.
		sort.SliceStable(sessions, func(i, j int) bool {
			if sessions[i].Pinned != sessions[j].Pinned {
				return sessions[i].Pinned
			}
			if sessions[i].SortAt != sessions[j].SortAt {
				return sessions[i].SortAt > sessions[j].SortAt
			}
			return sessions[i].CreatedAt > sessions[j].CreatedAt
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return sessions, nil
}

// archiveScopeClause turns the API scope into the matching WHERE fragment.
func archiveScopeClause(scope string) (string, error) {
	switch scope {
	case "", "active":
		return ` AND s.archived = 0`, nil
	case "archived":
		return ` AND s.archived = 1`, nil
	case "all":
		return ``, nil
	default:
		return ``, ErrInvalidArchiveScope
	}
}
