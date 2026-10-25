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

	// LatestItemOrderSeq is the sort key of the newest timeline item. It is
	// filled by the paginated list queries and is part of the list cursor;
	// single-session reads leave it zero.
	LatestItemOrderSeq int64
}

const sessionColumns = `id, connector_id, coalesce(project_id, ''), runtime, runtime_id,
	origin, coalesce(external_session_id, ''), coalesce(title, ''), coalesce(cwd, ''),
	status, takeover, pinned, archived, source_state,
	seq, updated_seq, last_read_seq, timeline_reset_seq, latest_turn_end_seq,
	coalesce(sort_at, created_at), created_at, updated_at, last_activity_at`

// sessionIdentityColumns is the unique identity of a runtime session; keep the
// text in sync with uq_sessions_connector_runtime_external.
const sessionIdentityColumns = "sessions.connector_id, sessions.runtime, sessions.external_session_id"

func scanSession(row interface{ Scan(...any) error }, extra ...any) (Session, error) {
	var s Session
	var takeover, pinned, archived int
	dest := []any{&s.ID, &s.ConnectorID, &s.ProjectID, &s.Runtime, &s.RuntimeID,
		&s.Origin, &s.ExternalID, &s.Title, &s.CWD, &s.Status,
		&takeover, &pinned, &archived, &s.SourceState,
		&s.Seq, &s.UpdatedSeq, &s.LastReadSeq, &s.ResetSeq, &s.LastTurnEnd,
		&s.SortAt, &s.CreatedAt, &s.UpdatedAt, &s.LastActivity}
	dest = append(dest, extra...)
	err := row.Scan(dest...)
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
	inserted := false
	id := in.ID
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (id, connector_id, project_id, runtime, runtime_id, origin,
			                      external_session_id, title, cwd, status, source_state,
			                      sort_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'visible', ?, ?, ?)`,
			in.ID, in.ConnectorID, projectID, in.Runtime, in.RuntimeID, in.Origin,
			externalID, in.Title, in.CWD, in.Status, now, now, now)
		if err == nil {
			inserted = true
			return nil
		}
		// Another row may already represent this runtime session (for example
		// when the connector inventory and the API raced). Reuse it rather than
		// surfacing a constraint violation as a 500.
		if in.ExternalID == "" || !isUniqueConstraint(err, sessionIdentityColumns) {
			return err
		}
		return tx.QueryRowContext(ctx, `
			SELECT id FROM sessions
			WHERE connector_id = ? AND runtime = ? AND external_session_id = ?`,
			in.ConnectorID, in.Runtime, in.ExternalID).Scan(&id)
	})
	if err != nil {
		return Session{}, err
	}
	if !inserted {
		return s.SessionByIDAny(ctx, s.Read(), id)
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

// SessionCursor is the position of the last row of a session page. Its fields
// mirror the values documented for GET /sessions?cursor=.
type SessionCursor struct {
	Pinned             int
	SortAt             int64
	LatestItemOrderSeq int64
	UpdatedSeq         int64
	ID                 string
}

// sessionLatestOrderSeqSQL resolves the newest timeline item's sort key. The
// correlated lookup is driven by idx_timeline_items_session_latest.
const sessionLatestOrderSeqSQL = `coalesce((SELECT ti.order_seq FROM timeline_items ti
	WHERE ti.session_id = s.id
	ORDER BY coalesce(ti.item_time, 0) DESC, ti.order_seq DESC, ti.updated_seq DESC
	LIMIT 1), -1)`

const sessionListOrderSQL = ` ORDER BY s.pinned DESC, coalesce(s.sort_at, s.created_at) DESC, ` +
	sessionLatestOrderSeqSQL + ` DESC, s.updated_seq DESC, s.id DESC`

// ListSessions returns one page of a user's sessions in dashboard order:
// pinned first, then the most recently active. cursor is the position returned
// by the previous page, or nil for the first page.
func (s *Store) ListSessions(
	ctx context.Context, userID string, archived bool, limit int, cursor *SessionCursor,
) ([]Session, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 200 {
		limit = 200
	}
	query := `SELECT ` + sessionColumnsFor("s") + `, ` + sessionLatestOrderSeqSQL + `
		FROM sessions s
		JOIN connectors c ON c.id = s.connector_id
		WHERE c.user_id = ? AND s.archived = ?`
	args := []any{userID, boolInt(archived)}
	if cursor != nil {
		query += ` AND (
			s.pinned < ?
			OR (s.pinned = ? AND coalesce(s.sort_at, s.created_at) < ?)
			OR (s.pinned = ? AND coalesce(s.sort_at, s.created_at) = ?
				AND ` + sessionLatestOrderSeqSQL + ` < ?)
			OR (s.pinned = ? AND coalesce(s.sort_at, s.created_at) = ?
				AND ` + sessionLatestOrderSeqSQL + ` = ? AND s.updated_seq < ?)
			OR (s.pinned = ? AND coalesce(s.sort_at, s.created_at) = ?
				AND ` + sessionLatestOrderSeqSQL + ` = ? AND s.updated_seq = ? AND s.id < ?)
		)`
		args = append(args,
			cursor.Pinned,
			cursor.Pinned, cursor.SortAt,
			cursor.Pinned, cursor.SortAt, cursor.LatestItemOrderSeq,
			cursor.Pinned, cursor.SortAt, cursor.LatestItemOrderSeq, cursor.UpdatedSeq,
			cursor.Pinned, cursor.SortAt, cursor.LatestItemOrderSeq, cursor.UpdatedSeq, cursor.ID)
	}
	query += sessionListOrderSQL + ` LIMIT ?`
	args = append(args, limit)
	return s.querySessionPage(ctx, query, args...)
}

// ListAllSessions returns the complete inventory for one archived state, with
// no page limit. GET /sessions/list and the dashboard snapshot need every row:
// splitting a person's sessions across pages there silently hides the tail.
func (s *Store) ListAllSessions(ctx context.Context, userID string, archived bool) ([]Session, error) {
	query := `SELECT ` + sessionColumnsFor("s") + `, ` + sessionLatestOrderSeqSQL + `
		FROM sessions s
		JOIN connectors c ON c.id = s.connector_id
		WHERE c.user_id = ? AND s.archived = ?` + sessionListOrderSQL
	return s.querySessionPage(ctx, query, userID, boolInt(archived))
}

// querySessionPage scans rows that end with the latest timeline item's sort key.
func (s *Store) querySessionPage(ctx context.Context, query string, args ...any) ([]Session, error) {
	rows, err := s.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		var latestOrderSeq int64
		sess, err := scanSession(rows, &latestOrderSeq)
		if err != nil {
			return nil, err
		}
		sess.LatestItemOrderSeq = latestOrderSeq
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

// AssignSessionExternalID gives a session the runtime id it will run under.
//
// A conversation created from the phone has no transcript yet and therefore no
// id. The first time it is opened on the desktop, the server mints one so
// `claude --session-id` can create that exact transcript and later submits can
// address it. The update is conditional: if another request (or the connector's
// inventory) already supplied an id, that one wins and is returned instead.
func (s *Store) AssignSessionExternalID(ctx context.Context, id, externalID string) (Session, error) {
	if externalID == "" {
		return Session{}, errors.New("empty runtime session id")
	}
	now := time.Now().UnixMilli()
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET external_session_id = ?, updated_at = ?
			WHERE id = ? AND (external_session_id IS NULL OR external_session_id = '')`,
			externalID, now, id)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s.SessionByIDAny(ctx, s.Read(), id)
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
	var id string
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		readErr := tx.QueryRowContext(ctx, `
			SELECT id FROM sessions
			WHERE connector_id = ? AND runtime = 'claude' AND external_session_id = ?`,
			connectorID, externalID).Scan(&id)
		switch {
		case readErr == nil:
			return updateDiscoveredTx(ctx, tx, id, projectID, title, cwd, updatedAt, now)
		case !errors.Is(readErr, sql.ErrNoRows):
			return readErr
		}

		id = NewID("ses")
		_, err := tx.ExecContext(ctx, `
			INSERT INTO sessions (id, connector_id, project_id, runtime, runtime_id, origin,
			                      external_session_id, title, cwd, status, source_state,
			                      sort_at, created_at, updated_at)
			VALUES (?, ?, ?, 'claude', 'claude', 'connector_import', ?, ?, ?, 'idle', 'visible', ?, ?, ?)`,
			id, connectorID, nullIfEmpty(projectID), nullIfEmpty(externalID),
			title, cwd, now, now, now)
		if err != nil {
			if !isUniqueConstraint(err, sessionIdentityColumns) {
				return err
			}
			// Another writer inserted the same runtime session first; fold this
			// scan into the row it created.
			if err := tx.QueryRowContext(ctx, `
				SELECT id FROM sessions
				WHERE connector_id = ? AND runtime = 'claude' AND external_session_id = ?`,
				connectorID, externalID).Scan(&id); err != nil {
				return err
			}
			return updateDiscoveredTx(ctx, tx, id, projectID, title, cwd, updatedAt, now)
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE sessions SET last_activity_at = ?, sort_at = max(coalesce(sort_at, 0), ?)
			WHERE id = ?`, updatedAt, updatedAt, id)
		return err
	})
	if err != nil {
		return Session{}, err
	}
	return s.SessionByIDAny(ctx, s.Read(), id)
}

// updateDiscoveredTx refreshes the mutable facts of a discovered session.
func updateDiscoveredTx(
	ctx context.Context, tx *sql.Tx, id, projectID, title, cwd string, updatedAt, now int64,
) error {
	// Only a title the person typed is frozen. An automatic title keeps
	// following the transcript, because the first scan of a fresh session
	// often has nothing better than an auto-generated slug.
	//
	// Seeing the transcript again also brings the session back: a row marked
	// missing when the file disappeared must not stay invisible forever.
	_, err := tx.ExecContext(ctx, `
		UPDATE sessions SET
			project_id = coalesce(?, project_id),
			title = CASE WHEN title_source = 'user' THEN title ELSE ? END,
			title_source = CASE WHEN title_source = 'user' THEN 'user' ELSE 'connector' END,
			cwd = coalesce(nullif(?, ''), cwd),
			source_state = 'visible',
			source_state_at = NULL,
			source_state_reason = NULL,
			sort_at = max(coalesce(sort_at, 0), ?),
			updated_at = ?
		WHERE id = ?`,
		nullIfEmpty(projectID), title, cwd, updatedAt, now, id)
	return err
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
	now := time.Now().UnixMilli()
	var ids []string
	// The read and the write share one transaction. Selecting first and
	// updating afterwards let a turn that started in between be reset to idle,
	// which stranded the running prompt.
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id FROM sessions
			WHERE connector_id = ? AND status IN ('running', 'stopping', 'error')`, connectorID)
		if err != nil {
			return err
		}
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

		reset := make([]string, 0, len(ids))
		for _, id := range ids {
			res, err := tx.ExecContext(ctx, `
				UPDATE sessions SET status = 'idle', updated_at = ?
				WHERE id = ? AND connector_id = ?
				  AND status IN ('running', 'stopping', 'error')`,
				now, id, connectorID)
			if err != nil {
				return err
			}
			// A conditional update keeps a row that changed after the read
			// (for example a fresh turn) out of the reset list.
			if n, _ := res.RowsAffected(); n > 0 {
				reset = append(reset, id)
			}
		}
		ids = reset
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}
