package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// Public share snapshots and the archive-all routes live here because both are
// dashboard conveniences over an existing session, not session-protocol
// operations.

// registerShareRoutes installs share creation/read/deletion and the two
// archive-all entry points.
func (a *App) registerShareRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/shares",
		a.requireUser(a.handleCreateSessionShare))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/shares",
		a.requireUser(a.handleListSessionShares))
	mux.HandleFunc("DELETE /api/v2/sessions/{session_id}/shares/{share_id}",
		a.requireUser(a.handleDeleteSessionShare))

	// The shipped web client and the specification call the public read
	// /public/shares/{id}; the acceptance checklist calls /shares/{id}. Serving
	// both costs one route and keeps either client working.
	mux.HandleFunc("GET /api/v2/shares/{share_id}", a.handlePublicSessionShare)
	mux.HandleFunc("GET /api/v2/public/shares/{share_id}", a.handlePublicSessionShare)

	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/sessions/archive-all",
		a.requireUser(a.handleConnectorArchiveAll))
	mux.HandleFunc("POST /api/v2/sessions/archive-all",
		a.requireUser(a.handleAllSessionsArchiveAll))
}

// registerAdminUserEditRoutes installs the two user-management mutations that
// app.go mounts next to registerAdminRoutes.
func (a *App) registerAdminUserEditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("PATCH /api/v2/admin/users/{user_id}",
		a.requireUser(a.adminOnly(a.handleAdminUpdateUser)))
	mux.HandleFunc("DELETE /api/v2/admin/users/{user_id}",
		a.requireUser(a.adminOnly(a.handleAdminDeleteUser)))
}

// --- shares ------------------------------------------------------------------

// sharedSessionInfo is the session facts embedded in a snapshot. The shape
// matches PublicSessionShareResponse.session; nullable values stay explicit
// nulls so the web page can render them without special cases.
type sharedSessionInfo struct {
	ID          string  `json:"id"`
	Title       *string `json:"title"`
	Runtime     string  `json:"runtime"`
	RuntimeName *string `json:"runtimeName"`
	CWD         *string `json:"cwd"`
}

// sessionShareSnapshot is the JSON document stored in
// session_shares.snapshot_json. Items are frozen at creation time, so editing
// the session later cannot rewrite a link that was already shared.
type sessionShareSnapshot struct {
	Session sharedSessionInfo `json:"session"`
	Items   []ItemView        `json:"items"`
}

// sessionShareCreateResponse mirrors SessionShareCreateResponse in the spec.
type sessionShareCreateResponse struct {
	ShareID   string `json:"shareId"`
	SharePath string `json:"sharePath"`
	ShareURL  string `json:"shareUrl"`
	Scope     string `json:"scope"`
	CreatedAt string `json:"createdAt"`
}

// sessionShareListItem is one entry in the owner-only share list. The
// specification has no list endpoint, so the fields mirror the create response
// plus nothing else: the list is only used to manage existing links.
type sessionShareListItem struct {
	ShareID   string `json:"shareId"`
	Scope     string `json:"scope"`
	SharePath string `json:"sharePath"`
	ShareURL  string `json:"shareUrl"`
	CreatedAt string `json:"createdAt"`
}

// publicSessionShareResponse mirrors PublicSessionShareResponse.
type publicSessionShareResponse struct {
	ShareID    string            `json:"shareId"`
	Scope      string            `json:"scope"`
	Session    sharedSessionInfo `json:"session"`
	Items      []ItemView        `json:"items"`
	CreatedAt  string            `json:"createdAt"`
	ServerTime string            `json:"serverTime"`
}

// handleCreateSessionShare freezes part of a conversation.
//
// "message" selects the assistant messages the client asked for (or every
// assistant message when it only sends a scope); "session" takes the whole
// timeline. itemIds is accepted for the shipped web client even though the
// acceptance contract only requires scope.
func (a *App) handleCreateSessionShare(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Scope   string   `json:"scope"`
		ItemIDs []string `json:"itemIds"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	scope := strings.TrimSpace(body.Scope)
	if scope != "message" && scope != "session" {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
			`scope must be "message" or "session"`))
		return
	}
	if len(body.ItemIDs) > 100 {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
			"itemIds must contain at most 100 items"))
		return
	}
	seen := make(map[string]struct{}, len(body.ItemIDs))
	for _, id := range body.ItemIDs {
		if id == "" {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
				"itemIds must not contain empty ids"))
			return
		}
		if _, duplicate := seen[id]; duplicate {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
				"itemIds must not contain duplicates"))
			return
		}
		seen[id] = struct{}{}
	}
	if scope == "session" && len(body.ItemIDs) > 0 {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
			"a session share must not select itemIds"))
		return
	}

	var shared []store.TimelineItem
	if scope == "message" && len(body.ItemIDs) > 0 {
		// Read each selected item by id: a long conversation may hold the
		// requested reply outside the newest timeline page.
		for _, id := range body.ItemIDs {
			item, err := a.db.TimelineItemByID(r.Context(), a.db.Read(), session.ID, id)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					httpx.Fail(w, httpx.NotFound("share item not found"))
					return
				}
				httpx.Fail(w, err)
				return
			}
			if item.Type != "message" || item.Role != "assistant" {
				httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
					"message shares may only contain assistant messages"))
				return
			}
			shared = append(shared, item)
		}
	} else {
		items, err := a.db.TimelineItems(r.Context(), a.db.Read(), session.ID, 2000)
		if err != nil {
			httpx.Fail(w, err)
			return
		}
		if scope == "message" {
			for _, item := range items {
				if item.Type == "message" && item.Role == "assistant" {
					shared = append(shared, item)
				}
			}
			// A link created before the first reply arrives is still a valid
			// snapshot. Falling back to any message (and finally to an empty
			// list) keeps the acceptance contract, which only sends a scope,
			// from turning into a 422 on a fresh conversation.
			if len(shared) == 0 {
				for _, item := range items {
					if item.Type == "message" {
						shared = append(shared, item)
					}
				}
			}
		} else {
			shared = items
		}
	}

	views := make([]ItemView, 0, len(shared))
	for _, item := range shared {
		view := toView(item, session.Runtime)
		if !json.Valid(view.Content) {
			view.Content = json.RawMessage(`{}`)
		}
		views = append(views, view)
	}
	snapshot, err := json.Marshal(sessionShareSnapshot{
		Session: sharedSessionInfoFrom(session),
		Items:   views,
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	share, err := a.db.CreateSessionShare(r.Context(), store.SessionShare{
		UserID:       user.ID,
		SessionID:    session.ID,
		Scope:        scope,
		SnapshotJSON: string(snapshot),
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, sessionShareCreateResponse{
		ShareID:   share.ID,
		SharePath: sharePathFor(share.ID),
		ShareURL:  a.shareURLFor(r, share.ID),
		Scope:     share.Scope,
		CreatedAt: isoTime(share.CreatedAt),
	})
}

// handleListSessionShares returns the caller's links for one session.
func (a *App) handleListSessionShares(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	shares, err := a.db.ListSessionShares(r.Context(), session.ID, user.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	views := make([]sessionShareListItem, 0, len(shares))
	for _, share := range shares {
		views = append(views, sessionShareListItem{
			ShareID:   share.ID,
			Scope:     share.Scope,
			SharePath: sharePathFor(share.ID),
			ShareURL:  a.shareURLFor(r, share.ID),
			CreatedAt: isoTime(share.CreatedAt),
		})
	}
	httpx.OK(w, map[string]any{"shares": views, "serverTime": timestampNow()})
}

// handleDeleteSessionShare revokes a link. A link owned by somebody else is
// reported as 404 by the store rather than 403.
func (a *App) handleDeleteSessionShare(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	err := a.db.DeleteSessionShare(
		r.Context(), r.PathValue("share_id"), session.ID, user.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.Fail(w, httpx.NotFound("share not found"))
			return
		}
		httpx.Fail(w, err)
		return
	}
	httpx.NoContent(w)
}

// handlePublicSessionShare is unauthenticated by design: the unguessable share
// id is the credential.
func (a *App) handlePublicSessionShare(w http.ResponseWriter, r *http.Request) {
	share, err := a.db.SessionShareByID(r.Context(), r.PathValue("share_id"))
	if err != nil {
		httpx.Fail(w, httpx.NotFound("share not found"))
		return
	}
	var snapshot sessionShareSnapshot
	if err := json.Unmarshal([]byte(share.SnapshotJSON), &snapshot); err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "share snapshot is corrupt"))
		return
	}
	if snapshot.Items == nil {
		snapshot.Items = make([]ItemView, 0)
	}
	httpx.OK(w, publicSessionShareResponse{
		ShareID:    share.ID,
		Scope:      share.Scope,
		Session:    snapshot.Session,
		Items:      snapshot.Items,
		CreatedAt:  isoTime(share.CreatedAt),
		ServerTime: timestampNow(),
	})
}

func sharedSessionInfoFrom(session store.Session) sharedSessionInfo {
	info := sharedSessionInfo{ID: session.ID, Runtime: session.Runtime}
	if session.Title != "" {
		title := session.Title
		info.Title = &title
	}
	if session.CWD != "" {
		cwd := session.CWD
		info.CWD = &cwd
	}
	return info
}

func sharePathFor(shareID string) string { return "/share/" + shareID }

// shareURLFor builds the absolute link. AA_PUBLIC_URL is authoritative because
// a reverse proxy usually rewrites Host; the request only supplies a fallback
// for local development.
func (a *App) shareURLFor(r *http.Request, shareID string) string {
	base := strings.TrimRight(a.cfg.PublicURL, "/")
	if base == "" {
		base = strings.TrimRight(requestEndpoint(r), "/")
	}
	path := sharePathFor(shareID)
	if base == "" {
		return path
	}
	return base + path
}

// --- archive-all -------------------------------------------------------------

// archiveAllRequest is ArchiveAllRequest in the specification.
type archiveAllRequest struct {
	Archived *bool  `json:"archived"`
	Scope    string `json:"scope"`
}

func decodeArchiveAll(w http.ResponseWriter, r *http.Request) (bool, string, bool) {
	var body archiveAllRequest
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return false, "", false
	}
	if body.Archived == nil {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "archived is required"))
		return false, "", false
	}
	scope := strings.TrimSpace(body.Scope)
	if scope == "" {
		scope = "active"
	}
	if scope != "active" && scope != "archived" && scope != "all" {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
			`scope must be "active", "archived" or "all"`))
		return false, "", false
	}
	return *body.Archived, scope, true
}

// handleConnectorArchiveAll archives or restores one device's sessions.
func (a *App) handleConnectorArchiveAll(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	archived, scope, ok := decodeArchiveAll(w, r)
	if !ok {
		return
	}
	sessions, err := a.db.SetSessionsArchivedWithScope(
		r.Context(), user.ID, connector.ID, archived, scope)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	a.writeArchiveAll(w, r, sessions)
}

// handleAllSessionsArchiveAll is the cross-device variant.
func (a *App) handleAllSessionsArchiveAll(w http.ResponseWriter, r *http.Request, user store.User) {
	archived, scope, ok := decodeArchiveAll(w, r)
	if !ok {
		return
	}
	sessions, err := a.db.SetSessionsArchivedWithScope(
		r.Context(), user.ID, "", archived, scope)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	a.writeArchiveAll(w, r, sessions)
}

func (a *App) writeArchiveAll(w http.ResponseWriter, r *http.Request, sessions []store.Session) {
	views := make([]SessionView, 0, len(sessions))
	for _, session := range sessions {
		views = append(views, a.sessionView(r.Context(), session))
	}
	httpx.OK(w, map[string]any{
		"sessions":   views,
		"affected":   len(views),
		"serverTime": timestampNow(),
	})
}
