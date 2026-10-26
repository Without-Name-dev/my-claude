package store

import (
	"context"
	"database/sql"
	"errors"
)

func MarkHistoryImportDirty(ctx context.Context, tx *sql.Tx, sessionID string, now int64) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO history_import_state (session_id, dirty, generation, updated_at)
		VALUES (?, 1, 1, ?)
		ON CONFLICT(session_id) DO UPDATE SET dirty = 1,
			generation = history_import_state.generation + 1,
			updated_at = excluded.updated_at`,
		sessionID, now)
	return err
}

func (s *Store) HistoryImportState(ctx context.Context, q Querier, sessionID string) (int64, bool, error) {
	var generation int64
	var dirty int
	err := q.QueryRowContext(ctx,
		`SELECT generation, dirty FROM history_import_state WHERE session_id = ?`, sessionID,
	).Scan(&generation, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return generation, dirty != 0, nil
}

func (s *Store) ClearHistoryImportDirty(ctx context.Context, sessionID string, generation int64) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM history_import_state WHERE session_id = ? AND generation = ?`, sessionID, generation,
		)
		return err
	})
}
