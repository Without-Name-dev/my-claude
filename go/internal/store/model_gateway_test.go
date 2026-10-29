package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
)

// hashToken mirrors the server's token hashing for the mint callback.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(context.Background(), filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestModelProviderCRUDAndOwnership(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	owner, err := s.CreateUser(ctx, User{Email: "owner@example.com", DisplayName: "Owner", PasswordHash: "x"})
	if err != nil {
		t.Fatalf("create owner: %v", err)
	}
	other, err := s.CreateUser(ctx, User{Email: "other@example.com", DisplayName: "Other", PasswordHash: "x"})
	if err != nil {
		t.Fatalf("create other: %v", err)
	}

	created, err := s.CreateModelProvider(ctx, ModelProvider{
		UserID: owner.ID, Name: "P", Protocol: "anthropic_messages",
		BaseURL: "https://api.example.com", AuthType: "api_key", Credential: "sk-1",
		ModelID: "m1", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if created.Revision != 1 || created.Credential != "sk-1" {
		t.Fatalf("unexpected created provider: %+v", created)
	}

	// Another user must not see or mutate it.
	if _, err := s.ModelProviderOwned(ctx, created.ID, other.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user read: %v", err)
	}

	// Absent credential keeps the stored one; clear removes it.
	name := "P2"
	if _, err := s.UpdateModelProvider(ctx, created.ID, owner.ID, 1, ProviderPatch{Name: &name}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	kept, _ := s.ModelProviderOwned(ctx, created.ID, owner.ID)
	if kept.Name != "P2" || kept.Credential != "sk-1" || kept.Revision != 2 {
		t.Fatalf("patch semantics wrong: %+v", kept)
	}
	if _, err := s.UpdateModelProvider(ctx, kept.ID, owner.ID, 2, ProviderPatch{ClearCredential: true}); err != nil {
		t.Fatalf("clear credential: %v", err)
	}
	cleared, _ := s.ModelProviderOwned(ctx, created.ID, owner.ID)
	if cleared.Credential != "" {
		t.Fatalf("credential not cleared: %q", cleared.Credential)
	}
	// Stale revision must be refused.
	if _, err := s.UpdateModelProvider(ctx, created.ID, owner.ID, 1, ProviderPatch{}); !errors.Is(err, ErrProviderRevision) {
		t.Fatalf("stale revision accepted: %v", err)
	}
}

func TestSessionRouteResolution(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	provider, _ := s.CreateModelProvider(ctx, ModelProvider{
		UserID: user.ID, Name: "P", Protocol: "anthropic_messages",
		BaseURL: "https://api.example.com", Credential: "sk",
		ModelID: "m1", Enabled: true,
	})
	session, err := s.CreateSession(ctx, Session{ConnectorID: connector.ID})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// No default and no route: honest error.
	if _, err := s.ResolveSessionRoute(ctx, session, user.ID); !errors.Is(err, ErrNoDeviceDefault) {
		t.Fatalf("expected ErrNoDeviceDefault, got %v", err)
	}

	// Device default resolves for an inheriting session.
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, provider.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}
	decision, err := s.ResolveSessionRoute(ctx, session, user.ID)
	if err != nil || decision.Mode != "inherit_device_default" || decision.Provider.ID != provider.ID {
		t.Fatalf("inherit resolution: %v %+v", err, decision)
	}

	// Explicit beats the default; disabling the provider fails honestly.
	if _, err := s.SetSessionModelRoute(ctx, session.ID, user.ID, "explicit", provider.ID, false); err != nil {
		t.Fatalf("set route: %v", err)
	}
	off := false
	s.UpdateModelProvider(ctx, provider.ID, user.ID, 0, ProviderPatch{Enabled: &off})
	if _, err := s.ResolveSessionRoute(ctx, session, user.ID); err == nil {
		t.Fatal("disabled provider must not resolve")
	}
	on := true
	s.UpdateModelProvider(ctx, provider.ID, user.ID, 0, ProviderPatch{Enabled: &on})

	// Deleting a referenced provider reports references; force leaves the
	// explicit route pointing at nothing and failing resolution.
	refs, _ := s.CountProviderReferences(ctx, provider.ID, user.ID)
	if refs.ConnectorDefaults != 1 || refs.ExplicitRoutes != 1 {
		t.Fatalf("reference counts wrong: %+v", refs)
	}
	if err := s.DeleteModelProvider(ctx, provider.ID, user.ID); !errors.Is(err, ErrProviderReferenced) {
		t.Fatalf("expected reference error, got %v", err)
	}
	if err := s.DeleteModelProviderForce(ctx, provider.ID, user.ID); err != nil {
		t.Fatalf("forced delete: %v", err)
	}
	if _, err := s.ResolveSessionRoute(ctx, session, user.ID); err == nil {
		t.Fatal("deleted provider must fail resolution, not fall back")
	}
	route, _ := s.SessionModelRouteFor(ctx, session.ID)
	if route.Mode != "explicit" || route.ProviderID != nil || route.Status != "failed" {
		t.Fatalf("route after provider delete: %+v", route)
	}
}

func TestGatewayCredentialLifecycle(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})

	mint := func() (string, string, string, error) {
		return "gwc_token1", hashToken("gwc_token1"), "gwc_token1", nil
	}
	_, token, err := s.IssueGatewayCredential(ctx, connector.ID, user.ID, mint)
	if err != nil || token != "gwc_token1" {
		t.Fatalf("issue: %v %q", err, token)
	}
	cred, err := s.GatewayCredentialByTokenHash(ctx, hashToken("gwc_token1"))
	if err != nil || cred.Status != "active" {
		t.Fatalf("resolve active: %v %+v", err, cred)
	}

	// Reissue rotates: the old token stops working.
	_, _, err = s.IssueGatewayCredential(ctx, connector.ID, user.ID, func() (string, string, string, error) {
		return "gwc_token2", hashToken("gwc_token2"), "gwc_token2", nil
	})
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if _, err := s.GatewayCredentialByTokenHash(ctx, hashToken("gwc_token1")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rotated token still resolves: %v", err)
	}
	if _, err := s.GatewayCredentialByTokenHash(ctx, hashToken("gwc_token2")); err != nil {
		t.Fatalf("new token does not resolve: %v", err)
	}
}

func TestRouteSwitchOperationIdempotency(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	session, _ := s.CreateSession(ctx, Session{ConnectorID: connector.ID})
	key := "idem-1"
	first, err := s.CreateRouteSwitchOperation(ctx, RouteSwitchOperation{
		UserID: user.ID, Scope: "session", SessionID: &session.ID, IdempotencyKey: &key,
	})
	if err != nil {
		t.Fatalf("create op: %v", err)
	}
	if _, err := s.CreateRouteSwitchOperation(ctx, RouteSwitchOperation{
		UserID: user.ID, Scope: "session", SessionID: &session.ID, IdempotencyKey: &key,
	}); !errors.Is(err, ErrDuplicateIdempotencyKey) {
		t.Fatalf("duplicate key accepted: %v", err)
	}
	found, err := s.RouteSwitchOperationByIdempotencyKey(ctx, user.ID, session.ID, key)
	if err != nil || found.ID != first.ID {
		t.Fatalf("idempotency lookup: %v %+v", err, found)
	}
}

func TestRoutingRejectsUnusableProviders(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	noKey, err := s.CreateModelProvider(ctx, ModelProvider{
		UserID: user.ID, Name: "no-key", Protocol: "anthropic_messages",
		BaseURL: "https://api.example.com", ModelID: "m", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, noKey.ID); !errors.Is(err, ErrProviderUnusable) {
		t.Fatalf("default to credential-less provider: %v", err)
	}
	session, _ := s.CreateSession(ctx, Session{ConnectorID: connector.ID})
	if _, err := s.SetSessionModelRoute(ctx, session.ID, user.ID, "explicit", noKey.ID, false); !errors.Is(err, ErrProviderUnusable) {
		t.Fatalf("route to credential-less provider: %v", err)
	}
	off := false
	keyed, _ := s.CreateModelProvider(ctx, ModelProvider{
		UserID: user.ID, Name: "keyed", Protocol: "anthropic_messages",
		BaseURL: "https://api.example.com", Credential: "sk", ModelID: "m", Enabled: true,
	})
	s.UpdateModelProvider(ctx, keyed.ID, user.ID, 0, ProviderPatch{Enabled: &off})
	if _, err := s.SetSessionModelRoute(ctx, session.ID, user.ID, "explicit", keyed.ID, false); !errors.Is(err, ErrProviderUnusable) {
		t.Fatalf("route to disabled provider: %v", err)
	}
}

func TestEmptyProviderPatchKeepsRevision(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	provider, _ := s.CreateModelProvider(ctx, ModelProvider{
		UserID: user.ID, Name: "p", Protocol: "anthropic_messages",
		BaseURL: "https://api.example.com", Credential: "sk", ModelID: "m", Enabled: true,
	})
	updated, err := s.UpdateModelProvider(ctx, provider.ID, user.ID, provider.Revision, ProviderPatch{})
	if err != nil {
		t.Fatalf("empty patch: %v", err)
	}
	if updated.Revision != provider.Revision {
		t.Fatalf("empty patch bumped revision: %d -> %d", provider.Revision, updated.Revision)
	}
}

func TestMarkRouteAppliedFinishesMatchingOperation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	session, _ := s.CreateSession(ctx, Session{ConnectorID: connector.ID})
	key := "k1"
	if _, err := s.CreateRouteSwitchOperation(ctx, RouteSwitchOperation{
		UserID: user.ID, Scope: "session", SessionID: &session.ID,
		Status: "waiting_for_turn_end", IdempotencyKey: &key,
	}); err != nil {
		t.Fatalf("op: %v", err)
	}
	if err := s.MarkRouteApplied(ctx, session.ID, user.ID, "prv_x", 2, "model-x"); err != nil {
		t.Fatalf("mark applied: %v", err)
	}
	op, err := s.RouteSwitchOperationByIdempotencyKey(ctx, user.ID, session.ID, key)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if op.Status != "applied" || op.CompletedAt == nil {
		t.Fatalf("operation not finished: %+v", op)
	}
	route, err := s.SessionModelRouteFor(ctx, session.ID)
	if err != nil || route.Status != "applied" || route.AppliedProviderID == nil {
		t.Fatalf("route not applied: %v %+v", err, route)
	}
}

func TestSwitchWaitsForNextTurn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	mk := func(name string) ModelProvider {
		p, err := s.CreateModelProvider(ctx, ModelProvider{
			UserID: user.ID, Name: name, Protocol: "anthropic_messages",
			BaseURL: "https://api.example.com", Credential: "sk", ModelID: name, Enabled: true,
		})
		if err != nil {
			t.Fatalf("provider %s: %v", name, err)
		}
		return p
	}
	a, b := mk("a"), mk("b")
	session, _ := s.CreateSession(ctx, Session{ConnectorID: connector.ID})
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, a.ID); err != nil {
		t.Fatalf("default: %v", err)
	}
	// A turn is already running on A.
	if err := s.MarkRouteApplied(ctx, session.ID, user.ID, a.ID, a.Revision, a.ModelID); err != nil {
		t.Fatalf("apply A: %v", err)
	}
	// The person switches to B while the turn runs.
	route, err := s.SetSessionModelRoute(ctx, session.ID, user.ID, "explicit", b.ID, true)
	if err != nil {
		t.Fatalf("switch: %v", err)
	}
	if route.Status != "waiting_for_turn_end" {
		t.Fatalf("status = %q, want waiting_for_turn_end", route.Status)
	}
	key := "turn-1"
	if _, err := s.CreateRouteSwitchOperation(ctx, RouteSwitchOperation{
		UserID: user.ID, Scope: "session", SessionID: &session.ID, ProviderID: &b.ID,
		Status: "waiting_for_turn_end", IdempotencyKey: &key,
	}); err != nil {
		t.Fatalf("op: %v", err)
	}
	// A follow-up request of the running turn is still served by A: the route
	// must keep waiting.
	if err := s.MarkRouteApplied(ctx, session.ID, user.ID, a.ID, a.Revision, a.ModelID); err != nil {
		t.Fatalf("apply A again: %v", err)
	}
	route, _ = s.SessionModelRouteFor(ctx, session.ID)
	if route.Status != "waiting_for_turn_end" {
		t.Fatalf("serving the old provider announced the switch: %q", route.Status)
	}
	op, _ := s.RouteSwitchOperationByIdempotencyKey(ctx, user.ID, session.ID, key)
	if op.Status != "waiting_for_turn_end" {
		t.Fatalf("operation finished early: %q", op.Status)
	}
	// The next turn is served by B; now the switch is real.
	if err := s.MarkRouteApplied(ctx, session.ID, user.ID, b.ID, b.Revision, b.ModelID); err != nil {
		t.Fatalf("apply B: %v", err)
	}
	route, _ = s.SessionModelRouteFor(ctx, session.ID)
	if route.Status != "applied" || route.AppliedProviderID == nil || *route.AppliedProviderID != b.ID {
		t.Fatalf("switch not applied: %+v", route)
	}
	op, _ = s.RouteSwitchOperationByIdempotencyKey(ctx, user.ID, session.ID, key)
	if op.Status != "applied" || op.CompletedAt == nil {
		t.Fatalf("operation not finished: %+v", op)
	}
}

func TestInheritSwitchWaitsDuringTurn(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	provider, _ := s.CreateModelProvider(ctx, ModelProvider{
		UserID: user.ID, Name: "p", Protocol: "anthropic_messages",
		BaseURL: "https://api.example.com", Credential: "sk", ModelID: "m", Enabled: true,
	})
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, provider.ID); err != nil {
		t.Fatalf("default: %v", err)
	}
	session, _ := s.CreateSession(ctx, Session{ConnectorID: connector.ID})
	route, err := s.SetSessionModelRoute(ctx, session.ID, user.ID, "inherit_device_default", "", true)
	if err != nil {
		t.Fatalf("inherit: %v", err)
	}
	if route.Status != "waiting_for_turn_end" {
		t.Fatalf("inherit switch ignored the running turn: %q", route.Status)
	}
}

func TestForceDeleteClearsSelectionAndFailsOperations(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	provider, _ := s.CreateModelProvider(ctx, ModelProvider{
		UserID: user.ID, Name: "p", Protocol: "anthropic_messages",
		BaseURL: "https://api.example.com", Credential: "sk", ModelID: "m", Enabled: true,
	})
	session, _ := s.CreateSession(ctx, Session{ConnectorID: connector.ID})
	if _, err := s.SetSessionModelRoute(ctx, session.ID, user.ID, "explicit", provider.ID, false); err != nil {
		t.Fatalf("route: %v", err)
	}
	key := "del-1"
	if _, err := s.CreateRouteSwitchOperation(ctx, RouteSwitchOperation{
		UserID: user.ID, Scope: "session", SessionID: &session.ID, ProviderID: &provider.ID,
		Status: "waiting_for_turn_end", IdempotencyKey: &key,
	}); err != nil {
		t.Fatalf("op: %v", err)
	}
	if err := s.DeleteModelProviderForce(ctx, provider.ID, user.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var selection sql.NullString
	if err := s.Read().QueryRowContext(ctx,
		`SELECT model_selection_id FROM sessions WHERE id = ?`, session.ID).Scan(&selection); err != nil {
		t.Fatalf("read selection: %v", err)
	}
	if selection.Valid && selection.String != "" {
		t.Fatalf("legacy selection still names the deleted provider: %q", selection.String)
	}
	op, err := s.RouteSwitchOperationByIdempotencyKey(ctx, user.ID, session.ID, key)
	if err != nil {
		t.Fatalf("op lookup: %v", err)
	}
	if op.Status != "failed" || op.Failure == nil {
		t.Fatalf("operation for deleted provider not failed: %+v", op)
	}
}

func TestNewSwitchCancelsSupersededOperation(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	mk := func(name string) ModelProvider {
		p, _ := s.CreateModelProvider(ctx, ModelProvider{
			UserID: user.ID, Name: name, Protocol: "anthropic_messages",
			BaseURL: "https://api.example.com", Credential: "sk", ModelID: name, Enabled: true,
		})
		return p
	}
	b, c := mk("b"), mk("c")
	session, _ := s.CreateSession(ctx, Session{ConnectorID: connector.ID})
	key := "sup-1"
	if _, err := s.CreateRouteSwitchOperation(ctx, RouteSwitchOperation{
		UserID: user.ID, Scope: "session", SessionID: &session.ID, ProviderID: &b.ID,
		Status: "waiting_for_turn_end", IdempotencyKey: &key,
	}); err != nil {
		t.Fatalf("op: %v", err)
	}
	// Choosing another provider before the turn ends supersedes the first.
	if _, err := s.SetSessionModelRoute(ctx, session.ID, user.ID, "explicit", c.ID, true); err != nil {
		t.Fatalf("second switch: %v", err)
	}
	op, _ := s.RouteSwitchOperationByIdempotencyKey(ctx, user.ID, session.ID, key)
	if op.Status != "cancelled" || op.CompletedAt == nil {
		t.Fatalf("superseded operation not cancelled: %+v", op)
	}
}

func TestDeletedTargetStaysFailedWhenOldProviderServes(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	mk := func(name string, enabled bool) ModelProvider {
		p, _ := s.CreateModelProvider(ctx, ModelProvider{
			UserID: user.ID, Name: name, Protocol: "anthropic_messages",
			BaseURL: "https://api.example.com", Credential: "sk", ModelID: name, Enabled: enabled,
		})
		return p
	}
	a, b := mk("a", true), mk("b", true)
	session, _ := s.CreateSession(ctx, Session{ConnectorID: connector.ID})
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, a.ID); err != nil {
		t.Fatalf("default: %v", err)
	}
	s.MarkRouteApplied(ctx, session.ID, user.ID, a.ID, a.Revision, a.ModelID)
	if _, err := s.SetSessionModelRoute(ctx, session.ID, user.ID, "explicit", b.ID, true); err != nil {
		t.Fatalf("switch: %v", err)
	}
	if err := s.DeleteModelProviderForce(ctx, b.ID, user.ID); err != nil {
		t.Fatalf("delete B: %v", err)
	}
	// The running turn is still served by A.
	if err := s.MarkRouteApplied(ctx, session.ID, user.ID, a.ID, a.Revision, a.ModelID); err != nil {
		t.Fatalf("apply A: %v", err)
	}
	route, _ := s.SessionModelRouteFor(ctx, session.ID)
	if route.Status != "failed" {
		t.Fatalf("deleted explicit target was reported applied: %q", route.Status)
	}
}

func TestGatewayCredentialRotationIsBounded(t *testing.T) {
	ctx := context.Background()
	s := testStore(t)
	user, _ := s.CreateUser(ctx, User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, Connector{UserID: user.ID, Name: "PC"})
	for i := 0; i < 30; i++ {
		n := i
		if _, _, err := s.IssueGatewayCredential(ctx, connector.ID, user.ID, func() (string, string, string, error) {
			token := "gwc_rot_" + string(rune('a'+n%26)) + string(rune('a'+n/26))
			return token, hashToken(token), token, nil
		}); err != nil {
			t.Fatalf("issue %d: %v", i, err)
		}
	}
	var rows int
	if err := s.Read().QueryRowContext(ctx,
		`SELECT count(*) FROM gateway_credentials WHERE connector_id = ?`, connector.ID).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows > 10 {
		t.Fatalf("credential history grew to %d rows", rows)
	}
}
