package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Session is one conversation with a runtime on a connector.
type Session struct {
	ID           string
	ConnectorID  string
	ProjectID    string
	Runtime      string
	RuntimeID    string
	Origin       string
	ExternalID   string
	Title        string
	CWD          string
	Status       string
	Takeover     bool
	Pinned       bool
	Archived     bool
	SourceState  string
	Seq          int64
	UpdatedSeq   int64
	LastReadSeq  int64
	ResetSeq     int64
	LastTurnEnd  int64
	SortAt       int64
	CreatedAt    int64
	UpdatedAt    int64
	LastActivity *int64
}

const sessionColumns = `id, connector_id, coalesce(project_id, ''), runtime, runtime_id,
	origin, coalesce(external_session_id, ''), coalesce(title, ''), coalesce(cwd, ''),
	status, takeover, pinned, archived, source_state,
	seq, updated_seq, last_read_seq, timeline_reset_seq, latest_turn_end_seq,
	coalesce(sort_at, created_at), created_at, updated_at, last_activity_at`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var s Session
	var takeover, pinned, archived int
	err := row.Scan(&s.ID, &s.ConnectorID, &s.ProjectID, &s.Runtime, &s.RuntimeID,
		&s.Origin, &s.ExternalID, &s.Title, &s.CWD, &s.Status,
		&takeover, &pinned, &archived, &s.SourceState,
		&s.Seq, &s.UpdatedSeq, &s.LastReadSeq, &s.ResetSeq, &s.LastTurnEnd,
		&s.SortAt, &s.CreatedAt, &s.UpdatedAt, &s.LastActivity)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, err
	}
	s.Takeover, s.Pinned, s.Archived = takeover != 0, pinned != 0, archived != 0
	return s, nil
}

// CreateSession inserts a session row.
func (s *Store) CreateSession(ctx context.Context, in Session) (Session, error) {
	now := time.Now().UnixMilli()
	if in.ID == "" {
		in.ID = NewID("ses")
	}
	if in.Origin == "" {
		in.Origin = "platform"
	}
	if in.Runtime == "" {
		in.Runtime = "claude"
	}
	if in.Status == "" {
		in.Status = "idle"
	}
	in.CreatedAt, in.UpdatedAt, in.SortAt = now, now, now
	in.SourceState = "visible"

	var projectID any
	if in.ProjectID != "" {
		projectID = in.ProjectID
	}
	var externalID any
	if in.ExternalID != "" {
		externalID = in.ExternalID
	}
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (id, connector_id, project_id, runtime, runtime_id, origin,
			                      external_session_id, title, cwd, status, source_state,
			                      sort_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'visible', ?, ?, ?)`,
			in.ID, in.ConnectorID, projectID, in.Runtime, in.RuntimeID, in.Origin,
			externalID, in.Title, in.CWD, in.Status, now, now, now)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return in, nil
}

// SessionByID returns one session, restricted to its owner.
func (s *Store) SessionByID(ctx context.Context, q Querier, id, userID string) (Session, error) {
	row := q.QueryRowContext(ctx, `
		SELECT `+sessionColumnsFor("s")+` FROM sessions s
		JOIN connectors c ON c.id = s.connector_id
		WHERE s.id = ? AND c.user_id = ?`, id, userID)
	return scanSession(row)
}

// SessionByIDAny returns a session without an ownership check, for internal
// callers that already resolved the owner.
func (s *Store) SessionByIDAny(ctx context.Context, q Querier, id string) (Session, error) {
	row := q.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	return scanSession(row)
}

// ListSessions returns a user's sessions ordered the way the dashboard shows
// them: pinned first, then most recently active.
func (s *Store) ListSessions(ctx context.Context, userID string, archived bool, limit int) ([]Session, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.Read().QueryContext(ctx, `
		SELECT `+sessionColumnsFor("s")+` FROM sessions s
		JOIN connectors c ON c.id = s.connector_id
		WHERE c.user_id = ? AND s.archived = ?
		ORDER BY s.pinned DESC, s.sort_at DESC, s.created_at DESC
		LIMIT ?`, userID, boolInt(archived), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// SetSessionStatus updates the status without consuming a revision. Status is
// derived from what the connector reports, not from the timeline.
func (s *Store) SetSessionStatus(ctx context.Context, id, status string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET status = ?, updated_at = ? WHERE id = ?`, status, now, id)
		return err
	})
}

// SetSessionArchived flips the archived flag.
func (s *Store) SetSessionArchived(ctx context.Context, id, userID string, archived bool) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE sessions SET archived = ?, archived_at = ?, updated_at = ?
			WHERE id = ? AND connector_id IN (SELECT id FROM connectors WHERE user_id = ?)`,
			boolInt(archived), archivedAt(archived, now), now, id, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SetSessionTitle records a title the person edited.
func (s *Store) SetSessionTitle(ctx context.Context, id, title string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET title = ?, title_source = 'user', updated_at = ? WHERE id = ?`,
			title, now, id)
		return err
	})
}

// AllocateSeq advances a session's revision and returns the new value.
//
// Call it inside a write transaction, with the session lock held, whenever a
// change must be visible to clients as one ordering point. Several timeline
// items written under one revision is normal: the revision is the session's
// clock, not a per-item counter.
func AllocateSeq(ctx context.Context, tx *sql.Tx, sessionID string, now int64) (int64, error) {
	var seq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT seq FROM sessions WHERE id = ?`, sessionID).Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	seq++
	if _, err := tx.ExecContext(ctx, `
		UPDATE sessions SET seq = ?, updated_seq = ?, updated_at = ?, last_activity_at = ?, sort_at = ?
		WHERE id = ?`, seq, seq, now, now, now, sessionID); err != nil {
		return 0, err
	}
	return seq, nil
}

// CurrentSeq reads a session's revision.
func (s *Store) CurrentSeq(ctx context.Context, id string) (int64, error) {
	var seq int64
	err := s.Read().QueryRowContext(ctx, `SELECT seq FROM sessions WHERE id = ?`, id).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return seq, err
}

// Querier is the subset of *sql.DB and *sql.Tx the read helpers accept.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func archivedAt(archived bool, now int64) any {
	if !archived {
		return nil
	}
	return now
}

// sessionColumnsFor is sessionColumns with a table alias.
//
// The expressions have to wrap the qualified name ("coalesce(s.title, ”)"),
// not be prefixed wholesale, or the SQL is invalid.
func sessionColumnsFor(alias string) string {
	a := alias + "."
	return strings.Join([]string{
		a + "id",
		a + "connector_id",
		"coalesce(" + a + "project_id, '')",
		a + "runtime",
		a + "runtime_id",
		a + "origin",
		"coalesce(" + a + "external_session_id, '')",
		"coalesce(" + a + "title, '')",
		"coalesce(" + a + "cwd, '')",
		a + "status",
		a + "takeover",
		a + "pinned",
		a + "archived",
		a + "source_state",
		a + "seq",
		a + "updated_seq",
		a + "last_read_seq",
		a + "timeline_reset_seq",
		a + "latest_turn_end_seq",
		"coalesce(" + a + "sort_at, " + a + "created_at)",
		a + "created_at",
		a + "updated_at",
		a + "last_activity_at",
	}, ", ")
}

// SessionsForConnector returns every session on one connector, for presence
// fan-out.
func (s *Store) SessionsForConnector(ctx context.Context, connectorID string) ([]Session, error) {
	rows, err := s.Read().QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE connector_id = ?`, connectorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}

// UpsertDiscoveredSession records a session the connector found on disk.
//
// The identity is (connector, runtime, external id): the same Claude session
// seen twice must update the existing row rather than create a duplicate.
func (s *Store) UpsertDiscoveredSession(
	ctx context.Context,
	connectorID, projectID, externalID, title, cwd string,
	updatedAt int64,
) (Session, error) {
	now := time.Now().UnixMilli()
	var existingID string
	err := s.Read().QueryRowContext(ctx, `
		SELECT id FROM sessions
		WHERE connector_id = ? AND runtime = 'claude' AND external_session_id = ?`,
		connectorID, externalID).Scan(&existingID)

	if errors.Is(err, sql.ErrNoRows) {
		session, createErr := s.CreateSession(ctx, Session{
			ConnectorID: connectorID,
			ProjectID:   projectID,
			Runtime:     "claude",
			RuntimeID:   "claude",
			ExternalID:  externalID,
			Title:       title,
			CWD:         cwd,
			Origin:      "connector_import",
		})
		if createErr != nil {
			return Session{}, createErr
		}
		_ = s.bumpActivity(ctx, session.ID, updatedAt)
		return s.SessionByIDAny(ctx, s.Read(), session.ID)
	}
	if err != nil {
		return Session{}, err
	}

	return s.updateDiscovered(ctx, existingID, projectID, title, cwd, updatedAt, now)
}

// updateDiscovered refreshes the mutable facts of a discovered session.
func (s *Store) updateDiscovered(
	ctx context.Context, id, projectID, title, cwd string, updatedAt, now int64,
) (Session, error) {
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// Only a title the person typed is frozen. An automatic title keeps
		// following the transcript, because the first scan of a fresh session
		// often has nothing better than an auto-generated slug.
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET
				project_id = coalesce(?, project_id),
				title = CASE WHEN title_source = 'user' THEN title ELSE ? END,
				title_source = CASE WHEN title_source = 'user' THEN 'user' ELSE 'connector' END,
				cwd = coalesce(nullif(?, ''), cwd),
				sort_at = max(coalesce(sort_at, 0), ?),
				updated_at = ?
			WHERE id = ?`,
			nullIfEmpty(projectID), title, cwd, updatedAt, now, id)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s.SessionByIDAny(ctx, s.Read(), id)
}

// bumpActivity records how recently a session actually did something.
func (s *Store) bumpActivity(ctx context.Context, id string, at int64) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET last_activity_at = ?, sort_at = max(coalesce(sort_at,0), ?) WHERE id = ?`,
			at, at, id)
		return err
	})
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// SetSessionTakeover marks whether the phone may drive this session.
//
// Claude Code lets a terminal and a remote client fight over one conversation;
// requiring an explicit takeover keeps a stray phone tap from typing into a
// session someone is using at the desk.
func (s *Store) SetSessionTakeover(ctx context.Context, id string, takeover bool) (Session, error) {
	now := time.Now().UnixMilli()
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET takeover = ?, updated_at = ? WHERE id = ?`,
			boolInt(takeover), now, id)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s.SessionByIDAny(ctx, s.Read(), id)
}

// MarkSessionsRead advances the read cursor to the current revision.
func (s *Store) MarkSessionsRead(ctx context.Context, userID string, ids []string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions SET last_read_seq = seq
				WHERE id = ? AND connector_id IN (SELECT id FROM connectors WHERE user_id = ?)`,
				id, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetSessionsArchived archives or restores a batch of sessions.
func (s *Store) SetSessionsArchived(ctx context.Context, userID string, ids []string, archived bool) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions SET archived = ?, archived_at = ?, updated_at = ?
				WHERE id = ? AND connector_id IN (SELECT id FROM connectors WHERE user_id = ?)`,
				boolInt(archived), archivedAt(archived, now), now, id, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

// MarkSessionMissing records that a transcript disappeared from disk.
//
// The row is kept: the person may still want the archived conversation, and a
// drive that is temporarily unmounted should not erase history.
func (s *Store) MarkSessionMissing(ctx context.Context, connectorID, externalID string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET source_state = 'missing', source_state_at = ?, updated_at = ?
			WHERE connector_id = ? AND external_session_id = ?`,
			now, now, connectorID, externalID)
		return err
	})
}

// ResetStalledSessions returns sessions left mid-turn by a connector that went
// away back to idle.
//
// A connector that just connected has no turn running: anything still marked
// running, stopping or error is a leftover from the previous connection, and
// leaving it there shows the person a permanent error they cannot dismiss.
func (s *Store) ResetStalledSessions(ctx context.Context, connectorID string) ([]string, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT id FROM sessions
		WHERE connector_id = ? AND status IN ('running', 'stopping', 'error')`, connectorID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	now := time.Now().UnixMilli()
	err = s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`UPDATE sessions SET status = 'idle', updated_at = ? WHERE id = ?`,
				now, id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}
