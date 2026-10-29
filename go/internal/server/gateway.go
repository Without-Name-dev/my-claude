package server

// The model gateway is the data plane: desktop Claude processes send Anthropic
// Messages requests here, and this file decides, per request, which of the
// user's configured third-party providers serves it. The management plane
// (provider CRUD, routes) lives in model_provider_handlers.go and
// model_route_handlers.go.
//
// Invariants carried from the design document:
//
//   - A gateway credential identifies a connector (and through it a user).
//     The session header only locates a session; it is never an authority.
//   - A request whose session cannot be identified may use the device's
//     default route and is recorded as unbound; it is never attributed to
//     some other session.
//   - A provider that is disabled, deleted, or credential-less produces an
//     honest error. There is no silent fallback to a different provider.
//   - One HTTP stream is served by the upstream it started on, to its end.
//     A switch mid-turn only affects requests that have not started.
//   - Provider credentials never appear in logs, URLs, errors, or responses.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

const (
	// anthropicVersion is sent upstream when the client did not send one.
	anthropicVersion = "2023-06-01"
	// turnPinTTL bounds how long a prompt's route snapshot stays pinned.
	turnPinTTL = 30 * time.Minute
	// turnPinLimit bounds the pin table; prompt ids arrive per request.
	turnPinLimit = 4096
	// providerIDHeader is the session-locating header Claude Code sends.
	providerSessionHeader = "x-claude-code-session-id"
	// providerPromptHeader associates requests of one prompt/turn.
	providerPromptHeader = "x-claude-code-prompt-id"
)

// gateway holds the data-plane state. The route cache and turn pins are
// process-local by design; SQLite stays the source of truth.
type gateway struct {
	app    *App
	client *http.Client

	cache *gatewayRouteCache

	pinsMu sync.Mutex
	pins   map[string]turnPin
	pinSeq uint64

	// cfgLocks serializes credential issue/revoke plus their pushes per
	// connector, so two enables cannot interleave and leave the device with
	// the older (already revoked) token.
	cfgLocks sync.Map

	metrics gatewayMetrics
}

// turnPin fixes the provider for every request of one prompt, so a switch
// the person made mid-turn cannot split a turn across providers.
type turnPin struct {
	providerID string
	used       uint64
	expires    time.Time
}

// gatewayMetrics are aggregated counters only: no prompts, no bodies, no
// credential-bearing URLs. Exposed through the admin service endpoint.
type gatewayMetrics struct {
	active int64 // atomic

	mu        sync.RWMutex
	requests  int64
	errors    int64
	streams   int64
	bytesIn   int64
	bytesOut  int64
	providers map[string]*providerMetric
}

type providerMetric struct {
	requests int64
	errors   int64
	streams  int64
	bytesIn  int64
	bytesOut int64
}

func newGateway(app *App) *gateway {
	transport := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		MaxConnsPerHost:       64,
		ResponseHeaderTimeout: app.cfg.GatewayUpstreamHeaderTimeout,
		DialContext:           gatewayDialer,
	}
	return &gateway{
		app: app,
		client: &http.Client{
			Transport: transport,
			// A provider URL is a final destination. Following a redirect
			// would hand the provider credential (sent as x-api-key, which Go
			// keeps across same-origin redirects) to whatever host the first
			// response named, so a redirect is surfaced as an error instead.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		cache:   newGatewayRouteCache(),
		pins:    map[string]turnPin{},
		metrics: gatewayMetrics{providers: map[string]*providerMetric{}},
	}
}

// gatewayDialer refuses cloud metadata and link-local destinations. Private
// addresses stay reachable on purpose: self-hosted providers and local model
// proxies are legitimate, and operators who need tighter egress limits deploy
// the server on a restricted network.
func gatewayDialer(ctx context.Context, network, address string) (net.Conn, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip != nil && isForbiddenUpstreamIP(ip) {
		return nil, fmt.Errorf("gateway refuses to connect to %s", host)
	}
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	// A hostname that resolved to a forbidden address is caught here, after
	// DNS, because the dialer was handed the address, not the name.
	if remote, ok := conn.RemoteAddr().(*net.TCPAddr); ok && isForbiddenUpstreamIP(remote.IP) {
		conn.Close()
		return nil, fmt.Errorf("gateway refuses to connect to %s", address)
	}
	return conn, nil
}

// awsIPv6Metadata is the AWS instance metadata endpoint on IPv6. It sits in
// the unique-local range, so the link-local check alone would allow it.
var awsIPv6Metadata = net.ParseIP("fd00:ec2::254")

// isForbiddenUpstreamIP reports link-local (including 169.254.169.254), the
// AWS IPv6 metadata address, and the unspecified address. Loopback and the
// RFC 1918 / ULA ranges stay allowed for self-hosted providers.
func isForbiddenUpstreamIP(ip net.IP) bool {
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	return ip.Equal(awsIPv6Metadata)
}

// lockConnectorConfig serializes gateway-config changes for one connector.
// The returned function must be called to release the lock.
func (g *gateway) lockConnectorConfig(connectorID string) func() {
	value, _ := g.cfgLocks.LoadOrStore(connectorID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// tryLockConnectorConfig is the non-blocking variant for the connector read
// loop, where waiting could deadlock against an awaited push on that loop.
func (g *gateway) tryLockConnectorConfig(connectorID string) (func(), bool) {
	value, _ := g.cfgLocks.LoadOrStore(connectorID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, false
	}
	return mu.Unlock, true
}

// ---------------------------------------------------------------------------
// Route cache (bounded, no TTL, invalidated by the write paths)
// ---------------------------------------------------------------------------

type cacheEntry struct {
	decision store.RouteDecision
	userID   string
	size     int
	used     uint64
}

// gatewayRouteCache is the in-process route cache. Entries survive until an
// explicit invalidation or an LRU eviction; there is no timer-based expiry.
// The invalidation epoch stops a database read that raced an invalidation
// from writing a stale decision back over it.
type gatewayRouteCache struct {
	mu         sync.Mutex
	epoch      uint64 // bumped by every invalidation
	lru        uint64 // bumped by every get/put, for LRU ordering
	entries    map[string]cacheEntry
	order      []string // LRU order, oldest first
	maxEntries int
	maxBytes   int
	bytes      int
}

func newGatewayRouteCache() *gatewayRouteCache {
	return &gatewayRouteCache{
		entries:    map[string]cacheEntry{},
		maxEntries: 512,
		maxBytes:   2 << 20,
	}
}

func decisionSize(d store.RouteDecision) int {
	p := d.Provider
	return len(p.ID) + len(p.UserID) + len(p.Name) + len(p.Protocol) + len(p.BaseURL) +
		len(p.AuthType) + len(p.Credential) + len(p.ModelID) + len(p.ModelName) +
		len(p.Capabilities) + 128
}

func (c *gatewayRouteCache) get(key string) (store.RouteDecision, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return store.RouteDecision{}, false
	}
	c.lru++
	entry.used = c.lru
	c.entries[key] = entry
	// Move the key to the tail so eviction is least-recently-used, not FIFO.
	for i, k := range c.order {
		if k == key {
			c.order = append(append(c.order[:i], c.order[i+1:]...), key)
			break
		}
	}
	return entry.decision, true
}

// beginRead captures the invalidation epoch; putIfEpoch writes back only
// while the epoch is unchanged, so a config change that landed while the
// database read was in flight wins over the stale result.
func (c *gatewayRouteCache) beginRead() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.epoch
}

func (c *gatewayRouteCache) putIfEpoch(key string, epoch uint64, d store.RouteDecision) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.epoch {
		return
	}
	size := decisionSize(d)
	if size > c.maxBytes {
		return // one entry larger than the whole budget is never cached
	}
	if old, exists := c.entries[key]; exists {
		c.bytes -= old.size
	} else {
		c.order = append(c.order, key)
	}
	c.lru++
	c.entries[key] = cacheEntry{decision: d, userID: d.Provider.UserID, size: size, used: c.lru}
	c.bytes += size
	c.evictLocked()
}

// invalidateUser drops every cached decision belonging to one account — any
// provider, default, or route change of that user funnels here — and bumps
// the epoch so in-flight reads cannot write stale values back.
func (c *gatewayRouteCache) invalidateUser(userID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for existing, entry := range c.entries {
		if entry.userID == userID {
			c.bytes -= entry.size
			delete(c.entries, existing)
		}
	}
	c.order = c.order[:0]
	for key := range c.entries {
		c.order = append(c.order, key)
	}
	c.epoch++
}

// invalidateKey drops entries stored under one exact key (a session or a
// device default) and bumps the epoch.
func (c *gatewayRouteCache) invalidateKey(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.entries[key]; ok {
		c.bytes -= entry.size
		delete(c.entries, key)
		for i, k := range c.order {
			if k == key {
				c.order = append(c.order[:i], c.order[i+1:]...)
				break
			}
		}
	}
	c.epoch++
}

func (c *gatewayRouteCache) evictLocked() {
	for (len(c.entries) > c.maxEntries || c.bytes > c.maxBytes) && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		if entry, ok := c.entries[oldest]; ok {
			c.bytes -= entry.size
			delete(c.entries, oldest)
		}
	}
}

// Public invalidation helpers used by the management handlers. A provider
// edit can affect any session route or device default of the owner, so it
// clears the whole account; a route or default change clears its one key.

func (g *gateway) invalidateUser(userID string) {
	g.cache.invalidateUser(userID)
}

func (g *gateway) invalidateSession(sessionID string) {
	g.cache.invalidateKey("s:" + sessionID)
}

// ---------------------------------------------------------------------------
// Entry points
// ---------------------------------------------------------------------------

// handleGatewayMessages is POST /api/v2/gateway/v1/messages.
func (a *App) handleGatewayMessages(w http.ResponseWriter, r *http.Request) {
	g := a.gateway
	requestID := store.NewID("gwr")

	credential, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	// Best-effort usage stamp; never blocks the request.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = a.db.TouchGatewayCredential(ctx, credential.ID)
	}()

	body, err := io.ReadAll(io.LimitReader(r.Body, a.cfg.GatewayMaxBodyBytes+1))
	if err != nil {
		g.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"the request body could not be read")
		return
	}
	if int64(len(body)) > a.cfg.GatewayMaxBodyBytes {
		g.writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"the request body exceeded the gateway limit")
		return
	}

	externalID := strings.TrimSpace(r.Header.Get(providerSessionHeader))
	promptID := strings.TrimSpace(r.Header.Get(providerPromptHeader))
	if len(externalID) > 256 || len(promptID) > 128 {
		g.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"session or prompt identifier is too long")
		return
	}

	session, lookupErr := g.locateSession(r.Context(), credential, externalID)
	if lookupErr != nil {
		a.log.Warn("gateway session lookup failed", "err", lookupErr)
		g.writeAnthropicError(w, http.StatusInternalServerError, "api_error",
			"the gateway could not identify the session")
		return
	}
	decision, resolveErr := g.resolveRoute(r.Context(), session, credential, promptID)
	if resolveErr != nil {
		if session != nil {
			if err := a.db.MarkRouteFailed(context.WithoutCancel(r.Context()), session.ID, resolveErr.Error()); err != nil {
				a.log.Warn("route failure not recorded", "err", err)
			}
		}
		g.writeRouteError(w, resolveErr)
		return
	}

	// Rewrite only the top-level model field; every other byte of the request
	// passes through untouched, so signatures and cached content survive.
	rewritten, streaming, ok := rewriteModel(body, decision.Provider.ModelID)
	if !ok {
		g.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"the request body is not a valid Messages JSON object")
		return
	}

	metric := g.beginMetric(credential.UserID, decision.Provider.ID, int64(len(body)))
	defer metric.finish()

	// One request summary is written when the request finishes — never on
	// streaming chunks — and it is a single INSERT, so the gateway adds one
	// row per request rather than a start/update pair.
	summary := store.GatewayRequestSummary{
		ID: requestID, UserID: credential.UserID, ConnectorID: credential.ConnectorID,
		ProviderID: &decision.Provider.ID, ModelID: &decision.Provider.ModelID,
		ProviderRevision: &decision.Provider.Revision, StartedAt: store.NowMS(), Status: "streaming",
	}
	if session != nil {
		summary.SessionID = &session.ID
		summary.ExternalSessionID = &externalID
	}
	finishSummary := func(status string, upstreamStatus *int, bytesOut int64, upstreamRequestID string, failure *string) {
		record := summary
		record.Status = status
		record.BytesIn = int64(len(body))
		record.BytesOut = bytesOut
		record.UpstreamStatus = upstreamStatus
		record.Error = failure
		finished := store.NowMS()
		record.FinishedAt = &finished
		if upstreamRequestID != "" {
			record.UpstreamRequestID = &upstreamRequestID
		}
		row := record
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := a.db.RecordGatewayRequest(ctx, row); err != nil {
				a.log.Warn("gateway request summary not written", "err", err)
			}
		}()
	}

	// Serving a request under the target is the honest moment a waiting
	// switch becomes applied. Only the first request of a turn usually needs
	// the write; the earlier target check makes repeats a no-op.
	if session != nil {
		if err := a.db.MarkRouteApplied(context.WithoutCancel(r.Context()), session.ID, credential.UserID,
			decision.Provider.ID, decision.Provider.Revision, decision.Provider.ModelID); err != nil {
			a.log.Warn("route applied not marked", "err", err)
		}
	}

	switch decision.Provider.Protocol {
	case "anthropic_messages":
		g.forwardAnthropic(w, r, decision.Provider, rewritten, streaming, metric, finishSummary)
	case "openai_chat_completions":
		g.forwardOpenAI(w, r, decision.Provider, rewritten, streaming, metric, finishSummary)
	default:
		failure := "unsupported provider protocol"
		finishSummary("failed", nil, 0, "", &failure)
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"provider protocol is not supported by this gateway")
	}
}

// summaryFinisher closes one gateway request's log row.
type summaryFinisher func(status string, upstreamStatus *int, bytesOut int64, upstreamRequestID string, failure *string)

// handleGatewayCountTokens is POST /api/v2/gateway/v1/messages/count_tokens.
// Only Anthropic-protocol upstreams can forward it; for OpenAI-protocol
// providers the gateway answers with an explicit, honest error rather than
// inventing a token count.
func (a *App) handleGatewayCountTokens(w http.ResponseWriter, r *http.Request) {
	g := a.gateway
	credential, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, a.cfg.GatewayMaxBodyBytes+1))
	if err != nil || int64(len(body)) > a.cfg.GatewayMaxBodyBytes {
		g.writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"the request body exceeded the gateway limit")
		return
	}
	externalID := strings.TrimSpace(r.Header.Get(providerSessionHeader))
	if len(externalID) > 256 {
		g.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"session identifier is too long")
		return
	}
	session, lookupErr := g.locateSession(r.Context(), credential, externalID)
	if lookupErr != nil {
		a.log.Warn("gateway session lookup failed", "err", lookupErr)
		g.writeAnthropicError(w, http.StatusInternalServerError, "api_error",
			"the gateway could not identify the session")
		return
	}
	decision, resolveErr := g.resolveRoute(r.Context(), session, credential, "")
	if resolveErr != nil {
		g.writeRouteError(w, resolveErr)
		return
	}
	if decision.Provider.Protocol != "anthropic_messages" {
		g.writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			"count_tokens is not supported by this provider protocol")
		return
	}
	rewritten, _, ok := rewriteModel(body, decision.Provider.ModelID)
	if !ok {
		g.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"the request body is not a valid count_tokens JSON object")
		return
	}
	upstreamURL := anthropicMessagesURL(decision.Provider.BaseURL) + "/count_tokens"
	upstream, err := g.upstreamRequest(r.Context(), decision.Provider, http.MethodPost, upstreamURL, r, rewritten, false)
	if err != nil {
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not reach the provider")
		return
	}
	resp, err := g.client.Do(upstream)
	if err != nil {
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not reach the provider")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"the provider returned a redirect; point the provider URL at its final destination")
		return
	}
	copyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}

// handleGatewayModels is GET /api/v2/gateway/v1/models. Model discovery is
// optional for Claude Code; answering with the user's providers costs little
// and keeps manual /model use working.
func (a *App) handleGatewayModels(w http.ResponseWriter, r *http.Request) {
	g := a.gateway
	credential, ok := g.authenticate(w, r)
	if !ok {
		return
	}
	providers, err := a.db.ListModelProviders(r.Context(), credential.UserID)
	if err != nil {
		g.writeAnthropicError(w, http.StatusInternalServerError, "api_error", "could not list models")
		return
	}
	data := make([]map[string]any, 0, len(providers))
	for _, p := range providers {
		if !p.Enabled {
			continue
		}
		data = append(data, map[string]any{
			"id": p.ModelID, "type": "model",
			"display_name": providerDisplayName(p),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data, "has_more": false, "first_id": "", "last_id": ""})
}

func providerDisplayName(p store.ModelProvider) string {
	if p.ModelName != "" {
		return p.ModelName
	}
	return p.Name
}

// ---------------------------------------------------------------------------
// Authentication and session location
// ---------------------------------------------------------------------------

// authenticate resolves the gateway credential. The token rides the same
// headers an Anthropic API client uses, so any Claude configuration style
// works: Authorization: Bearer <token> or x-api-key: <token>.
func (g *gateway) authenticate(w http.ResponseWriter, r *http.Request) (store.GatewayCredential, bool) {
	token := bearerToken(r)
	if token == "" {
		token = r.Header.Get("x-api-key")
	}
	if token == "" {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agents-anywhere-gateway"`)
		g.writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"a gateway credential is required")
		return store.GatewayCredential{}, false
	}
	credential, err := g.app.db.GatewayCredentialByTokenHash(r.Context(), HashToken(token))
	if err != nil {
		g.writeAnthropicError(w, http.StatusUnauthorized, "authentication_error",
			"the gateway credential is invalid or revoked")
		return store.GatewayCredential{}, false
	}
	return credential, true
}

// locateSession maps the request's session header to a server session. A
// request that raced a /clear rebind still resolves through the rebind map;
// an unknown or absent header yields nil and the request is routed by device
// default, recorded as unbound.
// locateSession maps the request's session header to a server session. A nil
// session with a nil error means "not identified": the request is routed by
// device default. An error means the lookup itself failed, and falling back to
// the device default would silently misattribute the request.
func (g *gateway) locateSession(ctx context.Context, credential store.GatewayCredential, externalID string) (*store.Session, error) {
	if externalID == "" {
		return nil, nil
	}
	session, err := g.app.db.SessionByExternalID(ctx, credential.ConnectorID, externalID)
	if err == nil {
		return &session, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	sessionID, err := g.app.db.RebindTarget(ctx, credential.ConnectorID, externalID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	session, err = g.app.db.SessionByID(ctx, g.app.db.Read(), sessionID, credential.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &session, nil
}

// resolveRoute picks the provider snapshot for this request, honouring a
// prompt's pinned turn route when one exists.
func (g *gateway) resolveRoute(ctx context.Context, session *store.Session, credential store.GatewayCredential, promptID string) (store.RouteDecision, error) {
	if session != nil && promptID != "" {
		if providerID, ok := g.pinLookup(session.ID, promptID); ok {
			if provider, err := g.app.db.ModelProviderOwned(ctx, providerID, credential.UserID); err == nil &&
				provider.Enabled && provider.Credential != "" {
				return store.RouteDecision{Provider: provider, Mode: "explicit"}, nil
			}
			// The pinned provider went away mid-turn; resolve freshly rather
			// than failing the follow-up request.
		}
	}
	decision, err := g.resolveRouteFresh(ctx, session, credential)
	if err != nil {
		return decision, err
	}
	if session != nil && promptID != "" {
		g.pinSet(session.ID, promptID, decision.Provider.ID)
	}
	return decision, nil
}

func (g *gateway) resolveRouteFresh(ctx context.Context, session *store.Session, credential store.GatewayCredential) (store.RouteDecision, error) {
	if session != nil {
		key := "s:" + session.ID
		if d, ok := g.cache.get(key); ok {
			return d, nil
		}
		generation := g.cache.beginRead()
		d, err := g.app.db.ResolveSessionRoute(ctx, *session, credential.UserID)
		if err != nil {
			return d, err
		}
		g.cache.putIfEpoch(key, generation, d)
		return d, nil
	}
	key := "d:" + credential.ConnectorID
	if d, ok := g.cache.get(key); ok {
		return d, nil
	}
	generation := g.cache.beginRead()
	d, err := g.app.db.ResolveConnectorDefault(ctx, credential.ConnectorID)
	if err != nil {
		return d, err
	}
	g.cache.putIfEpoch(key, generation, d)
	return d, nil
}

func (g *gateway) pinLookup(sessionID, promptID string) (string, bool) {
	g.pinsMu.Lock()
	defer g.pinsMu.Unlock()
	pin, ok := g.pins[sessionID+"\x00"+promptID]
	if !ok || time.Now().After(pin.expires) {
		return "", false
	}
	g.pinSeq++
	pin.used = g.pinSeq
	// Sliding expiry: a long tool loop keeps its pin for as long as it is
	// actually making requests, instead of losing it 30 minutes after the
	// prompt started.
	pin.expires = time.Now().Add(turnPinTTL)
	g.pins[sessionID+"\x00"+promptID] = pin
	return pin.providerID, true
}

func (g *gateway) pinSet(sessionID, promptID, providerID string) {
	g.pinsMu.Lock()
	defer g.pinsMu.Unlock()
	key := sessionID + "\x00" + promptID
	if len(g.pins) >= turnPinLimit && g.pins[key].providerID == "" {
		// Evict the least recently used pin to keep the table bounded.
		var oldestKey string
		var oldest turnPin
		for k, p := range g.pins {
			if p.expires.Before(time.Now()) {
				oldestKey, oldest = k, p
				break
			}
			if oldestKey == "" || p.used < oldest.used {
				oldestKey, oldest = k, p
			}
		}
		if oldestKey != "" {
			delete(g.pins, oldestKey)
		}
	}
	g.pinSeq++
	g.pins[key] = turnPin{providerID: providerID, used: g.pinSeq, expires: time.Now().Add(turnPinTTL)}
}

// ---------------------------------------------------------------------------
// Forwarding
// ---------------------------------------------------------------------------

// rewriteModel replaces the top-level "model" member and reports whether the
// request asks for streaming. Every other member is preserved byte-for-byte
// as a RawMessage, so system prompts, tools, and cached content are untouched.
func rewriteModel(body []byte, modelID string) ([]byte, bool, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, false, false
	}
	streaming := false
	if raw, ok := doc["stream"]; ok {
		var flag bool
		if err := json.Unmarshal(raw, &flag); err == nil {
			streaming = flag
		}
	}
	replacement, err := json.Marshal(modelID)
	if err != nil {
		return nil, false, false
	}
	doc["model"] = replacement
	out, err := json.Marshal(doc)
	if err != nil {
		return nil, false, false
	}
	return out, streaming, true
}

// upstreamRequest builds the provider-bound request. The client's own auth
// headers are dropped; the provider's credential is injected. Nothing
// credential-bearing is logged.
func (g *gateway) upstreamRequest(ctx context.Context, provider store.ModelProvider, method, url string, original *http.Request, body []byte, streaming bool) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if provider.Protocol == "anthropic_messages" {
		if v := original.Header.Get("anthropic-version"); v != "" {
			req.Header.Set("anthropic-version", v)
		} else {
			req.Header.Set("anthropic-version", anthropicVersion)
		}
		for _, beta := range original.Header.Values("anthropic-beta") {
			req.Header.Add("anthropic-beta", beta)
		}
		switch provider.AuthType {
		case "bearer":
			req.Header.Set("Authorization", "Bearer "+provider.Credential)
		default:
			req.Header.Set("x-api-key", provider.Credential)
		}
	} else {
		// OpenAI-compatible endpoints authenticate with a bearer token.
		req.Header.Set("Authorization", "Bearer "+provider.Credential)
	}
	if streaming {
		req.Header.Set("Accept", "text/event-stream")
	} else if accept := original.Header.Get("Accept"); accept != "" {
		req.Header.Set("Accept", accept)
	}
	return req, nil
}

// forwardAnthropic streams an Anthropic-protocol upstream through untouched.
// Chunks are copied and flushed as they arrive; the request context cancels
// the upstream when the client disconnects.
func (g *gateway) forwardAnthropic(
	w http.ResponseWriter, r *http.Request, provider store.ModelProvider,
	body []byte, streaming bool, metric *requestMetric, finishSummary summaryFinisher,
) {
	upstreamURL := anthropicMessagesURL(provider.BaseURL)
	if raw := r.URL.RawQuery; raw != "" {
		upstreamURL += "?" + raw
	}
	upstream, err := g.upstreamRequest(r.Context(), provider, http.MethodPost, upstreamURL, r, body, streaming)
	if err != nil {
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not build the provider request")
		return
	}
	resp, err := g.client.Do(upstream)
	if err != nil {
		metric.recordError()
		failure := "upstream unreachable"
		finishSummary("failed", nil, 0, "", &failure)
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"the provider could not be reached: "+provider.Name)
		return
	}
	defer resp.Body.Close()
	if streaming {
		metric.recordStream()
	}
	upstreamRequestID := resp.Header.Get("request-id")
	if upstreamRequestID == "" {
		upstreamRequestID = resp.Header.Get("anthropic-request-id")
	}

	// A redirect is never followed (it would carry the provider credential to
	// another host) and never forwarded (the client would carry the gateway
	// credential there instead). It is an error the person can act on.
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		metric.recordError()
		status := resp.StatusCode
		failure := "provider returned a redirect"
		finishSummary("failed", &status, 0, upstreamRequestID, &failure)
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"the provider returned a redirect; point the provider URL at its final destination")
		return
	}

	if resp.StatusCode >= 400 {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		copyUpstreamHeaders(w, resp)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(limited)
		status := resp.StatusCode
		metric.recordError()
		failure := fmt.Sprintf("upstream status %d", status)
		finishSummary("failed", &status, int64(len(limited)), upstreamRequestID, &failure)
		return
	}

	// A stream was requested but the provider answered with a complete JSON
	// message. Replaying it as SSE keeps the client's parser working; handing
	// back the JSON would break the stream contract.
	if streaming && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		buffered, readErr := io.ReadAll(io.LimitReader(resp.Body, aBodyLimit))
		if readErr != nil {
			metric.recordError()
			failure := "upstream read failed"
			finishSummary("failed", &resp.StatusCode, 0, upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error", "the provider response could not be read")
			return
		}
		// Some proxies stream SSE without saying so. If the body is obviously
		// an event stream, forward it rather than trying to parse it as JSON.
		if trimmed := bytes.TrimLeft(buffered, " \r\n\t"); bytes.HasPrefix(trimmed, []byte("event:")) ||
			bytes.HasPrefix(trimmed, []byte("data:")) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(buffered)
			metric.recordBytes(int64(len(buffered)))
			finishSummary("completed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, nil)
			return
		}
		var answer anthropicResponse
		if err := json.Unmarshal(buffered, &answer); err != nil || answer.Type == "" {
			metric.recordError()
			failure := "upstream response was not a message"
			finishSummary("failed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error",
				"the provider did not return a valid Messages response")
			return
		}
		if answer.Type == "error" {
			metric.recordError()
			failure := "provider returned an error object"
			finishSummary("failed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, &failure)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write(buffered)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		written := writeSyntheticSSE(w, &answer)
		metric.recordBytes(written)
		finishSummary("completed", &resp.StatusCode, written, upstreamRequestID, nil)
		return
	}

	if !streaming {
		// Read the whole answer before writing a single byte: a connection
		// that dies mid-body would otherwise be forwarded as a truncated
		// "completed" JSON document the client cannot parse.
		buffered, readErr := io.ReadAll(io.LimitReader(resp.Body, aBodyLimit+1))
		if readErr != nil {
			metric.recordError()
			failure := "upstream read failed"
			finishSummary("failed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error",
				"the provider response could not be read")
			return
		}
		if int64(len(buffered)) > aBodyLimit {
			metric.recordError()
			failure := "upstream response too large"
			finishSummary("failed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error",
				"the provider response exceeded the gateway limit")
			return
		}
		copyUpstreamHeaders(w, resp)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(buffered)
		metric.recordBytes(int64(len(buffered)))
		finishSummary("completed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, nil)
		return
	}
	copyUpstreamHeaders(w, resp)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32<<10)
	var copied int64
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// Client went away; cancel the upstream immediately.
				finishSummary("cancelled", &resp.StatusCode, copied, upstreamRequestID, nil)
				return
			}
			copied += int64(n)
			metric.recordBytes(int64(n))
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr == io.EOF {
			finishSummary("completed", &resp.StatusCode, copied, upstreamRequestID, nil)
			return
		}
		if readErr != nil {
			// Mid-stream upstream failure: the client sees a truncated
			// stream, which SSE consumers treat as a network error. That is
			// the honest outcome; the gateway does not replay requests.
			failure := "stream interrupted"
			finishSummary("failed", &resp.StatusCode, copied, upstreamRequestID, &failure)
			return
		}
	}
}

// aBodyLimit bounds a non-streaming upstream response the gateway buffers.
const aBodyLimit = 128 << 20

// copyUpstreamHeaders passes through the response headers an Anthropic
// client reads: identification, rate limits, and retry guidance.
func copyUpstreamHeaders(w http.ResponseWriter, resp *http.Response) {
	for _, name := range []string{
		"Content-Type", "Request-Id", "Anthropic-Version",
		"Anthropic-Ratelimit-Limit", "Anthropic-Ratelimit-Requests-Limit",
		"Anthropic-Ratelimit-Requests-Remaining", "Anthropic-Ratelimit-Requests-Reset",
		"Anthropic-Ratelimit-Tokens-Limit", "Anthropic-Ratelimit-Tokens-Remaining",
		"Anthropic-Ratelimit-Tokens-Reset", "Retry-After", "Openai-Version",
		"X-Request-Id",
	} {
		if values, ok := resp.Header[name]; ok {
			for _, v := range values {
				w.Header().Add(name, v)
			}
		}
	}
}

// anthropicMessagesURL joins a provider base URL to the Messages path. Bases
// may be given as "https://host", "https://host/v1", or a full
// ".../v1/messages"; all three produce a predictable upstream URL.
func anthropicMessagesURL(baseURL string) string {
	b := strings.TrimRight(baseURL, "/")
	switch {
	case strings.HasSuffix(b, "/v1/messages"):
		return b
	case strings.HasSuffix(b, "/v1"):
		return b + "/messages"
	default:
		return b + "/v1/messages"
	}
}

// openaiChatURL joins a provider base URL to the chat completions path.
func openaiChatURL(baseURL string) string {
	b := strings.TrimRight(baseURL, "/")
	switch {
	case strings.HasSuffix(b, "/chat/completions"):
		return b
	case strings.HasSuffix(b, "/v1"):
		return b + "/chat/completions"
	default:
		return b + "/v1/chat/completions"
	}
}

// ---------------------------------------------------------------------------
// Errors
// ---------------------------------------------------------------------------

// writeAnthropicError answers in the error shape Anthropic clients parse.
func (g *gateway) writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": message},
	})
}

// writeRouteError maps routing failures to Anthropic error responses with
// messages a person can act on. No provider URLs or credentials leak.
func (g *gateway) writeRouteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNoDeviceDefault):
		g.writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			"no model provider is configured for this device; set a default provider or choose one for the session")
	case strings.Contains(err.Error(), "disabled"):
		g.writeAnthropicError(w, http.StatusForbidden, "permission_error", err.Error())
	default:
		g.app.log.Warn("gateway route resolution failed", "err", err)
		g.writeAnthropicError(w, http.StatusInternalServerError, "api_error",
			"the gateway could not resolve a model route")
	}
}

// ---------------------------------------------------------------------------
// Provider connectivity check
// ---------------------------------------------------------------------------

// checkProvider runs a user-initiated check. mode "request" sends a real
// minimal model request (which may cost money); "connect" only probes
// reachability and authentication.
func (g *gateway) checkProvider(ctx context.Context, provider store.ModelProvider, fullRequest bool) store.ProviderCheckResult {
	result := store.ProviderCheckResult{At: time.Now().UnixMilli()}
	if fullRequest {
		result.Mode = "request"
	} else {
		result.Mode = "connect"
	}
	checkCtx, cancel := context.WithTimeout(ctx, providerCheckTimeout)
	defer cancel()

	if !fullRequest {
		// Probe with a HEAD/GET on the messages endpoint: reachability and
		// TLS only, no token spend.
		req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, anthropicMessagesURL(provider.BaseURL), nil)
		if provider.Protocol == "openai_chat_completions" {
			req, err = http.NewRequestWithContext(checkCtx, http.MethodGet, openaiChatURL(provider.BaseURL), nil)
		}
		if err != nil {
			result.Detail = "invalid provider URL"
			return result
		}
		resp, err := g.client.Do(req)
		if err != nil {
			result.Detail = "provider unreachable: " + err.Error()
			return result
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		result.Reachable = true
		// A connect probe sends no credential and no request, so it proves
		// reachability only: no status and no auth verdict.
		result.Detail = "reachable"
		return result
	}

	// Minimal real request: one token, non-streaming.
	var payload []byte
	if provider.Protocol == "anthropic_messages" {
		payload, _ = json.Marshal(map[string]any{
			"model":      provider.ModelID,
			"max_tokens": 1,
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		})
	} else {
		payload, _ = json.Marshal(map[string]any{
			"model":      provider.ModelID,
			"max_tokens": 1,
			"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		})
	}
	var url string
	var req *http.Request
	var err error
	if provider.Protocol == "anthropic_messages" {
		url = anthropicMessagesURL(provider.BaseURL)
		req, err = http.NewRequestWithContext(checkCtx, http.MethodPost, url, bytes.NewReader(payload))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("anthropic-version", anthropicVersion)
			if provider.AuthType == "bearer" {
				req.Header.Set("Authorization", "Bearer "+provider.Credential)
			} else {
				req.Header.Set("x-api-key", provider.Credential)
			}
		}
	} else {
		url = openaiChatURL(provider.BaseURL)
		req, err = http.NewRequestWithContext(checkCtx, http.MethodPost, url, bytes.NewReader(payload))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+provider.Credential)
		}
	}
	if err != nil {
		result.Detail = "could not build the check request"
		return result
	}
	resp, err := g.client.Do(req)
	if err != nil {
		result.Detail = "provider unreachable: " + err.Error()
		return result
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 8<<10))
	result.Reachable = true
	result.Status = resp.StatusCode
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		result.AuthOK = true
		result.Detail = "model request succeeded"
	case resp.StatusCode >= 300 && resp.StatusCode < 400:
		result.Detail = "the provider redirected the request; point the URL at its final destination"
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		result.Detail = "authentication failed; check the provider credential"
	case resp.StatusCode >= 500:
		result.Detail = fmt.Sprintf("provider answered status %d; it may be unavailable", resp.StatusCode)
	default:
		// The request was rejected. That is not a pass: the model, parameters
		// or protocol may be wrong, and authentication is unproven.
		result.Detail = fmt.Sprintf("provider rejected the request with status %d", resp.StatusCode)
	}
	return result
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

type requestMetric struct {
	gateway    *gateway
	userID     string
	providerID string
	bytesIn    int64
}

// begin counts a request as started and returns its metric handle.
func (g *gateway) beginMetric(userID, providerID string, bytesIn int64) *requestMetric {
	atomic.AddInt64(&g.metrics.active, 1)
	g.metrics.mu.Lock()
	g.metrics.requests++
	g.metrics.bytesIn += bytesIn
	pm := g.metrics.providerLocked(providerID)
	pm.requests++
	pm.bytesIn += bytesIn
	g.metrics.mu.Unlock()
	return &requestMetric{gateway: g, userID: userID, providerID: providerID, bytesIn: bytesIn}
}

func (m *gatewayMetrics) providerLocked(providerID string) *providerMetric {
	if m.providers[providerID] == nil {
		m.providers[providerID] = &providerMetric{}
	}
	return m.providers[providerID]
}

// finish releases the active-request slot.
func (m *requestMetric) finish() {
	atomic.AddInt64(&m.gateway.metrics.active, -1)
}

func (m *requestMetric) recordStream() {
	g := m.gateway
	g.metrics.mu.Lock()
	g.metrics.streams++
	g.metrics.providerLocked(m.providerID).streams++
	g.metrics.mu.Unlock()
}

func (m *requestMetric) recordError() {
	g := m.gateway
	g.metrics.mu.Lock()
	g.metrics.errors++
	g.metrics.providerLocked(m.providerID).errors++
	g.metrics.mu.Unlock()
}

func (m *requestMetric) recordBytes(out int64) {
	g := m.gateway
	g.metrics.mu.Lock()
	g.metrics.bytesOut += out
	g.metrics.providerLocked(m.providerID).bytesOut += out
	g.metrics.mu.Unlock()
}

// forgetProvider drops a deleted provider's counters, so create/delete cycles
// do not grow the metrics map without bound.
func (g *gateway) forgetProvider(providerID string) {
	g.metrics.mu.Lock()
	delete(g.metrics.providers, providerID)
	g.metrics.mu.Unlock()
}

// statsSnapshot renders the counters for the admin service endpoint. No
// user content, no credentials.
func (g *gateway) statsSnapshot() map[string]any {
	g.metrics.mu.RLock()
	defer g.metrics.mu.RUnlock()
	providers := make(map[string]any, len(g.metrics.providers))
	for id, pm := range g.metrics.providers {
		providers[id] = map[string]any{
			"requests": pm.requests, "errors": pm.errors, "streams": pm.streams,
			"bytesIn": pm.bytesIn, "bytesOut": pm.bytesOut,
		}
	}
	return map[string]any{
		"active":    atomic.LoadInt64(&g.metrics.active),
		"requests":  g.metrics.requests,
		"errors":    g.metrics.errors,
		"streams":   g.metrics.streams,
		"bytesIn":   g.metrics.bytesIn,
		"bytesOut":  g.metrics.bytesOut,
		"providers": providers,
	}
}
