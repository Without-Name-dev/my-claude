package server

import (
	"net/http"
	"strings"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// RuntimeCommandView is one entry in the slash-command menu.
type RuntimeCommandView struct {
	ID             string         `json:"id"`
	Title          string         `json:"title"`
	Description    *string        `json:"description"`
	Aliases        []string       `json:"aliases"`
	Category       *string        `json:"category"`
	Scope          string         `json:"scope"`
	Enabled        bool           `json:"enabled"`
	DisabledReason *string        `json:"disabledReason"`
	AcceptsArgs    bool           `json:"acceptsArgs"`
	ArgsSchema     map[string]any `json:"argsSchema"`
	Metadata       map[string]any `json:"metadata"`
}

// builtinCommands is the menu the phone shows.
//
// Claude Code resolves the real list inside the session, and the plugin runs
// whatever it is given — so this catalogue is presentation only. It is kept
// here rather than fetched from the connector because the connector has no
// channel into the plugin, and an empty menu would be worse than one that can
// lag a release behind.
var builtinCommands = []struct {
	id         string
	title      string
	desc       string
	category   string
	acceptsArg bool
}{
	{"clear", "Clear conversation", "Start over with an empty context", "Session", false},
	{"compact", "Compact conversation", "Summarise the thread to free context", "Session", false},
	{"context", "Show context usage", "See what is filling the context window", "Session", false},
	{"cost", "Show cost", "Token usage for this session", "Session", false},
	{"model", "Switch model", "Change the model for this session", "Configuration", true},
	{"config", "Open settings", "Review and edit configuration", "Configuration", false},
	{"permissions", "Permissions", "Review tool permissions", "Configuration", false},
	{"status", "Status", "Session, account and model status", "Session", false},
	{"todos", "Show todos", "The current task list", "Session", false},
	{"review", "Review a pull request", "Review changes on a branch or PR", "Workflow", true},
	{"init", "Initialise project", "Create a CLAUDE.md for this repo", "Workflow", false},
	{"memory", "Edit memory", "Open the CLAUDE.md memory files", "Configuration", false},
	{"mcp", "MCP servers", "Inspect connected MCP servers", "Configuration", false},
	{"agents", "Agents", "Manage subagents", "Configuration", false},
	{"hooks", "Hooks", "Manage session hooks", "Configuration", false},
	{"export", "Export conversation", "Save this transcript", "Session", true},
	{"help", "Help", "List available commands", "Session", false},
}

// handleListCommands returns the menu, filtered by the optional query.
func (a *App) handleListCommands(w http.ResponseWriter, r *http.Request, user store.User) {
	if _, ok := a.ownedSession(w, r, user); !ok {
		return
	}
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("query")))
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, ok := atoi(raw); ok && n > 0 && n <= 1000 {
			limit = int(n)
		}
	}

	out := make([]RuntimeCommandView, 0, len(builtinCommands))
	for _, command := range builtinCommands {
		if query != "" && !strings.Contains(command.id, query) &&
			!strings.Contains(strings.ToLower(command.title), query) {
			continue
		}
		description := command.desc
		category := command.category
		out = append(out, RuntimeCommandView{
			ID:          command.id,
			Title:       command.title,
			Description: &description,
			Aliases:     []string{},
			Category:    &category,
			Scope:       "session",
			Enabled:     true,
			AcceptsArgs: command.acceptsArg,
			ArgsSchema:  map[string]any{},
			Metadata:    map[string]any{},
		})
		if len(out) >= limit {
			break
		}
	}
	httpx.OK(w, map[string]any{"commands": out, "serverTime": timestampNow()})
}

// handleRunCommand executes a slash command through the connector.
func (a *App) handleRunCommand(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Command string   `json:"command"`
		Args    []string `json:"args"`
		Raw     *string  `json:"raw"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	name := strings.TrimPrefix(strings.TrimSpace(body.Command), "/")
	if name == "" || len(name) > 64 {
		httpx.Fail(w, httpx.BadRequest("command is required"))
		return
	}
	if !a.connectors.Online(session.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}

	line := "/" + name
	if body.Raw != nil && *body.Raw != "" {
		line += " " + *body.Raw
	} else if len(body.Args) > 0 {
		line += " " + strings.Join(body.Args, " ")
	}

	sent := a.connectors.Send(session.ConnectorID, protocol.TypeSubmit, protocol.Submit{
		RequestID: store.NewID("cmd"),
		SessionID: session.ID,
		Text:      line,
		Action:    "command",
	})
	if !sent {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	httpx.OK(w, map[string]any{
		"command":    name,
		"ok":         true,
		"code":       nil,
		"message":    nil,
		"result":     map[string]any{"queued": true},
		"session":    nil,
		"serverTime": timestampNow(),
	})
}
