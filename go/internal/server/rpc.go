package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	online := a.connectors.Online(connector.ID)
	runtime := map[string]any{
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
	httpx.OK(w, map[string]any{
		"connectorId": connector.ID,
		"runtimes":    []any{runtime},
		"serverTime":  timestampNow(),
	})
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
		"state":      a.runtimeState(session),
		"serverTime": timestampNow(),
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
