package server

import (
	"net/http"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// handleGetSessionConfig handles GET /api/v2/sessions/:id/config
func (a *App) handleGetSessionConfig(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	sessionID := r.PathValue("id")

	cfg, err := a.db.GetSessionConfig(ctx, sessionID)
	if err != nil {
		a.log.Error("failed to get session config", "error", err)
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "Failed to get config"))
		return
	}

	httpx.JSON(w, http.StatusOK, cfg)
}

// handleUpdateSessionConfig handles PATCH /api/v2/sessions/:id/config
func (a *App) handleUpdateSessionConfig(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	sessionID := r.PathValue("id")

	var req struct {
		EffortLevel *string `json:"effortLevel,omitempty"`
		ModelName   *string `json:"modelName,omitempty"`
		FastMode    *bool   `json:"fastMode,omitempty"`
		AutoMode    *bool   `json:"autoMode,omitempty"`
	}

	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.Fail(w, httpx.BadRequest("Invalid request body"))
		return
	}

	// Get current config
	cfg, err := a.db.GetSessionConfig(ctx, sessionID)
	if err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "Failed to get config"))
		return
	}

	// Update fields
	if req.EffortLevel != nil {
		if !ValidateEffort(*req.EffortLevel) {
			httpx.Fail(w, httpx.BadRequest("Invalid effort level"))
			return
		}
		cfg.EffortLevel = *req.EffortLevel
	}

	if req.ModelName != nil {
		if !ValidateModel(*req.ModelName) {
			httpx.Fail(w, httpx.BadRequest("Invalid model name"))
			return
		}
		cfg.ModelName = NormalizeModel(*req.ModelName)
	}

	if req.FastMode != nil {
		cfg.FastMode = *req.FastMode
	}

	if req.AutoMode != nil {
		cfg.AutoMode = *req.AutoMode
	}

	cfg.UpdatedAt = time.Now()

	if err := a.db.UpsertSessionConfig(ctx, *cfg); err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "Failed to update config"))
		return
	}

	// Publish config change event
	frame, err := newEnvelope(sessionID, 0, "config.updated", map[string]any{
		"config": cfg,
	})
	if err == nil {
		a.broker.Publish(sessionID, frame)
	}

	httpx.JSON(w, http.StatusOK, cfg)
}

// handleGetAgents handles GET /api/v2/sessions/:id/agents
func (a *App) handleGetAgents(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	sessionID := r.PathValue("id")

	agents, err := a.db.GetAgentExecutions(ctx, sessionID)
	if err != nil {
		a.log.Error("failed to get agents", "error", err)
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "Failed to get agents"))
		return
	}

	if agents == nil {
		agents = []store.AgentExecution{}
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"agents": agents,
	})
}

// handleGetWorkflows handles GET /api/v2/sessions/:id/workflows
func (a *App) handleGetWorkflows(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	sessionID := r.PathValue("id")

	workflows, err := a.db.GetWorkflowRuns(ctx, sessionID)
	if err != nil {
		a.log.Error("failed to get workflows", "error", err)
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "Failed to get workflows"))
		return
	}

	if workflows == nil {
		workflows = []store.WorkflowRun{}
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"workflows": workflows,
	})
}
