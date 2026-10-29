package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// turnInFlight reports whether a session is mid-turn, in which case a route
// change waits for the next turn instead of applying immediately. "waiting"
// (a pending question) is mid-turn too: the turn resumes on its old provider.
func turnInFlight(session store.Session) bool {
	switch session.Status {
	case "running", "stopping", "waiting", "waiting_approval":
		return true
	default:
		return false
	}
}

// failModelGateway maps store errors onto the status codes the clients
// already understand, so a deletion, a stale revision, or a validation
// mistake never surfaces as an opaque 500.
func (a *App) failModelGateway(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.Fail(w, httpx.NotFound("not found"))
	case errors.Is(err, store.ErrProviderRevision):
		httpx.Fail(w, httpx.NewError(http.StatusConflict,
			"provider was modified by another client; refresh and edit again"))
	case errors.Is(err, store.ErrProviderReferenced):
		httpx.Fail(w, httpx.NewError(http.StatusConflict,
			"provider is referenced by routes; pass force=true after reviewing them"))
	case errors.Is(err, store.ErrProviderUnusable):
		httpx.Fail(w, httpx.NewError(http.StatusConflict,
			"the provider is disabled or has no credential; fix it before routing to it"))
	case errors.Is(err, store.ErrNoDeviceDefault):
		httpx.Fail(w, httpx.NotFound("no default model provider for this device"))
	case errors.Is(err, store.ErrInvalidProvider):
		httpx.Fail(w, httpx.BadRequest(err.Error()))
	default:
		httpx.Fail(w, err)
	}
}

// validateClientKey bounds client-supplied identifiers that become map keys or
// database values, so a hostile header cannot grow the server's memory.
func validateClientKey(name, value string, limit int) error {
	if len(value) > limit {
		return httpx.BadRequest(name + " is too long")
	}
	return nil
}

// Route and gateway views. Every response here is scoped to the signed-in
// user; the store verifies ownership of the connector, session, and provider
// inside the same transaction as the write.

// routeProviderView names a provider inside a route without repeating the
// whole provider configuration.
type routeProviderView struct {
	ProviderID       string `json:"providerId"`
	ProviderName     string `json:"providerName"`
	ModelID          string `json:"modelId"`
	ModelName        string `json:"modelDisplayName"`
	ProviderRevision int64  `json:"providerRevision"`
	Enabled          bool   `json:"enabled"`
}

func routeProviderViewFor(p store.ModelProvider) routeProviderView {
	return routeProviderView{
		ProviderID: p.ID, ProviderName: p.Name, ModelID: p.ModelID, ModelName: p.ModelName,
		ProviderRevision: p.Revision, Enabled: p.Enabled,
	}
}

// handleGetSessionModelRoute answers the session's routing state: the target,
// what the gateway last applied, the device default, the last real request,
// and whether the desktop has a gateway credential.
func (a *App) handleGetSessionModelRoute(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	view, err := a.sessionRouteView(r.Context(), session, user)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	httpx.OK(w, map[string]any{"route": view, "serverTime": timestampNow()})
}

func (a *App) sessionRouteView(ctx context.Context, session store.Session, user store.User) (map[string]any, error) {
	view := map[string]any{
		"sessionId": session.ID,
		"status":    "unknown",
	}
	route, routeErr := a.db.SessionModelRouteFor(ctx, session.ID)
	switch {
	case routeErr == nil:
		view["mode"] = route.Mode
		view["status"] = route.Status
		view["revision"] = route.UpdatedAt
		if route.StatusDetail != nil {
			view["statusDetail"] = *route.StatusDetail
		}
		if route.ProviderID != nil {
			if provider, err := a.db.ModelProviderOwned(ctx, *route.ProviderID, user.ID); err == nil {
				view["target"] = routeProviderViewFor(provider)
			} else {
				// Deleted provider: name the gap honestly.
				view["target"] = map[string]any{
					"providerId": *route.ProviderID, "providerName": nil, "modelId": nil,
					"providerRevision": route.TargetRevision, "enabled": false,
					"missing": true,
				}
				if view["status"] == "applied" {
					view["status"] = "failed"
					view["statusDetail"] = "provider deleted; choose another one"
				}
			}
		}
		if route.AppliedProviderID != nil {
			if provider, err := a.db.ModelProviderOwned(ctx, *route.AppliedProviderID, user.ID); err == nil {
				view["applied"] = routeProviderViewFor(provider)
			} else {
				view["applied"] = map[string]any{"providerId": *route.AppliedProviderID, "missing": true}
			}
		}
	case errors.Is(routeErr, store.ErrNotFound):
		view["mode"] = "inherit_device_default"
	default:
		return nil, routeErr
	}

	// The device default the session would inherit.
	view["deviceDefault"] = nil
	if def, err := a.db.ConnectorModelDefaultFor(ctx, session.ConnectorID); err == nil {
		if provider, err := a.db.ModelProviderOwned(ctx, def.ProviderID, def.UserID); err == nil {
			view["deviceDefault"] = routeProviderViewFor(provider)
		}
	}

	// Last real upstream request under this session.
	view["lastRequest"] = nil
	if last, err := a.db.LastGatewayRequest(ctx, session.ID); err == nil {
		entry := map[string]any{
			"status": last.Status, "at": isoTime(last.StartedAt),
		}
		if last.ProviderID != nil {
			entry["providerId"] = *last.ProviderID
		}
		if last.ModelID != nil {
			entry["modelId"] = *last.ModelID
		}
		if last.UpstreamStatus != nil {
			entry["upstreamStatus"] = *last.UpstreamStatus
		}
		if last.Error != nil {
			entry["error"] = *last.Error
		}
		view["lastRequest"] = entry
	}

	// Gateway access state for this session's desktop.
	view["gateway"] = a.connectorGatewayView(ctx, session.ConnectorID, user)
	return view, nil
}

func (a *App) connectorGatewayView(ctx context.Context, connectorID string, user store.User) map[string]any {
	view := map[string]any{"credentialStatus": "none", "connected": false}
	cred, err := a.db.LatestGatewayCredentialFor(ctx, connectorID, user.ID)
	if err == nil {
		view["credentialStatus"] = cred.Status
		view["credentialId"] = cred.CredentialID
		view["credentialPrefix"] = cred.Prefix
		view["createdAt"] = isoTime(cred.CreatedAt)
		if cred.LastUsedAt != nil {
			view["lastUsedAt"] = isoTime(*cred.LastUsedAt)
			// "Connected" means the desktop's Claude processes have actually
			// used the credential recently. A revoked credential is never
			// connected, however recently it was used.
			view["connected"] = cred.Status == "active" &&
				time.Since(time.UnixMilli(*cred.LastUsedAt)) < 10*time.Minute
		}
	}
	return view
}

// handlePutSessionModelRoute selects a provider for one session, or restores
// inheritance. The change lands in the routing table synchronously; the
// response's status says when it takes effect (a running turn finishes on its
// current provider first).
func (a *App) handlePutSessionModelRoute(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	var body struct {
		ProviderID     *string `json:"providerId"`
		Inherit        bool    `json:"inherit"`
		IdempotencyKey *string `json:"idempotencyKey"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		a.failModelGateway(w, err)
		return
	}
	if body.IdempotencyKey != nil {
		if err := validateClientKey("idempotencyKey", *body.IdempotencyKey, 128); err != nil {
			httpx.Fail(w, err)
			return
		}
	}
	mode, providerID := "inherit_device_default", ""
	if !body.Inherit && body.ProviderID != nil && *body.ProviderID != "" {
		mode, providerID = "explicit", *body.ProviderID
	}

	// Idempotent retries return the operation the first attempt created.
	if body.IdempotencyKey != nil && *body.IdempotencyKey != "" {
		if op, err := a.db.RouteSwitchOperationByIdempotencyKey(r.Context(), user.ID, session.ID, *body.IdempotencyKey); err == nil {
			view, err := a.sessionRouteView(r.Context(), session, user)
			if err != nil {
				a.failModelGateway(w, err)
				return
			}
			httpx.OK(w, map[string]any{"route": view, "operation": routeOperationView(op), "serverTime": timestampNow()})
			return
		}
	}

	route, err := a.db.SetSessionModelRoute(r.Context(), session.ID, user.ID, mode, providerID, turnInFlight(session))
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	a.gateway.invalidateSession(session.ID)

	operation := store.RouteSwitchOperation{
		UserID: user.ID, Scope: "session",
		SessionID: &session.ID, Status: route.Status,
	}
	if route.ProviderID != nil {
		operation.ProviderID = route.ProviderID
		operation.TargetRevision = route.TargetRevision
	}
	if body.IdempotencyKey != nil {
		key := *body.IdempotencyKey
		operation.IdempotencyKey = &key
	}
	created, opErr := a.db.CreateRouteSwitchOperation(r.Context(), operation)
	switch {
	case opErr == nil:
		operation = created
	case errors.Is(opErr, store.ErrDuplicateIdempotencyKey):
		// A retry raced the first attempt between the lookup and the insert;
		// answer with the operation that won.
		if existing, err := a.db.RouteSwitchOperationByIdempotencyKey(r.Context(), user.ID, session.ID, *body.IdempotencyKey); err == nil {
			operation = existing
		}
	default:
		// The route landed; the operation record is bookkeeping. Log and go
		// on rather than reporting a switch that actually happened as failed.
		a.log.Warn("route switch operation not recorded", "err", opErr)
	}

	updated, err := a.db.BumpSessionSeq(r.Context(), session.ID, user.ID)
	if err == nil {
		a.publishRouteChanged(r.Context(), updated, user.ID)
	}
	view, err := a.sessionRouteView(r.Context(), session, user)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	response := map[string]any{"route": view, "serverTime": timestampNow()}
	if operation.ID != "" {
		response["operation"] = routeOperationView(operation)
	}
	httpx.OK(w, response)
}

// publishRouteChanged tells the session's watchers that selections and the
// model catalog moved. The two events share the bumped sequence, matching the
// envelope contract.
func (a *App) publishRouteChanged(ctx context.Context, session store.Session, userID string) {
	state := a.runtimeState(ctx, session)
	if frame, err := newEnvelope(session.ID, session.UpdatedSeq, EventRuntimeState, map[string]any{
		"state": state,
	}); err == nil {
		a.broker.Publish(session.ID, frame)
	}
	catalog := a.modelCatalogForProviders(ctx, session.ConnectorID, session.ID, userID)
	payload, _ := json.Marshal(map[string]any{"catalogType": "model", "catalog": catalog})
	if frame, err := newEnvelope(session.ID, session.UpdatedSeq, EventCatalogUpdated, json.RawMessage(payload)); err == nil {
		a.broker.Publish(session.ID, frame)
	}
}

// publishCatalogsForUser pushes fresh model catalogs to every live session of
// an account after a provider change. Sessions watch their own channel, so
// each open conversation converges without a refetch.
func (a *App) publishCatalogsForUser(ctx context.Context, userID string) {
	for _, sessionID := range a.broker.SessionsWithSubscribers() {
		session, err := a.db.SessionByID(ctx, a.db.Read(), sessionID, userID)
		if err != nil {
			continue
		}
		updated, err := a.db.BumpSessionSeq(ctx, session.ID, userID)
		if err != nil {
			continue
		}
		a.publishRouteChanged(ctx, updated, userID)
	}
}

// handleGetConnectorModelRoute answers the desktop default plus gateway
// access, and how many sessions inherit it.
func (a *App) handleGetConnectorModelRoute(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	view := map[string]any{
		"connectorId": connector.ID,
		"gateway":     a.connectorGatewayView(r.Context(), connector.ID, user),
	}
	if def, err := a.db.ConnectorModelDefaultFor(r.Context(), connector.ID); err == nil {
		if provider, err := a.db.ModelProviderOwned(r.Context(), def.ProviderID, def.UserID); err == nil {
			view["default"] = routeProviderViewFor(provider)
			view["revision"] = def.UpdatedAt
		}
	} else {
		view["default"] = nil
	}
	httpx.OK(w, map[string]any{"route": view, "serverTime": timestampNow()})
}

// handlePutConnectorModelRoute sets the desktop default. applyToSessions:
//   - "none" (default): only future sessions and inheriting sessions follow;
//   - "inherit_only": same as none (inheriting sessions follow automatically);
//   - "all": additionally pin every session on this desktop to the provider,
//     overriding per-session choices. Each session is an independent
//     operation; one failing does not block the rest, and the response counts
//     the outcomes.
func (a *App) handlePutConnectorModelRoute(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	var body struct {
		ProviderID      *string `json:"providerId"`
		ApplyToSessions string  `json:"applyToSessions"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		a.failModelGateway(w, err)
		return
	}
	if body.ApplyToSessions == "" {
		body.ApplyToSessions = "none"
	}
	switch body.ApplyToSessions {
	case "none", "inherit_only", "all":
	default:
		httpx.Fail(w, httpx.BadRequest(`applyToSessions must be "none", "inherit_only", or "all"`))
		return
	}

	if body.ProviderID == nil || *body.ProviderID == "" {
		// Clearing the default never touches explicit session routes.
		if err := a.db.ClearConnectorModelDefault(r.Context(), connector.ID, user.ID); err != nil {
			a.failModelGateway(w, err)
			return
		}
		a.gateway.invalidateUser(user.ID)
		a.publishCatalogsForUser(r.Context(), user.ID)
		view := map[string]any{
			"connectorId": connector.ID, "default": nil,
			"gateway": a.connectorGatewayView(r.Context(), connector.ID, user),
		}
		httpx.OK(w, map[string]any{"route": view, "serverTime": timestampNow()})
		return
	}

	if _, err := a.db.SetConnectorModelDefault(r.Context(), connector.ID, user.ID, *body.ProviderID); err != nil {
		a.failModelGateway(w, err)
		return
	}
	// Inheriting sessions cached their own resolved decision, so the whole
	// account's cache is cleared, not just the device-default key.
	a.gateway.invalidateUser(user.ID)
	a.publishCatalogsForUser(r.Context(), user.ID)

	// Pin all sessions when asked. Counts are per-outcome, failures listed.
	pinned, failed := 0, 0
	var failures []string
	if body.ApplyToSessions == "all" {
		sessions, err := a.db.SessionsForConnector(r.Context(), connector.ID)
		if err != nil {
			a.failModelGateway(w, err)
			return
		}
		for _, session := range sessions {
			if _, err := a.db.SetSessionModelRoute(r.Context(), session.ID, user.ID, "explicit", *body.ProviderID, turnInFlight(session)); err != nil {
				failed++
				failures = append(failures, session.ID)
				continue
			}
			pinned++
			a.gateway.invalidateSession(session.ID)
			if updated, err := a.db.BumpSessionSeq(r.Context(), session.ID, user.ID); err == nil {
				a.publishRouteChanged(r.Context(), updated, user.ID)
			}
		}
	}

	view := map[string]any{
		"connectorId": connector.ID,
		"gateway":     a.connectorGatewayView(r.Context(), connector.ID, user),
	}
	view["default"] = nil
	if def, err := a.db.ConnectorModelDefaultFor(r.Context(), connector.ID); err == nil {
		if provider, err := a.db.ModelProviderOwned(r.Context(), def.ProviderID, def.UserID); err == nil {
			view["default"] = routeProviderViewFor(provider)
			view["revision"] = def.UpdatedAt
		}
	}
	response := map[string]any{"route": view, "serverTime": timestampNow()}
	if body.ApplyToSessions == "all" {
		response["applied"] = map[string]any{"pinned": pinned, "failed": failed, "failedSessions": failures}
	}
	httpx.OK(w, response)
}

// handleGetRouteOperation reports one switch operation.
func (a *App) handleGetRouteOperation(w http.ResponseWriter, r *http.Request, user store.User) {
	op, err := a.db.RouteSwitchOperationByID(r.Context(), r.PathValue("operation_id"), user.ID)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	httpx.OK(w, map[string]any{"operation": routeOperationView(op), "serverTime": timestampNow()})
}

// handleCancelRouteOperation cancels an operation that has not applied yet.
// It never rolls back an upstream request already sent.
func (a *App) handleCancelRouteOperation(w http.ResponseWriter, r *http.Request, user store.User) {
	op, err := a.db.RouteSwitchOperationByID(r.Context(), r.PathValue("operation_id"), user.ID)
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	switch op.Status {
	case "applied":
		httpx.Fail(w, httpx.Conflict("the switch already applied; switch again to change it"))
		return
	case "waiting_for_turn_end":
		// The route is already recorded and will take effect at the next turn;
		// cancelling would silently drop a choice the person made. Switching
		// back is the honest way to change it.
		httpx.Fail(w, httpx.Conflict(
			"the switch is queued for the next turn; choose another provider to change it"))
		return
	case "cancelled", "failed":
		// Terminal: answer with the current state, idempotently.
	default:
		if err := a.db.FinishRouteSwitchOperation(r.Context(), op.ID, user.ID, "cancelled", ""); err != nil {
			a.failModelGateway(w, err)
			return
		}
		op.Status = "cancelled"
	}
	httpx.OK(w, map[string]any{"operation": routeOperationView(op), "serverTime": timestampNow()})
}

func routeOperationView(op store.RouteSwitchOperation) map[string]any {
	view := map[string]any{
		"operationId": op.ID,
		"scope":       op.Scope,
		"status":      op.Status,
		"createdAt":   isoTime(op.CreatedAt),
		"updatedAt":   isoTime(op.UpdatedAt),
	}
	if op.SessionID != nil {
		view["sessionId"] = *op.SessionID
	}
	if op.ConnectorID != nil {
		view["connectorId"] = *op.ConnectorID
	}
	if op.ProviderID != nil {
		view["providerId"] = *op.ProviderID
	}
	if op.TargetRevision != nil {
		view["targetRevision"] = *op.TargetRevision
	}
	if op.Failure != nil {
		view["failure"] = *op.Failure
	}
	if op.CompletedAt != nil {
		view["completedAt"] = isoTime(*op.CompletedAt)
	}
	return view
}

// ---------------------------------------------------------------------------
// Gateway credential issuance
// ---------------------------------------------------------------------------

// handleEnableConnectorGateway issues (or rotates) the desktop's gateway
// credential and pushes the configuration to the connector over its
// authenticated socket. The token itself is returned only in the connector
// push, never to the browser or app.
func (a *App) handleEnableConnectorGateway(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	// The configured public origin is trusted; request headers are not, and a
	// pushed credential must never point at an attacker-chosen host.
	baseURL := a.cfg.PublicURL + "/api/v2/gateway"
	if !a.connectors.Online(connector.ID) {
		httpx.Fail(w, httpx.NewError(http.StatusConflict,
			"the device is offline; enable the gateway when it reconnects"))
		return
	}
	// Issue, push and any rollback form one critical section: two concurrent
	// enables would otherwise race their pushes and leave the device holding
	// the token the server already revoked.
	unlockConfig := a.gateway.lockConnectorConfig(connector.ID)
	defer unlockConfig()
	issued, token, err := a.db.IssueGatewayCredential(r.Context(), connector.ID, user.ID, func() (string, string, string, error) {
		token, err := newToken("gwc")
		if err != nil {
			return "", "", "", err
		}
		return token, HashToken(token), token[:12], nil
	})
	if err != nil {
		a.failModelGateway(w, err)
		return
	}
	revokedID := issued.ID
	if _, err := a.pushGatewayConfigAwait(r.Context(), connector.ID, baseURL, token); err != nil {
		// The device never acknowledged the credential, so leaving it active
		// would report an access path that does not exist. Revoke with a
		// context of its own: the request context may already be canceled.
		revokeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		if revokeErr := a.db.RevokeGatewayCredential(revokeCtx, revokedID, user.ID); revokeErr != nil {
			a.log.Warn("issued credential could not be revoked", "err", revokeErr)
		}
		cancel()
		// The connector may have applied it just after our timeout; clear the
		// device's copy so it cannot keep using a revoked token.
		a.sendGatewayConfig(connector.ID, "", "")
		switch {
		case errors.Is(err, ErrUnavailable):
			a.failModelGateway(w, httpx.NewError(http.StatusConflict,
				"the device went offline; enable the gateway again when it reconnects"))
		default:
			a.failModelGateway(w, httpx.NewError(http.StatusBadGateway,
				"the device did not accept the gateway configuration: "+err.Error()))
		}
		return
	}
	httpx.OK(w, map[string]any{
		"ok":         true,
		"pushed":     true,
		"baseUrl":    baseURL,
		"gateway":    a.connectorGatewayView(r.Context(), connector.ID, user),
		"serverTime": timestampNow(),
	})
}

// handleDisableConnectorGateway revokes the credential and tells the
// connector to stop wiring gateway env vars into new terminals.
func (a *App) handleDisableConnectorGateway(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	unlockConfig := a.gateway.lockConnectorConfig(connector.ID)
	defer unlockConfig()
	cred, err := a.db.LatestGatewayCredentialFor(r.Context(), connector.ID, user.ID)
	switch {
	case err == nil && cred.Status == "active":
		if err := a.db.RevokeGatewayCredential(r.Context(), cred.CredentialID, user.ID); err != nil {
			a.failModelGateway(w, err)
			return
		}
	case err != nil && !errors.Is(err, store.ErrNotFound):
		a.failModelGateway(w, err)
		return
	}
	pushed := a.pushGatewayConfig(r.Context(), connector.ID, "", "")
	httpx.OK(w, map[string]any{
		"ok":         true,
		"pushed":     pushed,
		"gateway":    a.connectorGatewayView(r.Context(), connector.ID, user),
		"serverTime": timestampNow(),
	})
}

// resyncGatewayConfig reconciles a reconnecting connector with the server's
// credential state. Only a revoked credential needs pushing: an active one was
// delivered when it was issued (issuance requires the device to be online),
// and a device that never had one has nothing to clear.
func (a *App) resyncGatewayConfig(ctx context.Context, connectorID, userID string) {
	cred, err := a.db.LatestGatewayCredentialFor(ctx, connectorID, userID)
	switch {
	case err == nil && cred.Status == "revoked":
		// Fire-and-forget: this runs on the connector read loop, so awaiting a
		// reply would deadlock on that same loop. If an enable/disable holds
		// the lock, that operation is authoritative and this is skipped.
		if unlock, ok := a.gateway.tryLockConnectorConfig(connectorID); ok {
			a.sendGatewayConfig(connectorID, "", "")
			unlock()
		}
	case err != nil && !errors.Is(err, store.ErrNotFound):
		a.log.Warn("gateway credential lookup failed", "connector", connectorID, "err", err)
	}
}

// pushGatewayConfig delivers the gateway endpoint and credential to the
// connector. Offline connectors get the config on reconnect: the hello path
// re-requests pending config when the credential status says active but the
// connector has not acknowledged (see connector_hub). Returns whether the
// frame was delivered to a live socket.
func (a *App) pushGatewayConfig(ctx context.Context, connectorID, baseURL, token string) bool {
	_, err := a.pushGatewayConfigAwait(ctx, connectorID, baseURL, token)
	return err == nil
}

// sendGatewayConfig is the one-way variant for the connector read loop, where
// awaiting a reply would deadlock: the reply can only arrive on that loop.
func (a *App) sendGatewayConfig(connectorID, baseURL, token string) bool {
	params := map[string]any{"baseUrl": baseURL, "enabled": token != ""}
	if token != "" {
		params["token"] = token
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return false
	}
	return a.connectors.Send(connectorID, protocol.TypeRPCRequest, protocol.RPCRequest{
		ID: store.NewID("rpc"), Method: "gateway.config", Params: raw,
	})
}

// pushGatewayConfigAwait delivers the config and waits for the connector's
// acknowledgement, so "pushed" means the device stored it — not merely that a
// frame was queued. It must not be called from the connector read loop: the
// reply arrives on that same loop.
func (a *App) pushGatewayConfigAwait(ctx context.Context, connectorID, baseURL, token string) (json.RawMessage, error) {
	params := map[string]any{
		"baseUrl": baseURL,
		"enabled": token != "",
	}
	if token != "" {
		params["token"] = token
	}
	if !a.connectors.Online(connectorID) {
		return nil, ErrUnavailable
	}
	return a.callConnector(ctx, connectorID, "gateway.config", params, 10*time.Second)
}
