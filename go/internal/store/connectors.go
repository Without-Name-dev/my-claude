package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Connector is one paired desktop agent.
type Connector struct {
	ID          string
	UserID      string
	Name        string
	Kind        string
	DeviceOS    string
	Status      string
	LastSeenAt  *int64
	TokenHash   string
	TokenPrefix string
	// Revoked is 0 while usable, 1 when the person revoked it, 2 while being
	// deleted.
	Revoked   int
	CreatedAt int64
	UpdatedAt int64
}

const connectorColumns = `id, user_id, name, connector_kind, coalesce(device_os, ''),
	status, last_seen_at, token_hash, token_prefix, revoked, created_at, updated_at`

func scanConnector(row interface{ Scan(...any) error }) (Connector, error) {
	var c Connector
	err := row.Scan(&c.ID, &c.UserID, &c.Name, &c.Kind, &c.DeviceOS, &c.Status,
		&c.LastSeenAt, &c.TokenHash, &c.TokenPrefix, &c.Revoked, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Connector{}, ErrNotFound
		}
		return Connector{}, err
	}
	return c, nil
}

// CreateConnector inserts a pairing result.
func (s *Store) CreateConnector(ctx context.Context, c Connector) (Connector, error) {
	now := time.Now().UnixMilli()
	if c.ID == "" {
		c.ID = NewID("con")
	}
	if c.Kind == "" {
		c.Kind = "cli"
	}
	c.CreatedAt, c.UpdatedAt = now, now
	var os any
	if c.DeviceOS != "" {
		os = c.DeviceOS
	}
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO connectors (id, user_id, name, connector_kind, device_os, status,
			                        token_hash, token_prefix, revoked, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, 'offline', ?, ?, 0, ?, ?)`,
			c.ID, c.UserID, c.Name, c.Kind, os, c.TokenHash, c.TokenPrefix, now, now)
		return err
	})
	if err != nil {
		return Connector{}, err
	}
	c.Status = "offline"
	return c, nil
}

// ConnectorByID returns one connector.
func (s *Store) ConnectorByID(ctx context.Context, id string) (Connector, error) {
	row := s.Read().QueryRowContext(ctx, `SELECT `+connectorColumns+` FROM connectors WHERE id = ?`, id)
	return scanConnector(row)
}

// ConnectorByTokenHash resolves a presented bearer to its connector.
//
// A disabled account keeps its rows but must not be able to run anything on
// the instance, so the owner's state is part of the authentication check.
func (s *Store) ConnectorByTokenHash(ctx context.Context, hash string) (Connector, error) {
	row := s.Read().QueryRowContext(ctx, `
		SELECT `+connectorColumns+` FROM connectors
		WHERE token_hash = ? AND revoked = 0
		  AND user_id IN (SELECT id FROM users WHERE disabled = 0)`, hash)
	return scanConnector(row)
}

// ListConnectors returns a user's live connectors, newest first.
func (s *Store) ListConnectors(ctx context.Context, userID string) ([]Connector, error) {
	rows, err := s.Read().QueryContext(ctx, `
		SELECT `+connectorColumns+` FROM connectors
		WHERE user_id = ? AND revoked = 0 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Connector
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetConnectorPresence records that a connector socket opened or closed.
//
// A closed socket only marks the connector offline when it is still the same
// connection: a reconnect that lands before the old socket's cleanup must not
// be overwritten by the dying one. Losing that connection also ends its turns
// in the database, because nothing can still be running on the machine the
// server just stopped hearing from.
func (s *Store) SetConnectorPresence(ctx context.Context, id, connectionID string, online bool) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if online {
			_, err := tx.ExecContext(ctx, `
				UPDATE connectors
				SET status = 'online', presence_connection_id = ?, last_seen_at = ?, updated_at = ?
				WHERE id = ?`, connectionID, now, now, id)
			return err
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE connectors
			SET status = 'offline', presence_connection_id = NULL, last_seen_at = ?, updated_at = ?
			WHERE id = ? AND presence_connection_id = ?`, now, now, id, connectionID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// A newer socket already owns the connector; its turns are live.
			return nil
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE sessions SET status = 'idle', updated_at = ?
			WHERE connector_id = ? AND status IN ('running', 'stopping', 'error', 'waiting_approval')`, now, id)
		return err
	})
}

// TouchConnector refreshes last_seen for a heartbeat.
func (s *Store) TouchConnector(ctx context.Context, id string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE connectors SET last_seen_at = ? WHERE id = ?`, now, id)
		return err
	})
}
