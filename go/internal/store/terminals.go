package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Terminal is one desktop PTY as the server remembers it.
//
// The process handle, output buffers and sequence numbers live on the
// connector; this row is the durable half: who owns the terminal, which
// connector hosts it, its size and its last known state.
type Terminal struct {
	ID            string
	ConnectorID   string
	UserID        string
	SessionID     string
	Label         string
	Root          string
	CWD           string
	Shell         string
	Cols          int
	Rows          int
	Status        string // "starting" | "running" | "exited"
	ExitCode      *int
	PID           int
	ScrollbackSeq int64
	CreatedAt     int64
	UpdatedAt     int64
}

const terminalColumns = `id, connector_id, user_id, session_id, label, root, cwd, shell,
	cols, rows, status, exit_code, pid, scrollback_seq, created_at, updated_at`

func scanTerminal(row interface{ Scan(...any) error }) (Terminal, error) {
	var t Terminal
	err := row.Scan(&t.ID, &t.ConnectorID, &t.UserID, &t.SessionID, &t.Label,
		&t.Root, &t.CWD, &t.Shell, &t.Cols, &t.Rows, &t.Status, &t.ExitCode,
		&t.PID, &t.ScrollbackSeq, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Terminal{}, ErrNotFound
		}
		return Terminal{}, err
	}
	return t, nil
}

// CreateTerminal inserts the record before the connector is asked to start the
// PTY, so a concurrent stream request always has something to authorize.
func (s *Store) CreateTerminal(ctx context.Context, t Terminal) (Terminal, error) {
	now := time.Now().UnixMilli()
	if t.ID == "" {
		t.ID = NewID("trm")
	}
	if t.Cols <= 0 {
		t.Cols = 80
	}
	if t.Rows <= 0 {
		t.Rows = 24
	}
	if t.Status == "" {
		t.Status = "starting"
	}
	t.CreatedAt, t.UpdatedAt = now, now
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO connector_terminals (id, connector_id, user_id, session_id,
				label, root, cwd, shell, cols, rows, status, exit_code, pid,
				scrollback_seq, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.ID, t.ConnectorID, t.UserID, t.SessionID, t.Label, t.Root,
			t.CWD, t.Shell, t.Cols, t.Rows, t.Status, t.ExitCode, t.PID,
			t.ScrollbackSeq, t.CreatedAt, t.UpdatedAt)
		return err
	})
	if err != nil {
		return Terminal{}, err
	}
	return t, nil
}

// TerminalByID returns one terminal, regardless of owner. Callers must check
// UserID themselves so a wrong owner can be answered with 404 rather than a
// distinct "forbidden" that leaks existence.
func (s *Store) TerminalByID(ctx context.Context, id string) (Terminal, error) {
	row := s.Read().QueryRowContext(ctx,
		`SELECT `+terminalColumns+` FROM connector_terminals WHERE id = ?`, id)
	return scanTerminal(row)
}

// ListTerminals returns a connector's terminals newest first. An empty userID
// means every owner, which the connector link uses when it rebuilds its
// in-memory ownership map after a server restart.
func (s *Store) ListTerminals(ctx context.Context, connectorID, userID string) ([]Terminal, error) {
	query := `SELECT ` + terminalColumns + ` FROM connector_terminals WHERE connector_id = ?`
	args := []any{connectorID}
	if userID != "" {
		query += ` AND user_id = ?`
		args = append(args, userID)
	}
	query += ` ORDER BY created_at DESC`

	rows, err := s.Read().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Terminal
	for rows.Next() {
		t, err := scanTerminal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// MarkTerminalRunning records the PID reported by the connector.
func (s *Store) MarkTerminalRunning(ctx context.Context, id string, pid int) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE connector_terminals
			SET status = 'running', pid = ?, updated_at = ?
			WHERE id = ?`, pid, time.Now().UnixMilli(), id)
		return err
	})
}

// MarkTerminalExited stores the final exit code. A nil code means the process
// was killed before it could report one.
func (s *Store) MarkTerminalExited(ctx context.Context, id string, exitCode *int) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE connector_terminals
			SET status = 'exited', exit_code = ?, updated_at = ?
			WHERE id = ?`, exitCode, time.Now().UnixMilli(), id)
		return err
	})
}

// RenameTerminal changes the label shown in the dashboard.
func (s *Store) RenameTerminal(ctx context.Context, id, userID, label string) (Terminal, error) {
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE connector_terminals SET label = ?, updated_at = ?
			WHERE id = ? AND user_id = ?`,
			label, time.Now().UnixMilli(), id, userID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
	if err != nil {
		return Terminal{}, err
	}
	return s.TerminalByID(ctx, id)
}

// UpdateTerminalSize records a resize so a listing after a reconnect reports
// dimensions the PTY actually has.
func (s *Store) UpdateTerminalSize(ctx context.Context, id string, cols, rows int) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE connector_terminals SET cols = ?, rows = ?, updated_at = ?
			WHERE id = ?`, cols, rows, time.Now().UnixMilli(), id)
		return err
	})
}

// UpdateTerminalScrollback stores the highest sequence the server applied. It
// is advisory: the replay buffer itself is in memory.
func (s *Store) UpdateTerminalScrollback(ctx context.Context, id string, seq uint64) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE connector_terminals SET scrollback_seq = ?, updated_at = ?
			WHERE id = ?`, int64(seq), time.Now().UnixMilli(), id)
		return err
	})
}

// DeleteTerminal removes the metadata row. The PTY is closed by the caller
// before this is called.
func (s *Store) DeleteTerminal(ctx context.Context, id, userID string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			DELETE FROM connector_terminals WHERE id = ? AND user_id = ?`, id, userID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return ErrNotFound
		}
		return nil
	})
}
