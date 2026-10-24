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
	if root := r.URL.Query().Get("root"); root != "" {
		body.Root = root
	}

	result, err := a.callConnector(r.Context(), connector.ID, "fs.list", map[string]any{
		"root": body.Root,
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

// handleModelCatalog returns the models offered for a session. The connector
// does not publish a catalog yet, so an empty one keeps the picker usable.
func (a *App) handleModelCatalog(w http.ResponseWriter, r *http.Request, user store.User) {
	if _, ok := a.ownedSession(w, r, user); !ok {
		return
	}
	httpx.OK(w, map[string]any{
		"catalog": map[string]any{
			"runtime":  "claude",
			"revision": 1,
			"models":   []any{},
		},
		"serverTime": timestampNow(),
	})
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
			"permissions": []any{},
		},
		"serverTime": timestampNow(),
	})
}

// handleConnectorModelCatalog is the device-page variant of the session
// catalog. There is only one runtime, and it publishes no catalog yet.
func (a *App) handleConnectorModelCatalog(w http.ResponseWriter, r *http.Request, user store.User) {
	if _, ok := a.ownedConnector(w, r, user); !ok {
		return
	}
	if r.PathValue("runtime_id") != "claude" {
		httpx.Fail(w, httpx.NotFound("runtime not found"))
		return
	}
	httpx.OK(w, map[string]any{
		"catalog": map[string]any{
			"runtime":  "claude",
			"revision": 1,
			"models":   []any{},
		},
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
			"permissions": []any{},
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
		httpx.Fail(w, err)
		return
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
func (a *App) applySelections(ctx context.Context, sessionID string, selections map[string]*string) error {
	if len(selections) == 0 {
		return nil
	}
	var model, permission any
	hasModel, hasPermission := false, false
	for scope, value := range selections {
		switch scope {
		case "model":
			hasModel = true
			if value != nil {
				model = *value
			}
		case "permission":
			hasPermission = true
			if value != nil {
				permission = *value
			}
		}
	}
	if !hasModel && !hasPermission {
		return nil
	}
	sets := make([]string, 0, 3)
	args := make([]any, 0, 4)
	if hasModel {
		sets = append(sets, "model_selection_id = ?")
		args = append(args, model)
	}
	if hasPermission {
		sets = append(sets, "permission_selection_id = ?")
		args = append(args, permission)
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, store.NowMS(), sessionID)
	return a.db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
		return err
	})
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
