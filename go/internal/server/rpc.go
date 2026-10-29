package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// rpcClient correlates server-to-connector calls with their answers.
//
// Every call carries an id; the response frame repeats it. A connector that
// disconnects mid-call leaves its waiters to time out rather than hang.
type rpcClient struct {
	mu      sync.Mutex
	pending map[string]chan protocol.RPCResponse
}

func newRPCClient() *rpcClient {
	return &rpcClient{pending: map[string]chan protocol.RPCResponse{}}
}

func (r *rpcClient) register(id string) chan protocol.RPCResponse {
	ch := make(chan protocol.RPCResponse, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	return ch
}

func (r *rpcClient) complete(id string, response protocol.RPCResponse) {
	r.mu.Lock()
	ch, ok := r.pending[id]
	delete(r.pending, id)
	r.mu.Unlock()
	if ok {
		select {
		case ch <- response:
		default:
		}
	}
}

func (r *rpcClient) forget(id string) {
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
}

// deliverRPC routes an inbound response to its caller.
func (a *App) deliverRPC(raw json.RawMessage) {
	var response protocol.RPCResponse
	if err := json.Unmarshal(raw, &response); err != nil || response.ID == "" {
		return
	}
	a.rpc.complete(response.ID, response)
}

// callConnector performs one RPC and returns its result.
func (a *App) callConnector(
	ctx context.Context, connectorID, method string, params any, timeout time.Duration,
) (json.RawMessage, error) {
	if !a.connectors.Online(connectorID) {
		return nil, ErrUnavailable
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	id := store.NewID("rpc")
	ch := a.rpc.register(id)
	defer a.rpc.forget(id)

	if !a.connectors.Send(connectorID, protocol.TypeRPCRequest, protocol.RPCRequest{
		ID: id, Method: method, Params: raw,
	}) {
		return nil, ErrUnavailable
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("connector did not answer %s in %s", method, timeout)
	case response := <-ch:
		if response.Error != nil {
			return nil, &rpcFailure{code: response.Error.Code, message: response.Error.Message}
		}
		return response.Result, nil
	}
}

// rpcFailure carries the connector's own error code through to the client.
type rpcFailure struct {
	code    string
	message string
}

func (e *rpcFailure) Error() string { return e.message }

// handleListRuntimes reports what the connector can run.
//
// Only one engine exists today, but the shape is a list because the client
// renders it generically and a second engine would otherwise be a client
// change.
func (a *App) handleListRuntimes(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	httpx.OK(w, map[string]any{
		"connectorId": connector.ID,
		"runtimes":    []any{a.claudeRuntimeView(connector, a.connectors.Online(connector.ID))},
		"serverTime":  timestampNow(),
	})
}

// handleGetRuntime returns the single runtime the connector exposes.
func (a *App) handleGetRuntime(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	if id := r.PathValue("runtime_id"); id != "claude" {
		httpx.Fail(w, httpx.NotFound("runtime not found"))
		return
	}
	httpx.OK(w, a.claudeRuntimeView(connector, a.connectors.Online(connector.ID)))
}

// claudeRuntimeView is the runtime shape both the list and the detail endpoint
// return. The connector currently exposes exactly one runtime.
func (a *App) claudeRuntimeView(connector store.Connector, online bool) map[string]any {
	return map[string]any{
		"connectorId":      connector.ID,
		"runtimeId":        "claude",
		"runtimeType":      "claude",
		"name":             "Claude Code",
		"displayName":      "Claude Code",
		"typeDisplayName":  "Claude Code",
		"present":          online,
		"available":        online,
		"reason":           nil,
		"configured":       true,
		"active":           online,
		"status":           map[bool]string{true: "running", false: "stopped"}[online],
		"discovery":        map[string]any{},
		"metadata":         map[string]any{},
		"schema":           nil,
		"uiSchema":         map[string]any{},
		"defaults":         map[string]any{},
		"capabilities":     map[string]any{},
		"config":           map[string]any{},
		"error":            nil,
		"phase":            nil,
		"lastDiscoveredAt": timestampNow(),
		"createdAt":        isoTime(connector.CreatedAt),
		"updatedAt":        isoTime(connector.UpdatedAt),
	}
}

// handleFSList proxies a directory listing from the desktop.
func (a *App) handleFSList(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Root string `json:"root"`
		Path string `json:"path"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	root := r.URL.Query().Get("root")
	if root == "" {
		root = body.Root
	}
	// Every other fs/* route validates containment before calling the
	// connector. fs/list must too: without it a caller can send a root and a
	// path that escapes it. An empty root means "no boundary stated": the
	// directory picker starts there before the person chooses a workspace, and
	// the connector resolves it to its default. Relative climbs are still
	// refused in that case.
	if strings.TrimSpace(root) == "" {
		cleaned := fsNormalize(body.Path)
		if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
				"path escapes the connector root"))
			return
		}
	} else {
		listPath := body.Path
		if strings.TrimSpace(listPath) == "" {
			// The connector treats an empty path as the root itself.
			listPath = "."
		}
		target, err := resolveFSTarget(root, listPath)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		root = target.Root
	}

	result, err := a.callConnector(r.Context(), connector.ID, "fs.list", map[string]any{
		"root": root,
		"path": body.Path,
	}, 15*time.Second)
	if err != nil {
		a.writeRPCError(w, err)
		return
	}
	httpx.OK(w, map[string]any{"ok": true, "result": json.RawMessage(result), "error": nil})
}

// writeRPCError maps a failed call onto the status codes the client knows.
func (a *App) writeRPCError(w http.ResponseWriter, err error) {
	var failure *rpcFailure
	switch {
	case errors.Is(err, ErrUnavailable):
		httpx.Fail(w, httpx.Conflict("connector is offline"))
	case errors.As(err, &failure):
		httpx.JSON(w, http.StatusBadGateway, map[string]any{
			"detail": map[string]any{"code": failure.code, "message": failure.message},
		})
	case errors.Is(err, context.DeadlineExceeded):
		httpx.Fail(w, httpx.NewError(http.StatusGatewayTimeout, "connector did not answer in time"))
	default:
		httpx.Fail(w, httpx.NewError(http.StatusGatewayTimeout, err.Error()))
	}
}

// handleRuntimeCapabilities answers what the connector can do for one runtime.
func (a *App) handleRuntimeCapabilities(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	online := a.connectors.Online(connector.ID)
	ids := []string{
		"session.send_message",
		"session.interrupt",
		"session.steer",
		"session.commands",
		"session.interaction.approval",
		"runtime.attachment",
		"catalog.model",
		"catalog.permission",
	}
	caps := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		caps = append(caps, map[string]any{
			"capabilityId":      id,
			"version":           "1",
			"scope":             "runtime",
			"runtime":           r.PathValue("runtime_id"),
			"sessionId":         nil,
			"supported":         true,
			"available":         online,
			"allowed":           true,
			"unavailableReason": nil,
			"parameters":        map[string]any{},
		})
	}
	httpx.OK(w, map[string]any{
		"connectorId": connector.ID,
		"capabilitySet": map[string]any{
			"revision":     connector.UpdatedAt,
			"capabilities": caps,
		},
		"serverTime": timestampNow(),
	})
}

// handleRuntimeState returns the session's runtime state.
func (a *App) handleRuntimeState(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	httpx.OK(w, map[string]any{
		"state":      a.runtimeState(r.Context(), session),
		"serverTime": timestampNow(),
	})
}

// handleModelCatalog returns the models offered for a session: one entry per
// model provider the account owns. The catalog item's selectionId is the
// provider id, so a picker selection and a route switch are the same act.
func (a *App) handleModelCatalog(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	httpx.OK(w, map[string]any{
		"catalog":    a.modelCatalogForProviders(r.Context(), session.ConnectorID, session.ID, user.ID),
		"serverTime": timestampNow(),
	})
}

// modelCatalogForProviders builds the model catalog from the account's
// providers. revision changes whenever any provider or the default moves, so
// clients refetch rather than serve a stale list.
func (a *App) modelCatalogForProviders(ctx context.Context, connectorID, sessionID, userID string) map[string]any {
	providers, err := a.db.ListModelProviders(ctx, userID)
	if err != nil {
		return map[string]any{"runtime": "claude", "revision": 1, "models": []any{}}
	}
	var defaultProviderID string
	if def, err := a.db.ConnectorModelDefaultFor(ctx, connectorID); err == nil {
		defaultProviderID = def.ProviderID
	}
	var currentSelection string
	var routeStatus, appliedProviderID, lastRequestStatus string
	if sessionID != "" {
		currentSelection = a.currentModelSelection(ctx, sessionID, userID)
		if route, err := a.db.SessionModelRouteFor(ctx, sessionID); err == nil {
			routeStatus = route.Status
			if route.AppliedProviderID != nil {
				appliedProviderID = *route.AppliedProviderID
			}
		}
		if last, err := a.db.LastGatewayRequest(ctx, sessionID); err == nil {
			lastRequestStatus = last.Status
		}
	}
	models := make([]map[string]any, 0, len(providers))
	var revision int64 = 1
	if def, err := a.db.ConnectorModelDefaultFor(ctx, connectorID); err == nil {
		revision += def.Revision
	}
	if sessionID != "" {
		if route, err := a.db.SessionModelRouteFor(ctx, sessionID); err == nil {
			revision += route.UpdatedAt % 1000
		}
	}
	for _, p := range providers {
		revision += p.Revision + p.UpdatedAt%1000
		// The picker shows provider and model together: choosing the entry
		// chooses both, so naming only one of them would hide half the choice.
		display := p.Name
		if p.ModelName != "" {
			display = p.Name + " · " + p.ModelName
		}
		description := p.ModelID + " · " + providerProtocolLabel(p.Protocol)
		// Routing state rides the description so the existing pickers show
		// target versus applied without new client plumbing: "waiting" is the
		// honest state while a turn still runs on the previous provider.
		routeState := ""
		switch {
		case p.ID == currentSelection && routeStatus == "waiting_for_turn_end":
			routeState = "waiting_for_turn_end"
			description += " · 等待当前回合结束"
		case p.ID == appliedProviderID && lastRequestStatus == "failed":
			routeState = "last_request_failed"
			description += " · 最近请求失败"
		case p.ID == appliedProviderID && (routeStatus == "applied" || lastRequestStatus == "completed"):
			routeState = "applied"
			description += " · 已生效"
		}
		if !p.Enabled {
			description += " · disabled"
		}
		entry := map[string]any{
			"id":          p.ID,
			"displayName": display,
			"selectionId": p.ID,
			"description": description,
			"default":     p.ID == defaultProviderID,
			"enabled":     p.Enabled,
			// reasoningItems is a required key for the shipped mobile decoder;
			// an omitted one would fail the whole catalog decode.
			"reasoningItems": []any{},
			"metadata": map[string]any{
				"providerId": p.ID,
				"modelId":    p.ModelID,
				"protocol":   p.Protocol,
				"revision":   p.Revision,
				// Machine-readable routing state; clients that localize later
				// can read this instead of the description suffix.
				"routeState": routeState,
			},
		}
		if !p.Enabled {
			entry["disabledReason"] = "provider is disabled"
		}
		if p.ID == currentSelection {
			entry["current"] = true
		}
		models = append(models, entry)
	}
	return map[string]any{"runtime": "claude", "revision": revision, "models": models}
}

// currentModelSelection resolves what the session's picker should show as
// selected: the explicit route's provider, or the inherited default.
func (a *App) currentModelSelection(ctx context.Context, sessionID, userID string) string {
	session, err := a.db.SessionByID(ctx, a.db.Read(), sessionID, userID)
	if err != nil {
		return ""
	}
	if route, err := a.db.SessionModelRouteFor(ctx, sessionID); err == nil && route.Mode == "explicit" {
		// A deleted explicit target stays explicit and empty: showing the
		// device default here would claim a route the gateway will refuse.
		if route.ProviderID == nil {
			return ""
		}
		return *route.ProviderID
	}
	if def, err := a.db.ConnectorModelDefaultFor(ctx, session.ConnectorID); err == nil {
		return def.ProviderID
	}
	return ""
}

func providerProtocolLabel(protocol string) string {
	switch protocol {
	case "anthropic_messages":
		return "Anthropic"
	case "openai_chat_completions":
		return "OpenAI"
	default:
		return protocol
	}
}

// permissionCatalogItems is the fixed four-mode permission catalog. The
// plugin API has no verified way to read or set the live mode of a running
// session, so every mode is published but marked not remotely settable; the
// mobile clients store a target while showing the honest limitation. When a
// verified control path exists, this table is where availability flips.
func permissionCatalogItems() []map[string]any {
	modes := []struct{ ID, Label, Description string }{
		{"default", "Ask every time", "Follow the session's permission rules; ask before anything not allowed."},
		{"acceptEdits", "Accept edits", "Automatically accept allowed file edits; other actions still follow the rules."},
		{"plan", "Plan", "Explore and plan without modifying files."},
		{"auto", "Auto", "Claude decides permissions automatically; only available on supporting environments."},
	}
	out := make([]map[string]any, 0, len(modes))
	for i, m := range modes {
		out = append(out, map[string]any{
			"id":             m.ID,
			"displayName":    m.Label,
			"selectionId":    m.ID,
			"description":    m.Description,
			"default":        i == 0,
			"enabled":        false,
			"disabledReason": "remote permission switching is not verified for this runtime yet; change it in the terminal",
			"metadata": map[string]any{
				"i18n": map[string]any{
					"labelKey":       "dashboard.new.permissionModes.claude." + m.ID + ".label",
					"descriptionKey": "dashboard.new.permissionModes.claude." + m.ID + ".description",
				},
			},
		})
	}
	return out
}

// handlePermissionCatalog mirrors the model catalog for permission modes.
func (a *App) handlePermissionCatalog(w http.ResponseWriter, r *http.Request, user store.User) {
	if _, ok := a.ownedSession(w, r, user); !ok {
		return
	}
	httpx.OK(w, map[string]any{
		"catalog": map[string]any{
			"runtime":     "claude",
			"revision":    1,
			"permissions": permissionCatalogItems(),
		},
		"serverTime": timestampNow(),
	})
}

// handleConnectorModelCatalog is the device-page variant of the session
// catalog: the account's providers with the device default flagged.
func (a *App) handleConnectorModelCatalog(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	if r.PathValue("runtime_id") != "claude" {
		httpx.Fail(w, httpx.NotFound("runtime not found"))
		return
	}
	httpx.OK(w, map[string]any{
		"catalog":    a.modelCatalogForProviders(r.Context(), connector.ID, "", user.ID),
		"serverTime": timestampNow(),
	})
}

// handleConnectorPermissionCatalog is the device-page permission catalog.
func (a *App) handleConnectorPermissionCatalog(w http.ResponseWriter, r *http.Request, user store.User) {
	if _, ok := a.ownedConnector(w, r, user); !ok {
		return
	}
	if r.PathValue("runtime_id") != "claude" {
		httpx.Fail(w, httpx.NotFound("runtime not found"))
		return
	}
	httpx.OK(w, map[string]any{
		"catalog": map[string]any{
			"runtime":     "claude",
			"revision":    1,
			"permissions": permissionCatalogItems(),
		},
		"serverTime": timestampNow(),
	})
}

// handleConnectorCommands serves the slash-command menu for a runtime on the
// device page. The menu is local presentation data, not connector state.
func (a *App) handleConnectorCommands(w http.ResponseWriter, r *http.Request, user store.User) {
	if _, ok := a.ownedConnector(w, r, user); !ok {
		return
	}
	if r.PathValue("runtime_id") != "claude" {
		httpx.Fail(w, httpx.NotFound("runtime not found"))
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, ok := atoi(raw); ok && n > 0 && n <= 1000 {
			limit = int(n)
		}
	}
	httpx.OK(w, map[string]any{
		"commands":   commandViews(query, limit),
		"serverTime": timestampNow(),
	})
}

// handleConnectorPreferences returns the per-device preferences document.
// Nothing is stored today, so the document is empty rather than missing.
func (a *App) handleConnectorPreferences(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	httpx.OK(w, map[string]any{
		"connectorId": connector.ID,
		"preferences": map[string]any{},
		"serverTime":  timestampNow(),
	})
}

// handleRuntimeManagementUnsupported is the honest answer for the multi-runtime
// management UI: this server has one implicit runtime and no persistence for
// runtime instances. Until that exists, 501 says so instead of a 404 the
// client would read as a missing route.
func (a *App) handleRuntimeManagementUnsupported(w http.ResponseWriter, r *http.Request, user store.User) {
	if _, ok := a.ownedConnector(w, r, user); !ok {
		return
	}
	httpx.Fail(w, httpx.NewError(http.StatusNotImplemented,
		"runtime instance management is not implemented by this server"))
}

// handleUpdateSelections persists a model or permission change and echoes the
// current state back. The connector has no selection RPC, so the value is kept
// on the session for the next client that hydrates it.
func (a *App) handleUpdateSelections(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Selections map[string]*string `json:"selections"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if err := a.applySelections(r.Context(), session.ID, body.Selections); err != nil {
		a.failModelGateway(w, err)
		return
	}
	// A model selection is a route switch: watchers of this session must see
	// the new state and catalog under a bumped sequence, and the gateway must
	// stop using any cached decision for it.
	_, changedModel := body.Selections["model"]
	_, changedPermission := body.Selections["permission"]
	if changedModel || changedPermission {
		a.gateway.invalidateSession(session.ID)
		if bumped, err := a.db.BumpSessionSeq(r.Context(), session.ID, user.ID); err == nil {
			a.publishRouteChanged(r.Context(), bumped, user.ID)
		}
	}
	updated, err := a.db.SessionByIDAny(r.Context(), a.db.Read(), session.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"ok":              true,
		"state":           a.runtimeState(r.Context(), updated),
		"connectorResult": nil,
		"serverTime":      timestampNow(),
	})
}

// applySelections writes the known selection scopes to the session columns.
// Unknown scopes are ignored: the clients may send an effort selection that
// this server has nowhere to keep.
//
// A model selection is also a route switch: the catalog publishes one
// selectionId per model provider, so choosing a model pins the session's
// gateway route to that provider. A null model selection restores
// inheritance of the device default. Permission selections stay target-only
// until a verified control path exists (see permissionCatalogItems).
func (a *App) applySelections(ctx context.Context, sessionID string, selections map[string]*string) error {
	if len(selections) == 0 {
		return nil
	}
	modelValue, hasModel := selections["model"]
	permissionValue, hasPermission := selections["permission"]
	if !hasModel && !hasPermission {
		return nil
	}

	var routeError error
	if hasModel {
		session, err := a.db.SessionByIDAny(ctx, a.db.Read(), sessionID)
		if err != nil {
			return err
		}
		mode, providerID := "inherit_device_default", ""
		if modelValue != nil && *modelValue != "" {
			mode, providerID = "explicit", *modelValue
		}
		ownerID := sessionOwnerUserID(ctx, a, session)
		// The catalog publishes provider ids as model selection ids, so an
		// explicit value that is not one of this account's providers is either
		// stale (the provider was deleted) or not a model at all. Reporting
		// that is honest; silently keeping the old column would tell the
		// client a switch happened when it did not.
		if mode == "explicit" {
			if _, err := a.db.ModelProviderOwned(ctx, providerID, ownerID); err != nil {
				return httpx.BadRequest("the selected model provider no longer exists; refresh and choose another")
			}
		}
		if mode != "" {
			_, routeError = a.db.SetSessionModelRoute(ctx, sessionID, ownerID, mode, providerID, turnInFlight(session))
			if routeError == nil {
				a.gateway.invalidateSession(sessionID)
			}
		}
	}
	// A refused route means the whole selection is refused: writing the
	// permission column anyway would leave the session half-updated.
	if routeError != nil {
		return routeError
	}

	sets := make([]string, 0, 3)
	args := make([]any, 0, 4)
	// The legacy column mirrors the route. When the route write failed, the
	// column must not be moved either, or readers would see a selection the
	// gateway refuses to use.
	if hasModel && routeError == nil {
		var model any
		if modelValue != nil {
			model = *modelValue
		}
		sets = append(sets, "model_selection_id = ?")
		args = append(args, model)
	}
	if hasPermission {
		var permission any
		if permissionValue != nil {
			permission = *permissionValue
		}
		sets = append(sets, "permission_selection_id = ?")
		args = append(args, permission)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, store.NowMS(), sessionID)
	err := a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		return err
	})
	if err != nil {
		return err
	}
	return routeError
}

// sessionOwnerUserID resolves the account that owns a session's connector.
// Route writes verify ownership against it.
func sessionOwnerUserID(ctx context.Context, a *App, session store.Session) string {
	connector, err := a.db.ConnectorByID(ctx, session.ConnectorID)
	if err != nil {
		return ""
	}
	return connector.UserID
}

// handleRuntimeTypes lists the engines this connector could run.
func (a *App) handleRuntimeTypes(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	online := a.connectors.Online(connector.ID)
	entry := map[string]any{
		"connectorId":        connector.ID,
		"runtimeType":        "claude",
		"implementationType": "cli",
		"displayName":        "Claude Code",
		"description":        "Anthropic's Claude Code CLI running on this device.",
		"present":            online,
		"available":          online,
		"reason":             nil,
		"recommended":        true,
		"recommendationRank": 1,
		"discovery":          map[string]any{},
		"configSchema":       nil,
		"schema":             nil,
		"uiSchema":           map[string]any{},
		"defaults":           map[string]any{},
		"capabilities":       map[string]any{},
		"metadata":           map[string]any{},
		"instancePolicy":     "single",
		"maxInstances":       1,
		"lastDiscoveredAt":   timestampNow(),
		"createdAt":          isoTime(connector.CreatedAt),
		"updatedAt":          isoTime(connector.UpdatedAt),
	}
	httpx.OK(w, map[string]any{
		"connectorId":  connector.ID,
		"runtimeTypes": []any{entry},
		"serverTime":   timestampNow(),
	})
}
