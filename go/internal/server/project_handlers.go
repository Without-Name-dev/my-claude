package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// ProjectView is a project as the sidebar renders it.
type ProjectView struct {
	ID                   string         `json:"id"`
	UserID               string         `json:"userId"`
	ConnectorID          string         `json:"connectorId"`
	Name                 string         `json:"name"`
	WorkspacePath        string         `json:"workspacePath"`
	ManuallyCreated      bool           `json:"manuallyCreated"`
	Pinned               bool           `json:"pinned"`
	PinnedAt             *string        `json:"pinnedAt"`
	ActiveSessionCount   int            `json:"activeSessionCount"`
	SidebarSessionCounts map[string]int `json:"sidebarSessionCounts"`
	LastActivityAt       *string        `json:"lastActivityAt"`
	CreatedAt            string         `json:"createdAt"`
	UpdatedAt            string         `json:"updatedAt"`
}

// handleListProjects returns every project with its session counts.
func (a *App) handleListProjects(w http.ResponseWriter, r *http.Request, user store.User) {
	views, err := a.projectViews(r.Context(), user.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"projects": views, "serverTime": timestampNow()})
}

// projectViews builds the list once for both the REST call and the dashboard.
func (a *App) projectViews(ctx context.Context, userID string) ([]ProjectView, error) {
	projects, err := a.db.ListProjects(ctx, userID)
	if err != nil {
		return nil, err
	}
	counts, err := a.db.SessionCounts(ctx, userID)
	if err != nil {
		return nil, err
	}
	activity, err := a.db.ProjectLastActivity(ctx, userID)
	if err != nil {
		return nil, err
	}

	out := make([]ProjectView, 0, len(projects))
	for _, p := range projects {
		view := ProjectView{
			ID:                   p.ID,
			UserID:               p.UserID,
			ConnectorID:          p.ConnectorID,
			Name:                 p.Name,
			WorkspacePath:        p.WorkspacePath,
			ManuallyCreated:      p.ManuallyCreated,
			Pinned:               p.Pinned,
			CreatedAt:            isoTime(p.CreatedAt),
			UpdatedAt:            isoTime(p.UpdatedAt),
			SidebarSessionCounts: map[string]int{"active": 0, "archived": 0},
		}
		if counts[p.ID][0] != 0 || counts[p.ID][1] != 0 {
			view.ActiveSessionCount = counts[p.ID][0]
			view.SidebarSessionCounts["active"] = counts[p.ID][0]
			view.SidebarSessionCounts["archived"] = counts[p.ID][1]
		}
		if p.PinnedAt != nil {
			value := isoTime(*p.PinnedAt)
			view.PinnedAt = &value
		}
		if at, ok := activity[p.ID]; ok {
			value := isoTime(at)
			view.LastActivityAt = &value
		}
		out = append(out, view)
	}
	return out, nil
}

// writeProjectNameConflict reports the documented 409 shape, whose code lives
// inside detail rather than in a string.
func writeProjectNameConflict(w http.ResponseWriter) {
	httpx.JSON(w, http.StatusConflict, map[string]any{
		"detail": map[string]any{
			"code":    "project_name_conflict",
			"message": "project name already exists",
		},
	})
}

// handleCreateProject adds a workspace.
func (a *App) handleCreateProject(w http.ResponseWriter, r *http.Request, user store.User) {
	var body struct {
		Name          string `json:"name"`
		ConnectorID   string `json:"connectorId"`
		WorkspacePath string `json:"workspacePath"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	workspace := strings.TrimSpace(body.WorkspacePath)
	if name == "" || len(name) > 255 {
		httpx.Fail(w, httpx.BadRequest("name must be 1..255 characters"))
		return
	}
	if workspace == "" {
		httpx.Fail(w, httpx.BadRequest("workspacePath is required"))
		return
	}
	connector, err := a.db.ConnectorByID(r.Context(), body.ConnectorID)
	if err != nil || connector.UserID != user.ID || connector.Revoked != 0 {
		httpx.Fail(w, httpx.NotFound("connector not found"))
		return
	}

	existing, err := a.db.EnsureProject(r.Context(), user.ID, body.ConnectorID, workspace, name, true)
	if err != nil {
		if errors.Is(err, store.ErrProjectNameConflict) {
			writeProjectNameConflict(w)
			return
		}
		httpx.Fail(w, err)
		return
	}
	views, err := a.projectViews(r.Context(), user.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	for _, view := range views {
		if view.ID == existing.ID {
			httpx.OK(w, map[string]any{
				"project":          view,
				"attachedSessions": 0,
				"serverTime":       timestampNow(),
			})
			return
		}
	}
	httpx.Fail(w, httpx.NotFound("project not found"))
}

// handlePatchProject renames or pins a project.
func (a *App) handlePatchProject(w http.ResponseWriter, r *http.Request, user store.User) {
	project, ok := a.ownedProject(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Name   *string `json:"name"`
		Pinned *bool   `json:"pinned"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if body.Name == nil && body.Pinned == nil {
		httpx.Fail(w, httpx.BadRequest("nothing to update"))
		return
	}
	if body.Name != nil {
		trimmed := strings.TrimSpace(*body.Name)
		if trimmed == "" || len(trimmed) > 255 {
			httpx.Fail(w, httpx.BadRequest("name must be 1..255 characters"))
			return
		}
		body.Name = &trimmed
	}
	if err := a.db.UpdateProject(r.Context(), project.ID, body.Name, body.Pinned); err != nil {
		if errors.Is(err, store.ErrProjectNameConflict) {
			writeProjectNameConflict(w)
			return
		}
		httpx.Fail(w, err)
		return
	}
	views, err := a.projectViews(r.Context(), user.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	for _, view := range views {
		if view.ID == project.ID {
			httpx.OK(w, map[string]any{"project": view, "serverTime": timestampNow()})
			return
		}
	}
	httpx.Fail(w, httpx.NotFound("project not found"))
}

// handleDeleteProject removes a project without touching its sessions.
func (a *App) handleDeleteProject(w http.ResponseWriter, r *http.Request, user store.User) {
	project, ok := a.ownedProject(w, r, user)
	if !ok {
		return
	}
	detached, err := a.db.DeleteProject(r.Context(), project.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			httpx.Fail(w, httpx.NotFound("project not found"))
			return
		}
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"projectId":        project.ID,
		"detachedSessions": detached,
		"serverTime":       timestampNow(),
	})
}

// handleProjectSessions lists the sessions in one project.
func (a *App) handleProjectSessions(w http.ResponseWriter, r *http.Request, user store.User) {
	project, ok := a.ownedProject(w, r, user)
	if !ok {
		return
	}
	archived := r.URL.Query().Get("archived") == "true"
	// Select the project's own sessions, not the newest hundred of the whole
	// account with the rest dropped: past a hundred sessions a project's
	// conversations simply disappeared from this list.
	all, err := a.db.ListAllSessions(r.Context(), user.ID, archived)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	sessions := make([]store.Session, 0, len(all))
	for _, s := range all {
		if s.ProjectID == project.ID {
			sessions = append(sessions, s)
		}
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].Pinned != sessions[j].Pinned {
			return sessions[i].Pinned
		}
		if sessions[i].SortAt != sessions[j].SortAt {
			return sessions[i].SortAt > sessions[j].SortAt
		}
		return sessions[i].CreatedAt > sessions[j].CreatedAt
	})
	hasMore := len(sessions) > 100
	if hasMore {
		sessions = sessions[:100]
	}
	views := make([]SessionView, 0, len(sessions))
	for _, s := range sessions {
		views = append(views, a.sessionView(r.Context(), s))
	}
	httpx.OK(w, map[string]any{
		"sessions":   views,
		"hasMore":    hasMore,
		"nextCursor": nil,
		"serverTime": timestampNow(),
	})
}

func (a *App) ownedProject(w http.ResponseWriter, r *http.Request, user store.User) (store.Project, bool) {
	project, err := a.db.ProjectByID(r.Context(), r.PathValue("project_id"), user.ID)
	if err != nil {
		httpx.Fail(w, httpx.NotFound("project not found"))
		return store.Project{}, false
	}
	return project, true
}
