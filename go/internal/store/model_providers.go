package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ModelProvider is one user-owned third-party provider configuration. One
// configuration carries exactly one model: choosing the provider in a session
// chooses the model with it.
//
// Credential is stored in the clear by product decision and is only ever read
// by the gateway data plane. It must not be returned by any view or written to
// any log.
type ModelProvider struct {
	ID           string
	UserID       string
	Name         string
	Protocol     string // anthropic_messages | openai_chat_completions
	BaseURL      string
	AuthType     string // api_key | bearer
	Credential   string
	ModelID      string
	ModelName    string
	Capabilities string // JSON object as stored
	Enabled      bool
	Revision     int64
	CreatedAt    int64
	UpdatedAt    int64
}

const modelProviderColumns = `id, user_id, name, protocol, base_url, auth_type,
	credential_plaintext, model_id, model_display_name, capabilities_json,
	enabled, revision, created_at, updated_at`

// ProviderProtocols and ProviderAuthTypes are the only supported values in
// this version; each maps to a concrete adapter in the gateway.
var (
	ProviderProtocols = map[string]bool{
		"anthropic_messages":      true,
		"openai_chat_completions": true,
	}
	ProviderAuthTypes = map[string]bool{
		"api_key": true,
		"bearer":  true,
	}
)

// ErrProviderRevision is returned when a PATCH carries an expectedRevision the
// row has already moved past, so two devices cannot silently overwrite each
// other's edits.
var ErrProviderRevision = errors.New("provider was modified by another client")

// ErrProviderReferenced is returned when a delete would orphan sessions that
// explicitly route to the provider; the caller must present the references and
// let the person decide.
var ErrProviderReferenced = errors.New("provider is referenced by routes")

// ErrProviderUnusable is returned when a route or device default would point
// at a provider that cannot serve a request (disabled or credential-less).
// Routing is refused instead of accepted-then-failing at request time.
var ErrProviderUnusable = errors.New("provider is disabled or has no credential")

// ErrInvalidProvider wraps every validation failure so handlers can answer 400
// without string-matching.
var ErrInvalidProvider = errors.New("invalid provider configuration")

// Usable reports whether the provider can serve a request right now.
func (p ModelProvider) Usable() bool { return p.Enabled && p.Credential != "" }

// ValidateProvider checks the invariants the UI can rely on. The database
// CHECKs are the backstop; this gives readable errors.
func ValidateProvider(p ModelProvider) error {
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len(p.Name) > 100 {
		return fmt.Errorf("%w: provider name must be 1-100 characters", ErrInvalidProvider)
	}
	if !ProviderProtocols[p.Protocol] {
		return fmt.Errorf("%w: unsupported protocol", ErrInvalidProvider)
	}
	if !ProviderAuthTypes[p.AuthType] {
		return fmt.Errorf("%w: unsupported auth type", ErrInvalidProvider)
	}
	raw := strings.TrimSpace(p.BaseURL)
	if raw == "" || len(raw) > 2048 {
		return fmt.Errorf("%w: base URL must be 1-2048 characters", ErrInvalidProvider)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: base URL must be an absolute http(s) URL", ErrInvalidProvider)
	}
	// Credentials belong in the credential field, never in the URL, and a
	// query or fragment would be silently dropped when paths are joined.
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%w: base URL must not contain credentials, a query, or a fragment", ErrInvalidProvider)
	}
	p.ModelID = strings.TrimSpace(p.ModelID)
	if p.ModelID == "" || len(p.ModelID) > 200 {
		return fmt.Errorf("%w: model ID must be 1-200 characters", ErrInvalidProvider)
	}
	if len(p.ModelName) > 100 {
		return fmt.Errorf("%w: model display name must be at most 100 characters", ErrInvalidProvider)
	}
	return nil
}

func scanModelProvider(row interface{ Scan(...any) error }) (ModelProvider, error) {
	var p ModelProvider
	var enabled int
	err := row.Scan(&p.ID, &p.UserID, &p.Name, &p.Protocol, &p.BaseURL, &p.AuthType,
		&p.Credential, &p.ModelID, &p.ModelName, &p.Capabilities,
		&enabled, &p.Revision, &p.CreatedAt, &p.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ModelProvider{}, ErrNotFound
		}
		return ModelProvider{}, err
	}
	p.Enabled = enabled != 0
	return p, nil
}

// ListModelProviders returns the user's providers, most recently updated
// first. Credentials are loaded too — this method is server-internal only;
// handlers must project the rows through a view that omits the credential.
func (s *Store) ListModelProviders(ctx context.Context, userID string) ([]ModelProvider, error) {
	rows, err := s.Read().QueryContext(ctx,
		`SELECT `+modelProviderColumns+` FROM model_providers
		WHERE user_id = ? ORDER BY updated_at DESC, id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelProvider
	for rows.Next() {
		p, err := scanModelProvider(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ModelProviderByID returns one provider. The caller must verify ownership.
func (s *Store) ModelProviderByID(ctx context.Context, id string) (ModelProvider, error) {
	row := s.Read().QueryRowContext(ctx,
		`SELECT `+modelProviderColumns+` FROM model_providers WHERE id = ?`, id)
	return scanModelProvider(row)
}

// ModelProviderOwned loads a provider and verifies it belongs to the user.
func (s *Store) ModelProviderOwned(ctx context.Context, id, userID string) (ModelProvider, error) {
	p, err := s.ModelProviderByID(ctx, id)
	if err != nil {
		return ModelProvider{}, err
	}
	if p.UserID != userID {
		return ModelProvider{}, ErrNotFound
	}
	return p, nil
}

// CreateModelProvider inserts a provider; revision starts at 1.
func (s *Store) CreateModelProvider(ctx context.Context, in ModelProvider) (ModelProvider, error) {
	if in.AuthType == "" {
		in.AuthType = "api_key"
	}
	if err := ValidateProvider(in); err != nil {
		return ModelProvider{}, err
	}
	now := time.Now().UnixMilli()
	in.ID = NewID("prv")
	in.Name = strings.TrimSpace(in.Name)
	in.BaseURL = strings.TrimRight(strings.TrimSpace(in.BaseURL), "/")
	in.ModelID = strings.TrimSpace(in.ModelID)
	in.Credential = strings.TrimSpace(in.Credential)
	if in.Capabilities == "" {
		in.Capabilities = "{}"
	}
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO model_providers (id, user_id, name, protocol, base_url, auth_type,
			                             credential_plaintext, model_id, model_display_name,
			                             capabilities_json, enabled, revision, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)`,
			in.ID, in.UserID, in.Name, in.Protocol, in.BaseURL, in.AuthType,
			in.Credential, in.ModelID, in.ModelName, in.Capabilities,
			boolInt(in.Enabled), now, now)
		return err
	})
	if err != nil {
		return ModelProvider{}, err
	}
	in.Revision, in.CreatedAt, in.UpdatedAt = 1, now, now
	return in, nil
}

// ProviderPatch is the editable subset of a provider. The zero value of every
// field means "leave unchanged"; credential handling is explicit so an empty
// form can never be mistaken for "delete the key".
type ProviderPatch struct {
	Name         *string
	Protocol     *string
	BaseURL      *string
	AuthType     *string
	ModelID      *string
	ModelName    *string
	Enabled      *bool
	Capabilities *string
	// Credential replaces the stored secret when ReplaceCredential is set.
	ReplaceCredential bool
	Credential        string
	// ClearCredential removes the stored secret. Mutually exclusive with
	// ReplaceCredential.
	ClearCredential bool
}

// changesAnything reports whether the patch would write at least one field, so
// an empty PATCH does not bump the revision and break another client's
// expectedRevision.
func (p ProviderPatch) changesAnything() bool {
	return p.Name != nil || p.Protocol != nil || p.BaseURL != nil || p.AuthType != nil ||
		p.ModelID != nil || p.ModelName != nil || p.Enabled != nil || p.Capabilities != nil ||
		p.ReplaceCredential || p.ClearCredential
}

// UpdateModelProvider applies a patch with optimistic concurrency: the caller
// passes the revision it read; a row that moved on is refused rather than
// merged. Passing expectedRevision <= 0 skips the check (server-internal
// use).
func (s *Store) UpdateModelProvider(ctx context.Context, id, userID string, expectedRevision int64, patch ProviderPatch) (ModelProvider, error) {
	var updated ModelProvider
	err := s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT `+modelProviderColumns+` FROM model_providers WHERE id = ? AND user_id = ?`, id, userID)
		current, err := scanModelProvider(row)
		if err != nil {
			return err
		}
		if expectedRevision > 0 && current.Revision != expectedRevision {
			return ErrProviderRevision
		}
		if !patch.changesAnything() {
			updated = current
			return nil
		}
		next := current
		next.ID, next.UserID, next.CreatedAt = current.ID, current.UserID, current.CreatedAt
		if patch.Name != nil {
			next.Name = *patch.Name
		}
		if patch.Protocol != nil {
			next.Protocol = *patch.Protocol
		}
		if patch.BaseURL != nil {
			next.BaseURL = *patch.BaseURL
		}
		if patch.AuthType != nil {
			next.AuthType = *patch.AuthType
		}
		if patch.ModelID != nil {
			next.ModelID = *patch.ModelID
		}
		if patch.ModelName != nil {
			next.ModelName = *patch.ModelName
		}
		if patch.Enabled != nil {
			next.Enabled = *patch.Enabled
		}
		if patch.Capabilities != nil {
			next.Capabilities = *patch.Capabilities
		}
		if patch.ClearCredential {
			next.Credential = ""
		}
		if patch.ReplaceCredential {
			next.Credential = strings.TrimSpace(patch.Credential)
		}
		if err := ValidateProvider(next); err != nil {
			return err
		}
		next.Name = strings.TrimSpace(next.Name)
		next.BaseURL = strings.TrimRight(strings.TrimSpace(next.BaseURL), "/")
		now := time.Now().UnixMilli()
		res, err := tx.ExecContext(ctx, `
			UPDATE model_providers
			SET name = ?, protocol = ?, base_url = ?, auth_type = ?, credential_plaintext = ?,
			    model_id = ?, model_display_name = ?, capabilities_json = ?, enabled = ?,
			    revision = revision + 1, updated_at = ?
			WHERE id = ? AND revision = ?`,
			next.Name, next.Protocol, next.BaseURL, next.AuthType, next.Credential,
			next.ModelID, next.ModelName, next.Capabilities, boolInt(next.Enabled),
			now, id, current.Revision)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrProviderRevision
		}
		row = tx.QueryRowContext(ctx,
			`SELECT `+modelProviderColumns+` FROM model_providers WHERE id = ?`, id)
		updated, err = scanModelProvider(row)
		return err
	})
	if err != nil {
		return ModelProvider{}, err
	}
	return updated, nil
}

// ProviderReferences counts what still points at a provider, so a delete can
// be presented honestly instead of silently rerouting sessions.
type ProviderReferences struct {
	ConnectorDefaults int
	ExplicitRoutes    int
}

func (r ProviderReferences) Total() int { return r.ConnectorDefaults + r.ExplicitRoutes }

// CountProviderReferences counts the connector defaults and explicit session
// routes that would be affected by deleting or disabling a provider.
func (s *Store) CountProviderReferences(ctx context.Context, providerID, userID string) (ProviderReferences, error) {
	var refs ProviderReferences
	err := s.Read().QueryRowContext(ctx, `
		SELECT
			(SELECT count(*) FROM connector_model_defaults
			 WHERE provider_id = ? AND user_id = ?),
			(SELECT count(*) FROM session_model_routes
			 WHERE target_provider_id = ? AND user_id = ? AND mode = 'explicit')`,
		providerID, userID, providerID, userID).Scan(&refs.ConnectorDefaults, &refs.ExplicitRoutes)
	return refs, err
}

// DeleteModelProvider removes the provider only when nothing references it,
// so a blind delete can never orphan routes. Callers surface the reference
// counts and let the person decide; DeleteModelProviderForce is the explicit
// override.
func (s *Store) DeleteModelProvider(ctx context.Context, id, userID string) error {
	refs, err := s.CountProviderReferences(ctx, id, userID)
	if err != nil {
		return err
	}
	if refs.Total() > 0 {
		return ErrProviderReferenced
	}
	return s.DeleteModelProviderForce(ctx, id, userID)
}

// DeleteModelProviderForce removes the provider regardless of references.
// Explicit routes keep mode 'explicit' with a NULL target and must be
// re-selected; connector defaults cascade away.
func (s *Store) DeleteModelProviderForce(ctx context.Context, id, userID string) error {
	now := time.Now().UnixMilli()
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		// Queued switches to the provider can never complete.
		if _, err := tx.ExecContext(ctx, `
			UPDATE route_switch_operations
			SET status = 'failed', failure = 'provider deleted', updated_at = ?, completed_at = ?
			WHERE target_provider_id = ? AND user_id = ?
			  AND status IN ('pending', 'waiting_for_turn_end')`,
			now, now, id, userID); err != nil {
			return err
		}
		// The legacy selection column must not keep naming a deleted provider.
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions SET model_selection_id = NULL
			WHERE id IN (SELECT session_id FROM session_model_routes
			             WHERE target_provider_id = ? AND user_id = ? AND mode = 'explicit')`,
			id, userID); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx,
			`DELETE FROM model_providers WHERE id = ? AND user_id = ?`, id, userID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		// Connector defaults cascade via FK. Explicit routes keep their mode
		// with a NULL target (FK ON DELETE SET NULL); mark them failed so the
		// UI shows the true state instead of a silent inherit.
		_, err = tx.ExecContext(ctx, `
			UPDATE session_model_routes
			SET status = 'failed', status_detail = 'provider deleted',
			    updated_at = ?
			WHERE target_provider_id IS NULL AND user_id = ? AND mode = 'explicit'`,
			now, userID)
		return err
	})
}

// ProviderCheckResult is the outcome of one user-initiated connectivity check.
// Mode is "connect" (reachability only, no credential sent) or "request" (a
// real one-token model request, which proves reachability and authentication).
type ProviderCheckResult struct {
	Mode      string
	Reachable bool
	AuthOK    bool
	Status    int
	Detail    string
	At        int64
}

// SetProviderCheckResult records the outcome of a user-initiated check inside
// capabilities_json.check, which is free-form by contract.
func (s *Store) SetProviderCheckResult(ctx context.Context, id, userID string, result ProviderCheckResult) error {
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx,
			`SELECT capabilities_json FROM model_providers WHERE id = ? AND user_id = ?`, id, userID)
		var caps string
		if err := row.Scan(&caps); err != nil {
			return err
		}
		caps, err := mergeProviderCheck(caps, result)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE model_providers SET capabilities_json = ?, updated_at = ? WHERE id = ?`,
			caps, time.Now().UnixMilli(), id)
		return err
	})
}

// mergeProviderCheck folds a check outcome into the provider's capabilities
// JSON under the "check" key. The document is free-form by contract; a
// malformed stored value is replaced rather than propagated.
func mergeProviderCheck(caps string, result ProviderCheckResult) (string, error) {
	doc := map[string]any{}
	if caps != "" && json.Unmarshal([]byte(caps), &doc) != nil {
		doc = map[string]any{}
	}
	check := map[string]any{
		"mode":      result.Mode,
		"reachable": result.Reachable,
		"authOk":    result.AuthOK,
		"detail":    result.Detail,
		"at":        result.At,
	}
	if result.Status != 0 {
		check["status"] = result.Status
	}
	doc["check"] = check
	out, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
