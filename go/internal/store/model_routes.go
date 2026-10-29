package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ConnectorModelDefault is the default provider for one desktop. Sessions on
// that desktop inherit it unless they carry an explicit route.
type ConnectorModelDefault struct {
	ConnectorID string
	UserID      string
	ProviderID  string
	Revision    int64
	UpdatedAt   int64
}

// SessionModelRoute is the routing intent for one server session, plus the
// last state the gateway actually confirmed.
type SessionModelRoute struct {
	SessionID         string
	UserID            string
	Mode              string // inherit_device_default | explicit
	ProviderID        *string
	TargetRevision    *int64
	AppliedProviderID *string
	AppliedRevision   *int64
	AppliedModelID    *string
	Status            string
	StatusDetail      *string
	UpdatedAt         int64
}

// RouteSwitchOperation is one asynchronous switch request, queryable after a
// disconnect so the UI never has to guess whether it landed.
type RouteSwitchOperation struct {
	ID             string
	UserID         string
	Scope          string // session | connector
	SessionID      *string
	ConnectorID    *string
	ProviderID     *string
	TargetRevision *int64
	IdempotencyKey *string
	Status         string // pending | waiting_for_turn_end | applied | failed | cancelled
	Failure        *string
	CreatedAt      int64
	UpdatedAt      int64
	CompletedAt    *int64
}

// ErrNoDeviceDefault is the honest answer when a desktop has no default
// provider: the gateway refuses to guess one for an unidentified session.
var ErrNoDeviceDefault = errors.New("no default model provider for this device")

const sessionModelRouteColumns = `session_id, user_id, mode, target_provider_id, target_revision,
	applied_provider_id, applied_revision, applied_model_id, status, status_detail, updated_at`

func scanSessionModelRoute(row interface{ Scan(...any) error }) (SessionModelRoute, error) {
	var r SessionModelRoute
	var detail sql.NullString
	err := row.Scan(&r.SessionID, &r.UserID, &r.Mode, &r.ProviderID, &r.TargetRevision,
		&r.AppliedProviderID, &r.AppliedRevision, &r.AppliedModelID,
		&r.Status, &detail, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return SessionModelRoute{}, ErrNotFound
		}
		return SessionModelRoute{}, err
	}
	if detail.Valid {
		r.StatusDetail = &detail.String
	}
	return r, nil
}

// ConnectorModelDefaultFor reads the desktop's default provider.
func (s *Store) ConnectorModelDefaultFor(ctx context.Context, connectorID string) (ConnectorModelDefault, error) {
	var d ConnectorModelDefault
	err := s.Read().QueryRowContext(ctx, `
		SELECT connector_id, user_id, provider_id, revision, updated_at
		FROM connector_model_defaults WHERE connector_id = ?`, connectorID).
		Scan(&d.ConnectorID, &d.UserID, &d.ProviderID, &d.Revision, &d.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ConnectorModelDefault{}, ErrNotFound
		}
		return ConnectorModelDefault{}, err
	}
	return d, nil
}

// SetConnectorModelDefault stores the desktop's default provider, verifying in
// the same transaction that both the connector and the provider belong to the
// caller. Applying it to existing sessions is a separate, explicit choice.
func (s *Store) SetConnectorModelDefault(ctx context.Context, connectorID, userID, providerID string) (ConnectorModelDefault, error) {
	now := time.Now().UnixMilli()
	var out ConnectorModelDefault
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
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
		var providerOwner string
		var enabled int
		var credential string
		if err := tx.QueryRowContext(ctx,
			`SELECT user_id, enabled, credential_plaintext FROM model_providers WHERE id = ?`,
			providerID).Scan(&providerOwner, &enabled, &credential); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if providerOwner != userID {
			return ErrNotFound
		}
		// A default that cannot serve a request is refused up front, rather
		// than accepted and then failing every turn.
		if enabled == 0 || credential == "" {
			return ErrProviderUnusable
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO connector_model_defaults (connector_id, user_id, provider_id, revision, updated_at)
			VALUES (?, ?, ?, 1, ?)
			ON CONFLICT (connector_id) DO UPDATE SET
				provider_id = excluded.provider_id,
				user_id = excluded.user_id,
				revision = connector_model_defaults.revision + 1,
				updated_at = excluded.updated_at`,
			connectorID, userID, providerID, now)
		if err != nil {
			return err
		}
		out = ConnectorModelDefault{
			ConnectorID: connectorID, UserID: userID, ProviderID: providerID,
			Revision: 1, UpdatedAt: now,
		}
		return nil
	})
	if err != nil {
		return ConnectorModelDefault{}, err
	}
	if row, err := s.ConnectorModelDefaultFor(ctx, connectorID); err == nil {
		out = row
	}
	return out, nil
}

// ClearConnectorModelDefault removes the desktop default. Clearing an absent
// default succeeds: a second device clearing at the same time should not turn
// into an error for either.
func (s *Store) ClearConnectorModelDefault(ctx context.Context, connectorID, userID string) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`DELETE FROM connector_model_defaults WHERE connector_id = ? AND user_id = ?`,
			connectorID, userID)
		return err
	})
}

// SessionModelRouteFor reads one session's route row.
func (s *Store) SessionModelRouteFor(ctx context.Context, sessionID string) (SessionModelRoute, error) {
	row := s.Read().QueryRowContext(ctx,
		`SELECT `+sessionModelRouteColumns+` FROM session_model_routes WHERE session_id = ?`, sessionID)
	return scanSessionModelRoute(row)
}

// SetSessionModelRoute records a session-level choice. mode 'explicit' pins
// the session to providerID; 'inherit_device_default' follows the desktop.
//
// Status semantics: 'applied' only ever means "the gateway will use this
// target from the next turn on" — it never claims an upstream request
// succeeded. A session with a turn in flight waits for that turn to end.
func (s *Store) SetSessionModelRoute(
	ctx context.Context, sessionID, userID, mode string, providerID string, turnInFlight bool,
) (SessionModelRoute, error) {
	now := time.Now().UnixMilli()
	var out SessionModelRoute
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// A session's owner is its connector's owner.
		var sessionOwner string
		if err := tx.QueryRowContext(ctx, `
			SELECT c.user_id FROM sessions s JOIN connectors c ON c.id = s.connector_id
			WHERE s.id = ?`, sessionID).Scan(&sessionOwner); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if sessionOwner != userID {
			return ErrNotFound
		}
		var target any
		var targetRevision any
		status := "applied"
		if turnInFlight {
			// A switch never interrupts a turn, whichever mode it selects.
			status = "waiting_for_turn_end"
		}
		if mode == "explicit" {
			var providerOwner string
			var revision int64
			var enabled int
			var credential string
			if err := tx.QueryRowContext(ctx,
				`SELECT user_id, revision, enabled, credential_plaintext FROM model_providers WHERE id = ?`,
				providerID).Scan(&providerOwner, &revision, &enabled, &credential); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return ErrNotFound
				}
				return err
			}
			if providerOwner != userID {
				return ErrNotFound
			}
			if enabled == 0 || credential == "" {
				return ErrProviderUnusable
			}
			target = providerID
			targetRevision = revision
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO session_model_routes
				(session_id, user_id, mode, target_provider_id, target_revision, status, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (session_id) DO UPDATE SET
				mode = excluded.mode,
				target_provider_id = excluded.target_provider_id,
				target_revision = excluded.target_revision,
				status = excluded.status,
				status_detail = NULL,
				updated_at = excluded.updated_at`,
			sessionID, userID, mode, target, targetRevision, status, now)
		if err != nil {
			return err
		}
		// Keep the legacy model_selection_id column in lockstep so clients
		// reading runtime.state.selections see the same choice.
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET model_selection_id = ? WHERE id = ?`, target, sessionID); err != nil {
			return err
		}
		// A newer switch replaces an older queued one: the older operation can
		// never become the active route, so it must not stay "waiting" forever.
		if _, err := tx.ExecContext(ctx, `
			UPDATE route_switch_operations
			SET status = 'cancelled', failure = 'superseded by a newer switch',
			    updated_at = ?, completed_at = ?
			WHERE session_id = ? AND status = 'waiting_for_turn_end'`,
			now, now, sessionID); err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx,
			`SELECT `+sessionModelRouteColumns+` FROM session_model_routes WHERE session_id = ?`, sessionID)
		out, err = scanSessionModelRoute(row)
		return err
	})
	if err != nil {
		return SessionModelRoute{}, err
	}
	return out, nil
}

// RouteDecision is the resolved, immutable snapshot a request (or a whole
// turn) is pinned to.
type RouteDecision struct {
	Provider ModelProvider
	Mode     string // explicit | inherit_device_default
}

// ResolveConnectorDefault resolves the device default with the same
// honesty rules as session routing. Used for gateway requests whose session
// header is missing or not yet registered.
func (s *Store) ResolveConnectorDefault(ctx context.Context, connectorID string) (RouteDecision, error) {
	def, err := s.ConnectorModelDefaultFor(ctx, connectorID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return RouteDecision{}, ErrNoDeviceDefault
		}
		return RouteDecision{}, err
	}
	provider, err := s.ModelProviderOwned(ctx, def.ProviderID, def.UserID)
	if err != nil {
		return RouteDecision{}, ErrNoDeviceDefault
	}
	if !provider.Enabled {
		return RouteDecision{}, errors.New("the device's default provider is disabled; enable it or choose another one")
	}
	if provider.Credential == "" {
		return RouteDecision{}, errors.New("the device's default provider has no credential configured")
	}
	return RouteDecision{Provider: provider, Mode: "inherit_device_default"}, nil
}

// RebindTarget maps a retired external session id (a /clear predecessor)
// back to its server session, so a gateway request that raced the rebind
// still lands on the right conversation.
func (s *Store) RebindTarget(ctx context.Context, connectorID, fromExternalID string) (string, error) {
	var sessionID string
	err := s.Read().QueryRowContext(ctx, `
		SELECT session_id FROM session_rebinds
		WHERE connector_id = ? AND from_external_id = ?`,
		connectorID, fromExternalID).Scan(&sessionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrNotFound
		}
		return "", err
	}
	return sessionID, nil
}

// ResolveSessionRoute returns the provider a session's requests must use:
// the explicit route when present, otherwise the connector default. A
// provider that is disabled, credential-less, or deleted resolves to a
// precise error rather than a silent fallback to another provider.
//
// userID is the account the gateway credential belongs to; the session must
// live on one of that account's connectors.
func (s *Store) ResolveSessionRoute(ctx context.Context, session Session, userID string) (RouteDecision, error) {
	route, err := s.SessionModelRouteFor(ctx, session.ID)
	switch {
	case err == nil && route.Mode == "explicit":
		if route.ProviderID == nil {
			return RouteDecision{}, errors.New("this session's provider was deleted; choose another one")
		}
		provider, err := s.ModelProviderOwned(ctx, *route.ProviderID, userID)
		if err != nil {
			return RouteDecision{}, errors.New("this session's provider is unavailable; choose another one")
		}
		if !provider.Enabled {
			return RouteDecision{}, errors.New("this session's provider is disabled; enable it or choose another one")
		}
		if provider.Credential == "" {
			return RouteDecision{}, errors.New("this session's provider has no credential configured")
		}
		return RouteDecision{Provider: provider, Mode: "explicit"}, nil
	case err == nil:
		// Inherit mode falls through to the connector default.
	case errors.Is(err, ErrNotFound):
		// No row at all falls through to the connector default too.
	default:
		return RouteDecision{}, err
	}
	return s.ResolveConnectorDefault(ctx, session.ConnectorID)
}

// BumpSessionSeq advances a session's revision so a route or catalog change
// becomes an event clients can replay. Returns the refreshed session.
func (s *Store) BumpSessionSeq(ctx context.Context, sessionID, userID string) (Session, error) {
	var out Session
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions SET updated_seq = updated_seq + 1, updated_at = ?
			WHERE id = ? AND connector_id IN (SELECT id FROM connectors WHERE user_id = ?)`,
			now, sessionID, userID)
		if err != nil {
			return err
		}
		row := tx.QueryRowContext(ctx,
			`SELECT `+sessionColumnsFor("s")+` FROM sessions s
			JOIN connectors c ON c.id = s.connector_id
			WHERE s.id = ? AND c.user_id = ?`, sessionID, userID)
		out, err = scanSession(row)
		return err
	})
	return out, err
}

// MarkRouteApplied records that the gateway served a request under a target.
// An inheriting session has no route row yet, so one is created with mode
// inherit_device_default; an explicit route keeps its mode and target and
// only its applied_* fields move. A row already holding this exact applied
// state is left untouched, so steady traffic costs no writes.
func (s *Store) MarkRouteApplied(ctx context.Context, sessionID, userID, providerID string, revision int64, modelID string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO session_model_routes
				(session_id, user_id, mode, target_provider_id, target_revision,
				 applied_provider_id, applied_revision, applied_model_id, status, updated_at)
			VALUES (?, ?, 'inherit_device_default', NULL, NULL, ?, ?, ?, 'applied', ?)
			ON CONFLICT (session_id) DO UPDATE SET
				applied_provider_id = excluded.applied_provider_id,
				applied_revision = excluded.applied_revision,
				applied_model_id = excluded.applied_model_id,
				-- A switch to another provider waits for the next turn: serving
				-- the previous provider (a follow-up request of the running
				-- turn, pinned to it) must not announce the new route as live.
				-- Only the route's own target, or an inherit route (which has
				-- no target by definition), counts as applied. An explicit
				-- route whose provider was deleted must stay failed.
				status = CASE
					WHEN session_model_routes.mode = 'inherit_device_default'
					  OR session_model_routes.target_provider_id = excluded.applied_provider_id
					THEN 'applied' ELSE session_model_routes.status END,
				status_detail = CASE
					WHEN session_model_routes.mode = 'inherit_device_default'
					  OR session_model_routes.target_provider_id = excluded.applied_provider_id
					THEN NULL ELSE session_model_routes.status_detail END,
				updated_at = excluded.updated_at
			WHERE session_model_routes.applied_provider_id IS NOT excluded.applied_provider_id
			   OR session_model_routes.applied_revision IS NOT excluded.applied_revision
			   OR session_model_routes.applied_model_id IS NOT excluded.applied_model_id
			   OR (session_model_routes.status <> 'applied'
			       AND (session_model_routes.mode = 'inherit_device_default'
			            OR session_model_routes.target_provider_id = excluded.applied_provider_id))`,
			sessionID, userID, providerID, revision, modelID, now)
		if err != nil {
			return err
		}
		// A queued switch becomes applied at the same moment the route does,
		// so a client that polls the operation sees the transition.
		_, err = tx.ExecContext(ctx, `
			UPDATE route_switch_operations
			SET status = 'applied', updated_at = ?, completed_at = ?
			WHERE session_id = ? AND status = 'waiting_for_turn_end'
			  AND (target_provider_id IS NULL OR target_provider_id = ?)`,
			now, now, sessionID, providerID)
		return err
	})
}

// MarkRouteFailed notes a routing failure the UI should explain, and fails any
// operation still waiting on this session.
func (s *Store) MarkRouteFailed(ctx context.Context, sessionID, detail string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			UPDATE session_model_routes
			SET status = 'failed', status_detail = ?, updated_at = ?
			WHERE session_id = ?`, detail, now, sessionID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `
			UPDATE route_switch_operations
			SET status = 'failed', failure = ?, updated_at = ?, completed_at = ?
			WHERE session_id = ? AND status IN ('pending', 'waiting_for_turn_end')`,
			detail, now, now, sessionID)
		return err
	})
}

// ErrDuplicateIdempotencyKey reports that a switch with this key already
// exists; the caller should return the existing operation instead.
var ErrDuplicateIdempotencyKey = errors.New("a switch operation with this idempotency key already exists")

// CreateRouteSwitchOperation records a switch attempt. A non-empty
// idempotency key can only ever name one operation: a retry resolves to
// ErrDuplicateIdempotencyKey and the caller returns the existing row.
func (s *Store) CreateRouteSwitchOperation(ctx context.Context, op RouteSwitchOperation) (RouteSwitchOperation, error) {
	now := time.Now().UnixMilli()
	op.ID = NewID("rto")
	op.CreatedAt, op.UpdatedAt = now, now
	if op.Status == "" {
		op.Status = "pending"
	}
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var sessionID, connectorID, providerID, idem any
		if op.SessionID != nil {
			sessionID = *op.SessionID
		}
		if op.ConnectorID != nil {
			connectorID = *op.ConnectorID
		}
		if op.ProviderID != nil {
			providerID = *op.ProviderID
		}
		if op.IdempotencyKey != nil && *op.IdempotencyKey != "" {
			idem = *op.IdempotencyKey
		}
		// A route that applies immediately has no wait to record, so its
		// operation is complete at insert time.
		var completedAt any
		switch op.Status {
		case "applied", "failed", "cancelled":
			completedAt = now
			op.CompletedAt = &now
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO route_switch_operations
				(id, user_id, scope, session_id, connector_id, target_provider_id,
				 target_revision, idempotency_key, status, created_at, updated_at, completed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			op.ID, op.UserID, op.Scope, sessionID, connectorID, providerID,
			op.TargetRevision, idem, op.Status, now, now, completedAt)
		if isUniqueConstraint(err, "route_switch_operations.user_id, route_switch_operations.session_id, route_switch_operations.idempotency_key") {
			return ErrDuplicateIdempotencyKey
		}
		return err
	})
	if err != nil {
		return RouteSwitchOperation{}, err
	}
	return op, nil
}

// RouteSwitchOperationByID loads one operation for its owner.
func (s *Store) RouteSwitchOperationByID(ctx context.Context, id, userID string) (RouteSwitchOperation, error) {
	var op RouteSwitchOperation
	var sessionID, connectorID, providerID, idem, failure sql.NullString
	var targetRevision sql.NullInt64
	var completedAt sql.NullInt64
	err := s.Read().QueryRowContext(ctx, `
		SELECT id, user_id, scope, session_id, connector_id, target_provider_id,
		       target_revision, idempotency_key, status, failure, created_at, updated_at, completed_at
		FROM route_switch_operations WHERE id = ? AND user_id = ?`, id, userID).
		Scan(&op.ID, &op.UserID, &op.Scope, &sessionID, &connectorID, &providerID,
			&targetRevision, &idem, &op.Status, &failure, &op.CreatedAt, &op.UpdatedAt, &completedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RouteSwitchOperation{}, ErrNotFound
		}
		return RouteSwitchOperation{}, err
	}
	nullableToPtr(sessionID, &op.SessionID)
	nullableToPtr(connectorID, &op.ConnectorID)
	nullableToPtr(providerID, &op.ProviderID)
	nullableToPtr(idem, &op.IdempotencyKey)
	nullableToPtr(failure, &op.Failure)
	nullIntToPtr(targetRevision, &op.TargetRevision)
	nullIntToPtr(completedAt, &op.CompletedAt)
	return op, nil
}

// RouteSwitchOperationByIdempotencyKey finds a prior operation for a retried
// request, so the same key never produces two operations. The key is scoped to
// one session: reusing a key for another session is a different switch.
func (s *Store) RouteSwitchOperationByIdempotencyKey(ctx context.Context, userID, sessionID, key string) (RouteSwitchOperation, error) {
	var id string
	err := s.Read().QueryRowContext(ctx, `
		SELECT id FROM route_switch_operations
		WHERE user_id = ? AND session_id = ? AND idempotency_key = ?
		ORDER BY created_at DESC LIMIT 1`,
		userID, sessionID, key).Scan(&id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return RouteSwitchOperation{}, ErrNotFound
		}
		return RouteSwitchOperation{}, err
	}
	return s.RouteSwitchOperationByID(ctx, id, userID)
}

// FinishRouteSwitchOperation moves an operation to a terminal or waiting
// state. appliedAt is set for terminal states only.
func (s *Store) FinishRouteSwitchOperation(ctx context.Context, id, userID, status, failure string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		var failureValue any
		if failure != "" {
			failureValue = failure
		}
		var completedAt any
		switch status {
		case "applied", "failed", "cancelled":
			completedAt = now
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE route_switch_operations
			SET status = ?, failure = ?, updated_at = ?, completed_at = coalesce(?, completed_at)
			WHERE id = ? AND user_id = ?`,
			status, failureValue, now, completedAt, id, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

func nullableToPtr(n sql.NullString, dst **string) {
	if n.Valid {
		v := n.String
		*dst = &v
	}
}

func nullIntToPtr(n sql.NullInt64, dst **int64) {
	if n.Valid {
		v := n.Int64
		*dst = &v
	}
}
