package store

import (
	"context"
	"database/sql"
	"errors"
)

// This file holds the read-only queries behind the administration endpoints.
// Everything here aggregates the tables the Go server already writes; there is
// no separate analytics store to fall out of date.

// ListUsers returns accounts oldest first. A non-positive limit returns every
// remaining account, which is what the admin page does when it omits the
// parameter.
func (s *Store) ListUsers(ctx context.Context, limit, offset int) ([]User, error) {
	query := `SELECT ` + userColumns + ` FROM users ORDER BY created_at ASC, id ASC`
	args := make([]any, 0, 2)
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	} else if offset > 0 {
		query += ` LIMIT -1 OFFSET ?`
		args = append(args, offset)
	}
	rows, err := s.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]User, 0)
	for rows.Next() {
		user, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, user)
	}
	return out, rows.Err()
}

// CountSessions reports every session in the instance.
func (s *Store) CountSessions(ctx context.Context) (int, error) {
	var n int
	err := s.Read().QueryRowContext(ctx, `SELECT count(*) FROM sessions`).Scan(&n)
	return n, err
}

// CountConnectors reports live (non-revoked) connectors.
func (s *Store) CountConnectors(ctx context.Context) (int, error) {
	var n int
	err := s.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM connectors WHERE revoked = 0`).Scan(&n)
	return n, err
}

// CountRunningSessions reports sessions whose runtime is currently working.
func (s *Store) CountRunningSessions(ctx context.Context) (int, error) {
	var n int
	err := s.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM sessions WHERE status = 'running'`).Scan(&n)
	return n, err
}

// CountActiveSessionsSince counts sessions that did anything since the cutoff.
func (s *Store) CountActiveSessionsSince(ctx context.Context, sinceMS int64) (int, error) {
	var n int
	err := s.Read().QueryRowContext(ctx, `
		SELECT count(*) FROM sessions
		WHERE coalesce(last_activity_at, updated_at) >= ?`, sinceMS).Scan(&n)
	return n, err
}

// CountActiveUsersSince counts distinct owners of recently active sessions.
func (s *Store) CountActiveUsersSince(ctx context.Context, sinceMS int64) (int, error) {
	var n int
	err := s.Read().QueryRowContext(ctx, `
		SELECT count(DISTINCT c.user_id)
		FROM sessions s JOIN connectors c ON c.id = s.connector_id
		WHERE coalesce(s.last_activity_at, s.updated_at) >= ?`, sinceMS).Scan(&n)
	return n, err
}

// CountUsersBefore supports the cumulative inventory line in the report.
func (s *Store) CountUsersBefore(ctx context.Context, beforeMS int64) (int, error) {
	var n int
	err := s.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE created_at < ?`, beforeMS).Scan(&n)
	return n, err
}

// CountConnectorsBefore supports the cumulative inventory line in the report.
func (s *Store) CountConnectorsBefore(ctx context.Context, beforeMS int64) (int, error) {
	var n int
	err := s.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM connectors WHERE revoked = 0 AND created_at < ?`, beforeMS).Scan(&n)
	return n, err
}

// CountConnectorsSeenSince counts connectors whose last presence update is
// inside the window.
func (s *Store) CountConnectorsSeenSince(ctx context.Context, sinceMS int64) (int, error) {
	var n int
	err := s.Read().QueryRowContext(ctx, `
		SELECT count(*) FROM connectors
		WHERE revoked = 0 AND last_seen_at IS NOT NULL AND last_seen_at >= ?`, sinceMS).Scan(&n)
	return n, err
}

// SchemaVersion is the newest applied migration name.
func (s *Store) SchemaVersion(ctx context.Context) (string, error) {
	var version string
	err := s.Read().QueryRowContext(ctx,
		`SELECT version FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return version, err
}

// EarliestSessionTime is the first piece of session activity the instance
// recorded, used as the report's "data since" marker.
func (s *Store) EarliestSessionTime(ctx context.Context) (*int64, error) {
	var value sql.NullInt64
	if err := s.Read().QueryRowContext(ctx, `SELECT min(created_at) FROM sessions`).Scan(&value); err != nil {
		return nil, err
	}
	if !value.Valid {
		return nil, nil
	}
	ms := value.Int64
	return &ms, nil
}

// SessionCreationRow is one session, bucketed by the UTC hour it was created.
type SessionCreationRow struct {
	Hour      int64
	SessionID string
	Runtime   string
	Status    string
	UserID    string
}

// SessionCreatedBetween returns sessions created inside a UTC millisecond
// window, with the owner and runtime the report groups by.
func (s *Store) SessionCreatedBetween(ctx context.Context, fromMS, toMS int64) ([]SessionCreationRow, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT (s.created_at / 3600000) * 3600000, s.id, s.runtime, s.status, c.user_id
		FROM sessions s JOIN connectors c ON c.id = s.connector_id
		WHERE s.created_at >= ? AND s.created_at < ?
		ORDER BY s.created_at ASC`, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SessionCreationRow, 0)
	for rows.Next() {
		var row SessionCreationRow
		if err := rows.Scan(&row.Hour, &row.SessionID, &row.Runtime, &row.Status, &row.UserID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ConnectorCreationRow is one connector, bucketed by its UTC creation hour.
type ConnectorCreationRow struct {
	Hour        int64
	ConnectorID string
	DeviceOS    string
}

// ConnectorCreatedBetween returns connectors created inside a UTC millisecond
// window.
func (s *Store) ConnectorCreatedBetween(ctx context.Context, fromMS, toMS int64) ([]ConnectorCreationRow, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT (created_at / 3600000) * 3600000, id, coalesce(device_os, '')
		FROM connectors
		WHERE revoked = 0 AND created_at >= ? AND created_at < ?
		ORDER BY created_at ASC`, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ConnectorCreationRow, 0)
	for rows.Next() {
		var row ConnectorCreationRow
		if err := rows.Scan(&row.Hour, &row.ConnectorID, &row.DeviceOS); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// UserCreationRow is one signup with its exact millisecond timestamp.
type UserCreationRow struct {
	UserID    string
	CreatedAt int64
}

// UsersCreatedBetween returns accounts created inside a UTC millisecond
// window.
func (s *Store) UsersCreatedBetween(ctx context.Context, fromMS, toMS int64) ([]UserCreationRow, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT id, created_at FROM users
		WHERE created_at >= ? AND created_at < ?
		ORDER BY created_at ASC`, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]UserCreationRow, 0)
	for rows.Next() {
		var row UserCreationRow
		if err := rows.Scan(&row.UserID, &row.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// MessageActivityRow is a per-hour, per-session count of timeline messages.
type MessageActivityRow struct {
	Hour      int64
	Role      string
	Status    string
	Runtime   string
	SessionID string
	UserID    string
	Count     int
}

// MessageActivity aggregates message timeline items by the UTC hour they were
// recorded. order_seq is used when the source did not provide an item time,
// which is the case for messages sent through the platform.
func (s *Store) MessageActivity(ctx context.Context, fromMS, toMS int64) ([]MessageActivityRow, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT (coalesce(t.item_time, t.order_seq) / 3600000) * 3600000 AS hour,
		       coalesce(t.role, ''), t.status, s.runtime, s.id, c.user_id, count(*)
		FROM timeline_items t
		JOIN sessions s ON s.id = t.session_id
		JOIN connectors c ON c.id = s.connector_id
		WHERE t.type = 'message'
		  AND coalesce(t.item_time, t.order_seq) >= ?
		  AND coalesce(t.item_time, t.order_seq) < ?
		GROUP BY hour, t.role, t.status, s.runtime, s.id, c.user_id`, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]MessageActivityRow, 0)
	for rows.Next() {
		var row MessageActivityRow
		if err := rows.Scan(&row.Hour, &row.Role, &row.Status, &row.Runtime,
			&row.SessionID, &row.UserID, &row.Count); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// SessionActivityRow marks one session as active in one UTC hour.
type SessionActivityRow struct {
	Hour      int64
	SessionID string
	Runtime   string
	UserID    string
}

// SessionActivity aggregates the activity clock of every session by hour.
//
// last_activity_at is the authoritative signal. Rows imported by older
// connectors may not have it, so the clock falls back through updated_at to
// created_at; otherwise those sessions would silently disappear from the
// report. The report counts these rows as "sessions active in the range"
// instead of counting sessions that merely happened to be created there.
func (s *Store) SessionActivity(ctx context.Context, fromMS, toMS int64) ([]SessionActivityRow, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT (coalesce(s.last_activity_at, s.updated_at, s.created_at) / 3600000) * 3600000,
		       s.id, s.runtime, c.user_id
		FROM sessions s JOIN connectors c ON c.id = s.connector_id
		WHERE coalesce(s.last_activity_at, s.updated_at, s.created_at) >= ?
		  AND coalesce(s.last_activity_at, s.updated_at, s.created_at) < ?
		GROUP BY 1, s.id, s.runtime, c.user_id`, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SessionActivityRow, 0)
	for rows.Next() {
		var row SessionActivityRow
		if err := rows.Scan(&row.Hour, &row.SessionID, &row.Runtime, &row.UserID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ConnectorActivityRow marks one connector as seen in one UTC hour.
type ConnectorActivityRow struct {
	Hour        int64
	ConnectorID string
	DeviceOS    string
}

// ConnectorActivity buckets connector heartbeats by hour.
func (s *Store) ConnectorActivity(ctx context.Context, fromMS, toMS int64) ([]ConnectorActivityRow, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT (last_seen_at / 3600000) * 3600000, id, coalesce(device_os, '')
		FROM connectors
		WHERE revoked = 0 AND last_seen_at IS NOT NULL
		  AND last_seen_at >= ? AND last_seen_at < ?
		GROUP BY 1, id, device_os`, fromMS, toMS)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ConnectorActivityRow, 0)
	for rows.Next() {
		var row ConnectorActivityRow
		if err := rows.Scan(&row.Hour, &row.ConnectorID, &row.DeviceOS); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// DeviceOSCount is the connector inventory for one operating system.
type DeviceOSCount struct {
	OS    string
	Count int
}

// ConnectorCountsByOS returns the current connector inventory grouped by OS.
func (s *Store) ConnectorCountsByOS(ctx context.Context) ([]DeviceOSCount, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT coalesce(nullif(device_os, ''), 'unknown'), count(*)
		FROM connectors WHERE revoked = 0
		GROUP BY 1 ORDER BY 2 DESC, 1 ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DeviceOSCount, 0)
	for rows.Next() {
		var row DeviceOSCount
		if err := rows.Scan(&row.OS, &row.Count); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// Administrative user edits can fail for reasons the caller must translate
// into a specific 409 instead of a generic 500.
var (
	ErrLastAdmin      = errors.New("cannot remove the last administrator")
	ErrSelfAdminEdit  = errors.New("cannot change your own role or disable your own account")
	ErrSelfDeleteUser = errors.New("cannot delete your own account")
)

// UpdateUserAsAdmin applies the editable account fields.
//
// The checks and the write share one transaction so two administrators cannot
// demote themselves at the same time and leave the instance without a usable
// administrator.
func (s *Store) UpdateUserAsAdmin(
	ctx context.Context, actorID, targetID string,
	displayName, role *string, disabled *bool,
) (User, error) {
	var updated User
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		target, err := scanUser(tx.QueryRowContext(ctx,
			`SELECT `+userColumns+` FROM users WHERE id = ?`, targetID))
		if err != nil {
			return err
		}

		nextRole := target.Role
		if role != nil {
			nextRole = *role
		}
		nextDisabled := target.Disabled
		if disabled != nil {
			nextDisabled = *disabled
		}

		// An administrator may edit their own profile, but not surrender the
		// only account that can still administer the instance.
		if targetID == actorID {
			if nextRole != target.Role {
				return ErrSelfAdminEdit
			}
			if nextDisabled && !target.Disabled {
				return ErrSelfAdminEdit
			}
		}
		if target.Role == "admin" && !target.Disabled && (nextRole != "admin" || nextDisabled) {
			admins, err := countEnabledAdmins(ctx, tx)
			if err != nil {
				return err
			}
			if admins <= 1 {
				return ErrLastAdmin
			}
		}

		sets := make([]string, 0, 4)
		args := make([]any, 0, 5)
		if displayName != nil {
			sets = append(sets, "display_name = ?")
			args = append(args, *displayName)
		}
		if role != nil {
			sets = append(sets, "role = ?")
			args = append(args, *role)
		}
		if disabled != nil {
			sets = append(sets, "disabled = ?")
			args = append(args, boolInt(*disabled))
		}
		sets = append(sets, "updated_at = ?")
		args = append(args, NowMS())
		args = append(args, targetID)
		if _, err := tx.ExecContext(ctx,
			`UPDATE users SET `+joinStrings(sets, ", ")+` WHERE id = ?`, args...); err != nil {
			return err
		}

		updated, err = scanUser(tx.QueryRowContext(ctx,
			`SELECT `+userColumns+` FROM users WHERE id = ?`, targetID))
		return err
	})
	if err != nil {
		return User{}, err
	}
	return updated, nil
}

// DeleteUserAsAdmin removes an account and everything that belongs to it.
//
// The order follows the foreign keys: sessions reference both connectors and
// projects, so they go first; projects then release their connector reference;
// connectors go last before the user because users are referenced with
// ON DELETE RESTRICT.
func (s *Store) DeleteUserAsAdmin(ctx context.Context, actorID, targetID string) ([]AttachmentFileRef, error) {
	var files []AttachmentFileRef
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		target, err := scanUser(tx.QueryRowContext(ctx,
			`SELECT `+userColumns+` FROM users WHERE id = ?`, targetID))
		if err != nil {
			return err
		}

		// Check the last usable administrator before anything else. Whether the
		// target is the caller must not hide the more important lockout reason.
		if target.Role == "admin" && !target.Disabled {
			admins, err := countEnabledAdmins(ctx, tx)
			if err != nil {
				return err
			}
			if admins <= 1 {
				return ErrLastAdmin
			}
		}
		if targetID == actorID {
			return ErrSelfDeleteUser
		}

		// The attachment rows cascade with the sessions, so collect the file
		// locations while they still exist; the caller removes the blobs after
		// this transaction commits.
		files, err = s.AttachmentFilesForUser(ctx, tx, targetID)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			DELETE FROM sessions
			WHERE connector_id IN (SELECT id FROM connectors WHERE user_id = ?)`, targetID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM projects WHERE user_id = ?`, targetID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM connectors WHERE user_id = ?`, targetID); err != nil {
			return err
		}
		// Verification codes intentionally have no FK to users; leaving them
		// behind would make a later signup with the same address fail on stale
		// state.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM email_verification_codes WHERE user_id = ?`, targetID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, targetID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func countEnabledAdmins(ctx context.Context, tx *sql.Tx) (int, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n)
	return n, err
}

// joinStrings keeps the update statement readable without pulling strings into
// this file's imports for one call.
func joinStrings(parts []string, sep string) string {
	out := ""
	for index, part := range parts {
		if index > 0 {
			out += sep
		}
		out += part
	}
	return out
}
