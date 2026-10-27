package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

const commandQueueAckTimeout = 10 * time.Second

type pendingCommandAck struct {
	connectorID       string
	sessionID         string
	externalSessionID string
	response          chan protocol.CommandAck
}

func (a *App) registerCommandAck(connectorID, sessionID, externalSessionID, requestID string) chan protocol.CommandAck {
	response := make(chan protocol.CommandAck, 1)
	a.commandAckMu.Lock()
	// Limit pending commands to prevent unbounded growth
	if len(a.commandAcks) >= 1000 {
		a.commandAckMu.Unlock()
		return nil
	}
	a.commandAcks[requestID] = pendingCommandAck{
		connectorID: connectorID, sessionID: sessionID,
		externalSessionID: externalSessionID, response: response,
	}
	a.commandAckMu.Unlock()
	return response
}

func (a *App) removeCommandAck(requestID string) {
	a.commandAckMu.Lock()
	delete(a.commandAcks, requestID)
	a.commandAckMu.Unlock()
}

func (a *App) applyCommandAck(conn *connectorConn, ack protocol.CommandAck) {
	if ack.RequestID == "" || ack.SessionID == "" {
		return
	}
	a.commandAckMu.Lock()
	pending, ok := a.commandAcks[ack.RequestID]
	if ok && pending.connectorID == conn.id && pending.sessionID == ack.SessionID &&
		(pending.externalSessionID == "" || pending.externalSessionID == ack.ExternalSessionID) {
		select {
		case pending.response <- ack:
		default:
		}
	}
	a.commandAckMu.Unlock()
}

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

	httpx.OK(w, map[string]any{"commands": commandViews(query, limit), "serverTime": timestampNow()})
}

// commandViews renders the built-in menu, filtered by id or title.
func commandViews(query string, limit int) []RuntimeCommandView {
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
	return out
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
		raw := strings.TrimSpace(*body.Raw)
		// Clients that send the raw line include the command itself. Prefixing
		// it again produced "/compact /compact …", which the plugin then ran as
		// a different command line.
		first, _, _ := strings.Cut(raw, " ")
		if strings.TrimPrefix(first, "/") == name {
			line = raw
		} else {
			line += " " + *body.Raw
		}
	} else if len(body.Args) > 0 {
		line += " " + strings.Join(body.Args, " ")
	}

	// A command is only run by a plugin that is already serving this exact
	// conversation: the connector queues it without a watcher, so there is
	// nothing to report and nothing to wait for when no plugin is live. Saying
	// "queued" to a caller whose command will never run is a dead end.
	if !a.connectors.Live(session.ConnectorID, session.ExternalID) {
		httpx.Fail(w, httpx.Conflict("the conversation is not live on the desktop; open it there to run commands"))
		return
	}
	requestID := store.NewID("cmd")
	ack := a.registerCommandAck(session.ConnectorID, session.ID, session.ExternalID, requestID)
	if ack == nil {
		httpx.Fail(w, httpx.TooManyRequests("too many pending commands"))
		return
	}
	defer a.removeCommandAck(requestID)
	sent := a.connectors.Send(session.ConnectorID, protocol.TypeSubmit, protocol.Submit{
		RequestID:         requestID,
		SessionID:         session.ID,
		ExternalSessionID: session.ExternalID,
		Text:              line,
		Action:            "command",
	})
	if !sent {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	select {
	case result := <-ack:
		if !result.Accepted {
			message := strings.TrimSpace(result.Error)
			if message == "" {
				message = "the desktop rejected the command"
			}
			httpx.Fail(w, httpx.Conflict(message))
			return
		}
	case <-time.After(commandQueueAckTimeout):
		httpx.OK(w, map[string]any{
			"command":    name,
			"ok":         false,
			"code":       "command_outcome_unknown",
			"message":    "the desktop may have accepted the command, but did not confirm it in time; check the terminal before retrying",
			"result":     map[string]any{"executionState": "unknown", "retryable": false},
			"session":    nil,
			"serverTime": timestampNow(),
		})
		return
	case <-r.Context().Done():
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
