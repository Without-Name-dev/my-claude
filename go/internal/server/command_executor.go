package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// handleExecuteCommand handles POST /api/v2/sessions/:id/commands/execute
func (a *App) handleExecuteCommand(w http.ResponseWriter, r *http.Request, user store.User) {
	ctx := r.Context()
	sessionID := r.PathValue("id")

	var req struct {
		Command string `json:"command"`
	}

	if err := httpx.ReadJSON(r, &req); err != nil {
		httpx.Fail(w, httpx.BadRequest("Invalid request body"))
		return
	}

	// Parse command
	cmd, err := ParseCommand(req.Command)
	if err != nil {
		httpx.Fail(w, httpx.BadRequest(err.Error()))
		return
	}

	// Execute command based on type
	result, err := a.executeCommand(ctx, sessionID, cmd)
	if err != nil {
		httpx.Fail(w, httpx.NewError(http.StatusBadRequest, err.Error()))
		return
	}

	httpx.JSON(w, http.StatusOK, map[string]any{
		"command": cmd.Name,
		"result":  result,
	})
}

// executeCommand executes a parsed command
func (a *App) executeCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	switch cmd.Type() {
	case CommandEffort:
		return a.executeEffortCommand(ctx, sessionID, cmd)
	case CommandModel:
		return a.executeModelCommand(ctx, sessionID, cmd)
	case CommandFast:
		return a.executeFastCommand(ctx, sessionID, cmd)
	case CommandAutoMode:
		return a.executeAutoModeCommand(ctx, sessionID, cmd)
	case CommandCompact:
		return a.executeCompactCommand(ctx, sessionID, cmd)
	case CommandClear:
		return a.executeClearCommand(ctx, sessionID, cmd)
	case CommandHelp:
		return a.executeHelpCommand(ctx, sessionID, cmd)
	case CommandConfig:
		return a.executeConfigCommand(ctx, sessionID, cmd)
	case CommandWorkflows:
		return a.executeWorkflowsCommand(ctx, sessionID, cmd)
	default:
		return nil, fmt.Errorf("unknown command: %s", cmd.Name)
	}
}

// executeEffortCommand handles /effort <level>
func (a *App) executeEffortCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("effort level required: /effort <low|medium|high|xhigh|max|auto>")
	}

	level := strings.ToLower(cmd.Args[0])
	if !ValidateEffort(level) {
		return nil, fmt.Errorf("invalid effort level: %s (valid: low, medium, high, xhigh, max, auto)", level)
	}

	// Get current config
	cfg, err := a.db.GetSessionConfig(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session config: %w", err)
	}

	// Update effort level
	cfg.EffortLevel = level
	cfg.UpdatedAt = time.Now()

	if err := a.db.UpsertSessionConfig(ctx, *cfg); err != nil {
		return nil, fmt.Errorf("failed to update effort level: %w", err)
	}

	// Publish config change event
	a.publishConfigChange(sessionID, "effort", level)

	return map[string]any{
		"effortLevel": level,
		"message":     fmt.Sprintf("Effort level set to %s", level),
	}, nil
}

// executeModelCommand handles /model <name>
func (a *App) executeModelCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	if len(cmd.Args) == 0 {
		return nil, fmt.Errorf("model name required: /model <opus|sonnet|haiku>")
	}

	model := strings.ToLower(cmd.Args[0])
	if !ValidateModel(model) {
		return nil, fmt.Errorf("invalid model: %s (valid: opus, sonnet, haiku)", model)
	}

	// Normalize model name
	normalizedModel := NormalizeModel(model)

	// Get current config
	cfg, err := a.db.GetSessionConfig(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session config: %w", err)
	}

	// Update model
	cfg.ModelName = normalizedModel
	cfg.UpdatedAt = time.Now()

	if err := a.db.UpsertSessionConfig(ctx, *cfg); err != nil {
		return nil, fmt.Errorf("failed to update model: %w", err)
	}

	// Publish config change event
	a.publishConfigChange(sessionID, "model", normalizedModel)

	return map[string]any{
		"modelName": normalizedModel,
		"message":   fmt.Sprintf("Model switched to %s", normalizedModel),
	}, nil
}

// executeFastCommand handles /fast [on|off]
func (a *App) executeFastCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	// Get current config
	cfg, err := a.db.GetSessionConfig(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session config: %w", err)
	}

	// Toggle or set fast mode
	if len(cmd.Args) == 0 {
		cfg.FastMode = !cfg.FastMode
	} else {
		arg := strings.ToLower(cmd.Args[0])
		if arg == "on" || arg == "true" || arg == "1" {
			cfg.FastMode = true
		} else if arg == "off" || arg == "false" || arg == "0" {
			cfg.FastMode = false
		} else {
			return nil, fmt.Errorf("invalid argument: %s (valid: on, off)", arg)
		}
	}

	cfg.UpdatedAt = time.Now()

	if err := a.db.UpsertSessionConfig(ctx, *cfg); err != nil {
		return nil, fmt.Errorf("failed to update fast mode: %w", err)
	}

	// Publish config change event
	a.publishConfigChange(sessionID, "fastMode", cfg.FastMode)

	status := "off"
	if cfg.FastMode {
		status = "on"
	}

	return map[string]any{
		"fastMode": cfg.FastMode,
		"message":  fmt.Sprintf("Fast mode %s", status),
	}, nil
}

// executeAutoModeCommand handles /auto mode <on|off>
func (a *App) executeAutoModeCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	if len(cmd.Args) < 2 || strings.ToLower(cmd.Args[0]) != "mode" {
		return nil, fmt.Errorf("usage: /auto mode <on|off>")
	}

	// Get current config
	cfg, err := a.db.GetSessionConfig(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session config: %w", err)
	}

	arg := strings.ToLower(cmd.Args[1])
	if arg == "on" || arg == "true" || arg == "1" {
		cfg.AutoMode = true
	} else if arg == "off" || arg == "false" || arg == "0" {
		cfg.AutoMode = false
	} else {
		return nil, fmt.Errorf("invalid argument: %s (valid: on, off)", arg)
	}

	cfg.UpdatedAt = time.Now()

	if err := a.db.UpsertSessionConfig(ctx, *cfg); err != nil {
		return nil, fmt.Errorf("failed to update auto mode: %w", err)
	}

	// Publish config change event
	a.publishConfigChange(sessionID, "autoMode", cfg.AutoMode)

	status := "off"
	if cfg.AutoMode {
		status = "on"
	}

	return map[string]any{
		"autoMode": cfg.AutoMode,
		"message":  fmt.Sprintf("Auto mode %s", status),
	}, nil
}

// executeCompactCommand handles /compact
func (a *App) executeCompactCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	// TODO: Implement conversation history compaction
	// This would typically involve summarizing old messages and removing them
	return map[string]any{
		"message": "Conversation compacted (feature not yet implemented)",
	}, nil
}

// executeClearCommand handles /clear
func (a *App) executeClearCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	// TODO: Implement session clearing
	// This would archive current timeline and start fresh
	return map[string]any{
		"message": "Session cleared (feature not yet implemented)",
	}, nil
}

// executeHelpCommand handles /help
func (a *App) executeHelpCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	help := GetCommandHelp()
	return map[string]any{
		"commands": help,
		"message":  "Available commands",
	}, nil
}

// executeConfigCommand handles /config
func (a *App) executeConfigCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	cfg, err := a.db.GetSessionConfig(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get session config: %w", err)
	}

	return map[string]any{
		"config":  cfg,
		"message": "Current configuration",
	}, nil
}

// executeWorkflowsCommand handles /workflows
func (a *App) executeWorkflowsCommand(ctx context.Context, sessionID string, cmd *Command) (map[string]any, error) {
	workflows, err := a.db.GetWorkflowRuns(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("failed to get workflows: %w", err)
	}

	return map[string]any{
		"workflows": workflows,
		"message":   fmt.Sprintf("Found %d workflow runs", len(workflows)),
	}, nil
}

// publishConfigChange publishes a config change event via WebSocket
func (a *App) publishConfigChange(sessionID, key string, value any) {
	frame, err := newEnvelope(sessionID, 0, "config.changed", map[string]any{
		"key":   key,
		"value": value,
	})
	if err == nil {
		a.broker.Publish(sessionID, frame)
	}
}
