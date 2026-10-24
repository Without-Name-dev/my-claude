package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"time"
)

// setting reads one instance-level value.
func (a *App) setting(ctx context.Context, key string) (string, error) {
	var value string
	err := a.db.Read().QueryRowContext(ctx,
		`SELECT value FROM instance_settings WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

// setSetting writes one instance-level value.
func (a *App) setSetting(ctx context.Context, key, value string) error {
	return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO instance_settings (key, value, updated_at) VALUES (?, ?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
			key, value, time.Now().UnixMilli())
		return err
	})
}

// clearSetting removes one instance-level value.
func (a *App) clearSetting(ctx context.Context, key string) error {
	return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM instance_settings WHERE key = ?`, key)
		return err
	})
}

// envSetupToken lets a deployment pin the bootstrap token instead of reading
// it from the log.
func envSetupToken() string { return os.Getenv("AA_SETUP_TOKEN") }

// itoa is shorthand for the integer-to-string conversions settings need.
func itoa(value int64) string { return strconv.FormatInt(value, 10) }

// atoi parses a stored integer setting.
func atoi(value string) (int64, bool) {
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil
}

// jsonUnmarshal is a thin alias so handlers read cleanly.
func jsonUnmarshal(raw string, dst any) error { return json.Unmarshal([]byte(raw), dst) }
