package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// GatewayCredential is a revocable token that identifies a desktop's Claude
// processes to the model gateway. It is separate from the connector's main
// bearer on purpose: the gateway token is handed to interactive terminal
// processes, so it must be narrow and revocable without disconnecting the
// desktop.
type GatewayCredential struct {
	ID          string
	ConnectorID string
	UserID      string
	TokenHash   string
	TokenPrefix string
	Status      string // active | revoked
	CreatedAt   int64
	LastUsedAt  *int64
	RevokedAt   *int64
}

// IssueGatewayCredential mints a new credential for a connector. The plaintext
// token is returned exactly once; only its hash is stored. Any previous active
// credential for the connector is revoked first, so a device holds at most one
// live gateway token and reissuing rotates it.
func (s *Store) IssueGatewayCredential(ctx context.Context, connectorID, userID string, mint func() (token, hash, prefix string, err error)) (GatewayCredential, string, error) {
	var out GatewayCredential
	var plaintext string
	now := time.Now().UnixMilli()
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// Ownership: the connector must belong to the caller.
		var owner string
		if err := tx.QueryRowContext(ctx,
			`SELECT user_id FROM connectors WHERE id = ?`, connectorID).Scan(&owner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if owner != userID {
			return ErrNotFound
		}
		token, hash, prefix, err := mint()
		if err != nil {
			return err
		}
		id := NewID("gwc")
		if _, err := tx.ExecContext(ctx, `
			UPDATE gateway_credentials SET status = 'revoked', revoked_at = ?
			WHERE connector_id = ? AND status = 'active'`, now, connectorID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO gateway_credentials (id, connector_id, user_id, token_hash, token_prefix,
			                                 status, created_at)
			VALUES (?, ?, ?, ?, ?, 'active', ?)`,
			id, connectorID, userID, hash, prefix, now); err != nil {
			return err
		}
		out = GatewayCredential{
			ID: id, ConnectorID: connectorID, UserID: userID,
			TokenHash: hash, TokenPrefix: prefix, Status: "active", CreatedAt: now,
		}
		plaintext = token
		// Rotations accumulate revoked rows. Keep the newest few per connector
		// for auditing and drop the rest, so repeated re-enabling cannot grow
		// the table without bound.
		_, err = tx.ExecContext(ctx, `
			DELETE FROM gateway_credentials
			WHERE connector_id = ? AND status = 'revoked'
			  AND id NOT IN (
			    SELECT id FROM gateway_credentials
			    WHERE connector_id = ? ORDER BY rowid DESC LIMIT 10)`,
			connectorID, connectorID)
		return err
	})
	if err != nil {
		return GatewayCredential{}, "", err
	}
	return out, plaintext, nil
}

// GatewayCredentialByTokenHash resolves a presented gateway token. A revoked
// credential, a deleted connector, or a disabled account all fail the lookup —
// the gateway cannot distinguish them from an unknown token, by design.
func (s *Store) GatewayCredentialByTokenHash(ctx context.Context, hash string) (GatewayCredential, error) {
	var c GatewayCredential
	var lastUsed, revoked sql.NullInt64
	err := s.Read().QueryRowContext(ctx, `
		SELECT g.id, g.connector_id, g.user_id, g.token_hash, g.token_prefix, g.status,
		       g.created_at, g.last_used_at, g.revoked_at
		FROM gateway_credentials g
		JOIN connectors c ON c.id = g.connector_id
		WHERE g.token_hash = ? AND g.status = 'active'
		  AND c.revoked = 0
		  AND g.user_id IN (SELECT id FROM users WHERE disabled = 0)`, hash).
		Scan(&c.ID, &c.ConnectorID, &c.UserID, &c.TokenHash, &c.TokenPrefix, &c.Status,
			&c.CreatedAt, &lastUsed, &revoked)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GatewayCredential{}, ErrNotFound
		}
		return GatewayCredential{}, err
	}
	if lastUsed.Valid {
		v := lastUsed.Int64
		c.LastUsedAt = &v
	}
	if revoked.Valid {
		v := revoked.Int64
		c.RevokedAt = &v
	}
	return c, nil
}

// TouchGatewayCredential records use. Best effort: a failed touch must not
// fail a model request, so errors are returned but ignored by the caller.
func (s *Store) TouchGatewayCredential(ctx context.Context, id string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE gateway_credentials SET last_used_at = ? WHERE id = ?`, now, id)
		return err
	})
}

// RevokeGatewayCredential retires one credential.
func (s *Store) RevokeGatewayCredential(ctx context.Context, id, userID string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE gateway_credentials SET status = 'revoked', revoked_at = ?
			WHERE id = ? AND user_id = ? AND status = 'active'`, now, id, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// GatewayCredentialStatus describes a connector's gateway access for the
// device page: whether a credential exists and when it was last used.
type GatewayCredentialStatus struct {
	CredentialID string
	Status       string
	Prefix       string
	CreatedAt    int64
	LastUsedAt   *int64
}

// LatestGatewayCredentialFor reports the newest credential row for a
// connector, active or revoked, so the UI can show "issued, then revoked".
func (s *Store) LatestGatewayCredentialFor(ctx context.Context, connectorID, userID string) (GatewayCredentialStatus, error) {
	var out GatewayCredentialStatus
	var lastUsed sql.NullInt64
	err := s.Read().QueryRowContext(ctx, `
		SELECT id, status, coalesce(token_prefix, ''), created_at, last_used_at
		FROM gateway_credentials WHERE connector_id = ? AND user_id = ?
		ORDER BY created_at DESC, rowid DESC LIMIT 1`, connectorID, userID).
		Scan(&out.CredentialID, &out.Status, &out.Prefix, &out.CreatedAt, &lastUsed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GatewayCredentialStatus{}, ErrNotFound
		}
		return GatewayCredentialStatus{}, err
	}
	if lastUsed.Valid {
		v := lastUsed.Int64
		out.LastUsedAt = &v
	}
	return out, nil
}

// GatewayRequestSummary is the minimal per-request record the gateway writes
// once a request finishes. No bodies, no credentials, no prompts.
type GatewayRequestSummary struct {
	ID                string
	UserID            string
	ConnectorID       string
	SessionID         *string
	ExternalSessionID *string
	ProviderID        *string
	ModelID           *string
	ProviderRevision  *int64
	StartedAt         int64
	FinishedAt        *int64
	Status            string // streaming | completed | failed | cancelled
	UpstreamStatus    *int
	BytesIn           int64
	BytesOut          int64
	UpstreamRequestID *string
	Error             *string
}

// RecordGatewayRequest writes one completed request summary in a single
// insert. The gateway calls it after the response has left, so a failure to
// record never interrupts a live stream.
func (s *Store) RecordGatewayRequest(ctx context.Context, in GatewayRequestSummary) error {
	var sessionID, externalID, providerID, modelID, upstreamRequestID, failure any
	if in.SessionID != nil {
		sessionID = *in.SessionID
	}
	if in.ExternalSessionID != nil {
		externalID = *in.ExternalSessionID
	}
	if in.ProviderID != nil {
		providerID = *in.ProviderID
	}
	if in.ModelID != nil {
		modelID = *in.ModelID
	}
	if in.UpstreamRequestID != nil {
		upstreamRequestID = *in.UpstreamRequestID
	}
	if in.Error != nil {
		failure = *in.Error
	}
	status := in.Status
	if status == "" {
		status = "completed"
	}
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO gateway_request_log
				(id, user_id, connector_id, session_id, external_session_id, provider_id,
				 model_id, provider_revision, started_at, finished_at, status,
				 upstream_status, bytes_in, bytes_out, upstream_request_id, error)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.ID, in.UserID, in.ConnectorID, sessionID, externalID, providerID,
			modelID, in.ProviderRevision, in.StartedAt, in.FinishedAt, status,
			in.UpstreamStatus, in.BytesIn, in.BytesOut, upstreamRequestID, failure)
		return err
	})
}

// LastGatewayRequest reports the most recent request summary for a session,
// which the route view exposes as lastRequest.
func (s *Store) LastGatewayRequest(ctx context.Context, sessionID string) (GatewayRequestSummary, error) {
	var out GatewayRequestSummary
	var session, external, provider, model, upstreamRequestID, failure sql.NullString
	var revision sql.NullInt64
	var finished sql.NullInt64
	var upstream sql.NullInt64
	err := s.Read().QueryRowContext(ctx, `
		SELECT id, user_id, connector_id, session_id, external_session_id, provider_id,
		       model_id, provider_revision, started_at, finished_at, status,
		       upstream_status, bytes_in, bytes_out, upstream_request_id, error
		FROM gateway_request_log WHERE session_id = ?
		ORDER BY started_at DESC LIMIT 1`, sessionID).
		Scan(&out.ID, &out.UserID, &out.ConnectorID, &session, &external, &provider,
			&model, &revision, &out.StartedAt, &finished, &out.Status,
			&upstream, &out.BytesIn, &out.BytesOut, &upstreamRequestID, &failure)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GatewayRequestSummary{}, ErrNotFound
		}
		return GatewayRequestSummary{}, err
	}
	nullableToPtr(session, &out.SessionID)
	nullableToPtr(external, &out.ExternalSessionID)
	nullableToPtr(provider, &out.ProviderID)
	nullableToPtr(model, &out.ModelID)
	nullableToPtr(upstreamRequestID, &out.UpstreamRequestID)
	nullableToPtr(failure, &out.Error)
	nullIntToPtr(revision, &out.ProviderRevision)
	nullIntToPtr(finished, &out.FinishedAt)
	if upstream.Valid {
		v := int(upstream.Int64)
		out.UpstreamStatus = &v
	}
	return out, nil
}

// TrimGatewayRequestLog keeps the summary table bounded. Called periodically;
// a failure is logged, never fatal.
func (s *Store) TrimGatewayRequestLog(ctx context.Context, keep int) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			DELETE FROM gateway_request_log WHERE id NOT IN (
				SELECT id FROM gateway_request_log ORDER BY started_at DESC LIMIT ?)`, keep)
		return err
	})
}
