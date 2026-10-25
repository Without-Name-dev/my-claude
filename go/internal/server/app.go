// Package server assembles the HTTP API.
package server

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/auth"
	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// VerifyUserToken decodes a bearer into a user id.
func VerifyUserToken(token string) (string, error) { return auth.VerifyUserAccessToken(token) }

// App holds everything a request may need.
type App struct {
	cfg        config.Server
	db         *store.Store
	log        *slog.Logger
	tickets    *TicketManager
	broker     *Broker
	connectors *ConnectorHub
	turns      *turnRegistry
	approvals  *approveRegistry
	dashboards *dashboardHub
	history    *historyLimiter
	rpc        *rpcClient
	activity   *activityRegistry

	// openMu guards lastAutoOpen: when the send path last asked each session's
	// connector to open a terminal, so a retried message does not ask again
	// while the first one is still starting.
	openMu       sync.Mutex
	lastAutoOpen map[string]time.Time
}

// New wires the application.
func New(cfg config.Server, db *store.Store, log *slog.Logger) *App {
	return &App{
		cfg:        cfg,
		db:         db,
		log:        log,
		tickets:    NewTicketManager(),
		broker:     NewBroker(),
		connectors: NewConnectorHub(log),
		turns:      newTurnRegistry(),
		approvals:  newApproveRegistry(),
		dashboards: newDashboardHub(),
		history:    newHistoryLimiter(),
		rpc:        newRPCClient(),
		activity:   newActivityRegistry(),

		lastAutoOpen: map[string]time.Time{},
	}
}

// Handler returns the root router.
func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.OK(w, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("GET /api/v2/health", func(w http.ResponseWriter, r *http.Request) {
		// The iOS client decodes serverTime as a required field, so the health
		// probe has to carry it too.
		httpx.OK(w, map[string]any{"status": "ok", "serverTime": timestampNow()})
	})
	mux.HandleFunc("GET /api/v2/health/ready", a.handleReady)

	// Authentication.
	mux.HandleFunc("GET /api/v2/auth/config", a.handleAuthConfig)
	mux.HandleFunc("POST /api/v2/auth/password-salt", a.handlePasswordSalt)
	mux.HandleFunc("POST /api/v2/auth/register", a.handleRegister)
	mux.HandleFunc("POST /api/v2/auth/login", a.handleLogin)
	mux.HandleFunc("GET /api/v2/auth/me", a.requireUser(a.handleMe))
	mux.HandleFunc("PUT /api/v2/auth/me/profile", a.requireUser(a.handleUpdateProfile))

	// OAuth: the first-party apps sign in through the web login and redeem a
	// one-time code for a bearer token.
	mux.HandleFunc("GET /api/v2/oauth/authorize", a.requireUser(a.handleOAuthAuthorizeRedirect))
	mux.HandleFunc("POST /api/v2/oauth/authorize", a.requireUser(a.handleOAuthAuthorize))
	mux.HandleFunc("POST /api/v2/oauth/token", a.handleOAuthToken)
	mux.HandleFunc("PUT /api/v2/auth/me/avatar", a.requireUser(a.handleUpdateAvatar))
	mux.HandleFunc("DELETE /api/v2/auth/me/avatar", a.requireUser(a.handleDeleteAvatar))
	mux.HandleFunc("POST /api/v2/auth/change-password", a.requireUser(a.handleChangePassword))

	// Websocket tickets.
	mux.HandleFunc("POST /api/v2/ws-ticket", a.requireUser(a.handleWSTicket))

	// Connectors.
	mux.HandleFunc("GET /api/v2/connectors", a.requireUser(a.handleListConnectors))
	mux.HandleFunc("POST /api/v2/connectors", a.requireUser(a.handleCreateConnector))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}", a.requireUser(a.handleGetConnector))
	mux.HandleFunc("PATCH /api/v2/connectors/{connector_id}", a.requireUser(a.handlePatchConnector))
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/revoke", a.requireUser(a.handleRevokeConnector))
	mux.HandleFunc("DELETE /api/v2/connectors/{connector_id}", a.requireUser(a.handleDeleteConnector))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/runtimes", a.requireUser(a.handleListRuntimes))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/runtime-types", a.requireUser(a.handleRuntimeTypes))
	// Discovery is a no-op for this connector: it always reports exactly one
	// runtime type and one instance. Answering with the current inventory keeps
	// the device page's refresh button working.
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/runtime-types/discover", a.requireUser(a.handleRuntimeTypes))
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/runtimes/discover", a.requireUser(a.handleListRuntimes))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/preferences", a.requireUser(a.handleConnectorPreferences))
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/runtimes", a.requireUser(a.handleRuntimeManagementUnsupported))
	mux.HandleFunc("PATCH /api/v2/connectors/{connector_id}/runtimes/{runtime_id}", a.requireUser(a.handleRuntimeManagementUnsupported))
	mux.HandleFunc("PUT /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/config", a.requireUser(a.handleRuntimeManagementUnsupported))
	mux.HandleFunc("DELETE /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/config", a.requireUser(a.handleRuntimeManagementUnsupported))
	mux.HandleFunc("PUT /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/active", a.requireUser(a.handleRuntimeManagementUnsupported))
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/fs/list", a.requireUser(a.handleFSList))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}", a.requireUser(a.handleGetRuntime))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/capabilities", a.requireUser(a.handleRuntimeCapabilities))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/catalogs/model", a.requireUser(a.handleConnectorModelCatalog))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/catalogs/permission", a.requireUser(a.handleConnectorPermissionCatalog))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/runtimes/{runtime_id}/commands", a.requireUser(a.handleConnectorCommands))

	// Projects.
	mux.HandleFunc("GET /api/v2/projects", a.requireUser(a.handleListProjects))
	mux.HandleFunc("POST /api/v2/projects", a.requireUser(a.handleCreateProject))
	mux.HandleFunc("PATCH /api/v2/projects/{project_id}", a.requireUser(a.handlePatchProject))
	mux.HandleFunc("DELETE /api/v2/projects/{project_id}", a.requireUser(a.handleDeleteProject))
	mux.HandleFunc("GET /api/v2/projects/{project_id}/sessions", a.requireUser(a.handleProjectSessions))
	mux.HandleFunc("POST /api/v2/projects/{project_id}/sessions/archive-all", a.requireUser(a.handleProjectArchiveAll))

	// Announcements.
	mux.HandleFunc("GET /api/v2/announcement", a.handleAnnouncement)
	a.registerAdminRoutes(mux)
	a.registerAdminUserEditRoutes(mux)

	// Pairing.
	mux.HandleFunc("POST /api/v2/pairing/start", a.handlePairingStart)
	mux.HandleFunc("POST /api/v2/pairing/claim", a.requireUser(a.handlePairingClaim))
	mux.HandleFunc("POST /api/v2/pairing/poll", a.handlePairingPoll)

	// Sessions.
	mux.HandleFunc("POST /api/v2/sessions", a.requireUser(a.handleCreateSession))
	mux.HandleFunc("POST /api/v2/sessions/create-and-start", a.requireUser(a.handleCreateAndStart))
	mux.HandleFunc("GET /api/v2/sessions", a.requireUser(a.handleListSessions))
	mux.HandleFunc("GET /api/v2/sessions/list", a.requireUser(a.handleListAllSessions))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/snapshot", a.requireUser(a.handleSnapshot))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/events", a.requireUser(a.handleEvents))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/timeline", a.requireUser(a.handleTimeline))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/meta", a.requireUser(a.handleSessionMeta))
	mux.HandleFunc("PATCH /api/v2/sessions/{session_id}/meta", a.requireUser(a.handlePatchSession))
	mux.HandleFunc("POST /api/v2/sessions/archive", a.requireUser(a.handleBulkArchive))
	mux.HandleFunc("POST /api/v2/sessions/unarchive", a.requireUser(a.handleBulkArchive))
	mux.HandleFunc("POST /api/v2/sessions/read", a.requireUser(a.handleBulkRead))
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/takeover", a.requireUser(a.handleTakeover))
	mux.HandleFunc("DELETE /api/v2/sessions/{session_id}/takeover", a.requireUser(a.handleReleaseTakeover))
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/runtime/messages", a.requireUser(a.handleSendMessage))
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/runtime/open", a.requireUser(a.handleOpenRuntime))
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/runtime/close", a.requireUser(a.handleCloseRuntime))
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/runtime/interrupt", a.requireUser(a.handleInterrupt))
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/runtime/steer", a.requireUser(a.handleSteer))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/runtime/state", a.requireUser(a.handleRuntimeState))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/activity", a.requireUser(a.handleSessionActivity))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/runtime/capabilities", a.requireUser(a.handleSessionCapabilities))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/runtime/notices", a.requireUser(a.handleSessionNotices))
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/sync", a.requireUser(a.handleSessionSync))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/runtime/catalogs/model", a.requireUser(a.handleModelCatalog))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/runtime/catalogs/permission", a.requireUser(a.handlePermissionCatalog))
	mux.HandleFunc("PATCH /api/v2/sessions/{session_id}/runtime/selections", a.requireUser(a.handleUpdateSelections))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/runtime/commands", a.requireUser(a.handleListCommands))
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/runtime/commands", a.requireUser(a.handleRunCommand))
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/runtime/notices/{notice_id}/respond", a.requireUser(a.handleNoticeRespond))

	// Attachments and file downloads.
	a.registerAttachmentRoutes(mux)
	a.registerFileRoutes(mux)
	a.registerShareRoutes(mux)
	a.registerTerminalRoutes(mux)

	// Websockets.
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/ws", a.handleSessionWS)
	mux.HandleFunc("GET /api/v2/connector/ws", a.ServeConnectorWS)
	mux.HandleFunc("GET /api/v2/dashboard/ws", a.handleDashboardWS)
	mux.HandleFunc("GET /api/v2/stream/ws", a.handleStreamWS)

	// Anything under the API prefix that no route claimed answers in JSON.
	// A plain-text 404 is what the client shows as "404 page not found".
	mux.HandleFunc("/api/v2/", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusNotFound, map[string]string{"detail": "not found"})
	})

	var handler http.Handler = mux
	if a.cfg.WebDir != "" {
		handler = a.withStaticFiles(mux)
	}
	return a.withCommon(handler)
}

// Start launches the background work that outlives a single request.
func (a *App) Start(ctx context.Context) {
	go a.refreshLiveHistories(ctx)
}

// handleReady reports database reachability.
func (a *App) handleReady(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.db.Read().PingContext(ctx); err != nil {
		httpx.JSON(w, http.StatusServiceUnavailable, map[string]any{
			"status": "unavailable",
			"detail": "database",
		})
		return
	}
	httpx.OK(w, map[string]any{"status": "ready"})
}

// withCommon installs the middleware every route shares.
func (a *App) withCommon(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && a.originAllowed(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Credentials", "true")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Uploads have their own limits; everything else is JSON and must not be
		// able to make the server read megabytes. The connector's transfer
		// upload carries raw file bytes and enforces the 256MiB ceiling itself,
		// so the 2MiB JSON cap would truncate it.
		if r.Body != nil && !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") &&
			!isRawUpload(r) {
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
		}
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		a.log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
		)
	})
}

// originAllowed reports whether a cross-origin browser request may read the
// response.
//
// The web client is served from this same origin, so nothing needs CORS in the
// default deployment. Reflecting an arbitrary Origin while also allowing
// credentials would let any site read a signed-in person's data.
func (a *App) originAllowed(origin string) bool {
	for _, allowed := range a.cfg.CORSOrigins {
		if allowed == origin {
			return true
		}
	}
	return false
}

// rawTransferUploadPath is the route registered by registerFileRoutes.
const rawTransferUploadPath = "/api/v2/connector/fs/transfers/"

// isRawUpload reports whether the request is the connector's push upload,
// which carries file bytes rather than JSON. Its handler applies fsMaxPushBytes.
func isRawUpload(r *http.Request) bool {
	return r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, rawTransferUploadPath)
}

// statusRecorder remembers the status for the access log.
//
// It must forward Hijack and Flush: a websocket upgrade hijacks the connection,
// and a wrapper that hides the hijacker turns every upgrade into a 501.
// maxJSONBodyBytes is the ceiling for a non-multipart request body.
const maxJSONBodyBytes = 2 << 20

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("connection does not support hijacking")
	}
	return hijacker.Hijack()
}

func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}
