// Package protocol defines the wire types shared by the server and the
// connector.
//
// This file is the connector link: one websocket per connector, carrying both
// directions over a small envelope. Both ends ship together, so unlike the
// client-facing API these shapes are free to change — but they are versioned
// anyway, because a connector left running on someone's desktop updates on its
// own schedule and must not be talked to in a format it does not know.
package protocol

import "encoding/json"

// ConnectorProtocolVersion is bumped when a change would make an older
// connector misinterpret a frame. Additive fields do not bump it.
const ConnectorProtocolVersion = 1

// Envelope wraps every connector frame.
type Envelope struct {
	Type string          `json:"t"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Connector → server.
const (
	// TypeHello is the first frame after the socket opens. The server refuses
	// any other frame before it.
	TypeHello = "hello"
	// TypeBeat proves the connector is alive; the server marks it offline when
	// three intervals pass without one.
	TypeBeat = "beat"
	// TypeTurnBegin announces that a turn started, before any text.
	TypeTurnBegin = "turn.begin"
	// TypeTurnDelta carries newly produced text for a running turn.
	TypeTurnDelta = "turn.delta"
	// TypeTurnEnd closes a turn with its final text and the facts worth keeping.
	TypeTurnEnd = "turn.end"
	// TypeToolBegin and TypeToolEnd bracket one tool call.
	TypeToolBegin = "tool.begin"
	TypeToolEnd   = "tool.end"
	// TypeApprovalRequest asks the person to allow or reject a tool call.
	TypeApprovalRequest = "approval.request"
	// TypeApprovalCancel retracts a pending question.
	TypeApprovalCancel = "approval.cancel"
)

// Server → connector.
const (
	// TypeSubmit asks the connector to raise a turn in the addressed session.
	TypeSubmit = "submit"
	// TypeAbort asks it to cancel the running turn.
	TypeAbort = "abort"
	// TypeApprovalAnswer delivers the person's decision.
	TypeApprovalAnswer = "approval.answer"
	// TypeSessionOpen asks the connector to bring a conversation up in a
	// terminal on its machine, so a phone message sent to a conversation with
	// no running plugin still has somewhere to land.
	TypeSessionOpen = "session.open"
	// TypeSessionClose asks the connector to stop the terminal it is running
	// for one conversation, so a person can restart it from the phone.
	TypeSessionClose = "session.close"
	// TypePing is a liveness probe; the connector answers with TypeBeat.
	TypePing = "ping"
)

// Capability names a connector may advertise in Hello.Capabilities.
const (
	// CapabilityBridge reports that a phone-pane plugin is answering now, so a
	// queued submit would be picked up.
	CapabilityBridge = "bridge"
	// CapabilityBridgeConfigured reports that this connector has a bridge
	// directory at all, even if no plugin is answering right now. A submit can
	// be queued only when this is true; a terminal spawned on a connector
	// without it would have nothing to read the message.
	CapabilityBridgeConfigured = "bridgeConfigured"
	// CapabilitySessionOpen reports that this connector can start a terminal
	// running Claude Code. An older connector omits it and must not be sent a
	// session.open frame it would silently ignore.
	CapabilitySessionOpen = "sessionOpen"
)

// Hello is the connector's introduction.
type Hello struct {
	Version      int             `json:"version"`
	ConnectorID  string          `json:"connectorId"`
	Name         string          `json:"name"`
	Platform     string          `json:"platform"`
	Runtimes     []RuntimeInfo   `json:"runtimes"`
	Capabilities map[string]bool `json:"capabilities"`
	// LiveSessions names the runtime sessions whose phone-pane plugin is
	// currently able to accept a prompt. Absent from older connectors, which
	// simply means none can be confirmed live.
	LiveSessions []string `json:"liveSessions,omitempty"`
}

// Beat is the connector's periodic liveness frame. It repeats the live session
// set so presence follows a plugin dying between heartbeats; a server that does
// not know the field ignores it.
type Beat struct {
	// Bridge reports that a phone-pane plugin is loaded and its heartbeat is
	// fresh, which is what lets the server accept a submit at all.
	Bridge bool `json:"bridge"`
	// LiveSessions is the current per-session presence set. It is a complete
	// replacement, not a delta: a session that disappears from the list is no
	// longer live, which is how a plugin stopping is noticed.
	//
	// It is deliberately always on the wire, even when empty or nil: a Server
	// must be able to tell "this connector says no session is live" from "this
	// frame did not carry the field at all" (an older connector's pong, whose
	// absent field must not wipe the presence the previous beat reported).
	LiveSessions []string `json:"liveSessions"`
}

// RuntimeInfo describes one engine the connector can drive. Only "claude"
// exists today; the field stays a list because the Python product advertised
// several and clients render the list generically.
type RuntimeInfo struct {
	Type    string `json:"type"`
	Version string `json:"version,omitempty"`
	Ready   bool   `json:"ready"`
}

// TurnBegin opens a turn.
type TurnBegin struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	RequestID string `json:"requestId,omitempty"`
	QueuedAt  int64  `json:"queuedAt,omitempty"`
}

// TurnDelta carries text produced since the last delta of the same turn.
//
// Rev is the number of deltas sent for this turn so far; the server drops a
// delta whose Rev is not exactly one past what it has applied, which makes
// replays after a reconnect harmless.
type TurnDelta struct {
	SessionID string `json:"sessionId"`
	TurnID    string `json:"turnId"`
	Rev       int    `json:"rev"`
	Text      string `json:"text"`
}

// TurnEnd closes a turn.
type TurnEnd struct {
	SessionID string         `json:"sessionId"`
	TurnID    string         `json:"turnId"`
	RequestID string         `json:"requestId,omitempty"`
	Status    string         `json:"status"` // "done" | "aborted" | "error"
	Text      string         `json:"text"`
	Error     string         `json:"error,omitempty"`
	State     map[string]any `json:"state,omitempty"`
}

// ToolBegin announces a tool call.
type ToolBegin struct {
	SessionID string          `json:"sessionId"`
	TurnID    string          `json:"turnId"`
	ToolUseID string          `json:"toolUseId"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input,omitempty"`
}

// ToolEnd reports the outcome of a tool call.
type ToolEnd struct {
	SessionID string          `json:"sessionId"`
	ToolUseID string          `json:"toolUseId"`
	OK        bool            `json:"ok"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// ApprovalRequest asks the person to decide on a tool call. Kind lets the
// client render the right card: a plain allow/deny, a question with options, or
// a plan awaiting approval.
type ApprovalRequest struct {
	SessionID string `json:"sessionId"`
	// ExternalSessionID is the runtime's own session id when the plugin reports
	// it; the server can use it to address a question that arrives before the
	// connector has mapped the turn.
	ExternalSessionID string          `json:"externalSessionId,omitempty"`
	TurnID            string          `json:"turnId,omitempty"`
	ToolUseID         string          `json:"toolUseId"`
	Tool              string          `json:"tool"`
	Kind              string          `json:"kind"` // "permission" | "question" | "plan"
	Input             json.RawMessage `json:"input,omitempty"`
}

// ApprovalCancel retracts a question that no longer needs an answer.
type ApprovalCancel struct {
	SessionID string `json:"sessionId"`
	ToolUseID string `json:"toolUseId"`
}

// Submit asks the connector to run a turn.
type Submit struct {
	RequestID string `json:"requestId"`
	SessionID string `json:"sessionId"`
	// ExternalSessionID is the runtime's own session id, used to route the
	// message to the conversation it was addressed to.
	ExternalSessionID string `json:"externalSessionId,omitempty"`
	Text              string `json:"text"`
	// Action is "prompt" for ordinary text and "command" for a slash command,
	// which the plugin executes without raising a turn.
	Action string `json:"action,omitempty"`
	// Attachments are the files the person picked on their phone or browser.
	// The bytes stay on the server; the connector fetches them with its own
	// bearer and hands the plugin local paths inside the prompt text, because
	// the plugin protocol carries text only.
	Attachments []SubmitAttachment `json:"attachments,omitempty"`
}

// SubmitAttachment is one file attached to an outgoing message.
//
// DownloadURL is a path on the connector's own server
// ("/api/v2/connector/sessions/{id}/attachments/{fileId}/content") guarded by
// the connector token, so the connector never needs a user credential.
type SubmitAttachment struct {
	FileID      string `json:"fileId"`
	Name        string `json:"name"`
	MediaType   string `json:"mediaType"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256,omitempty"`
	DownloadURL string `json:"downloadUrl"`
}

// SessionOpen asks the connector to open a terminal for one conversation.
//
// Resume tells the connector which runtime flag to start Claude Code with: an
// existing transcript is reopened with --resume, while a conversation that has
// never run is created with --session-id so the id the server already stored
// becomes the new transcript's name. The connector confirms this against its
// own transcript tree: it can tell whether the file exists, while the server
// only knows that it minted the id and that a previous open was requested.
// CWD is resolved on the connector's own machine; the server only knows the
// path the desktop reported.
type SessionOpen struct {
	SessionID         string `json:"sessionId"`
	ExternalSessionID string `json:"externalSessionId"`
	CWD               string `json:"cwd"`
	Title             string `json:"title"`
	Resume            bool   `json:"resume"`
}

// SessionClose asks the connector to stop whatever is running for one
// conversation: the Claude Code process the phone opened, and the terminal
// window that hosts it. The transcript and the server record stay; the next
// message reopens the conversation, which is what makes "close" a restart.
type SessionClose struct {
	SessionID         string `json:"sessionId"`
	ExternalSessionID string `json:"externalSessionId"`
}

// Abort asks the connector to stop the running turn.
//
// ExternalSessionID is optional: a server that knows the runtime's own session
// id sends it so the connector can address the abort without relying on which
// message it happens to be following.
type Abort struct {
	RequestID         string `json:"requestId"`
	SessionID         string `json:"sessionId"`
	ExternalSessionID string `json:"externalSessionId,omitempty"`
}

// ApprovalAnswer carries the person's decision back to the blocked hook.
type ApprovalAnswer struct {
	RequestID string `json:"requestId"`
	ToolUseID string `json:"toolUseId"`
	Decision  string `json:"decision"` // "allow" | "deny"
	// UpdatedInput replaces the tool input entirely when the person edited it.
	UpdatedInput json.RawMessage   `json:"updatedInput,omitempty"`
	Answers      map[string]string `json:"answers,omitempty"`
	Message      string            `json:"message,omitempty"`
}

// TypeInventory reports the sessions the connector can see on its machine.
const TypeInventory = "session.inventory"

// Inventory is the connector's view of the local transcripts. The server uses
// it to create the projects and sessions a person expects to find waiting.
//
// Full asks the server to treat the list as the complete truth rather than a
// set of changes; it is sent on connect and occasionally afterwards so a
// missed frame cannot leave a session permanently missing.
type Inventory struct {
	Sessions []InventorySession `json:"sessions"`
	Full     bool               `json:"full,omitempty"`
	// Removed names sessions that disappeared from disk since the last report.
	Removed []string `json:"removed,omitempty"`
}

// InventorySession is one discovered conversation.
type InventorySession struct {
	ExternalID string `json:"externalId"`
	CWD        string `json:"cwd"`
	Title      string `json:"title,omitempty"`
	Model      string `json:"model,omitempty"`
	UpdatedAt  int64  `json:"updatedAt"`
	Version    string `json:"version,omitempty"`
}

// TypeHistoryRequest asks the connector for a session's transcript.
const TypeHistoryRequest = "history.request"

// TypeHistory carries the transcript back.
const TypeHistory = "session.history"

// TypeHistoryAck confirms that the server committed a transcript version.
const TypeHistoryAck = "session.history.ack"

// HistoryRequest names the session whose conversation to read.
type HistoryRequest struct {
	SessionID  string `json:"sessionId"`
	ExternalID string `json:"externalId"`
	// Limit caps a recent-tail refresh. Full requests transfer every item in
	// bounded chunks, so the transcript size does not set one frame's size.
	Limit int  `json:"limit,omitempty"`
	Full  bool `json:"full,omitempty"`
	// Force bypasses a connector's confirmed full-version cache when the server
	// knows it has no transcript rows for this session.
	Force bool `json:"force,omitempty"`
}

// History is one session's transcript, oldest first. Full imports carry a
// completion marker so the server can publish once after all chunks commit.
type History struct {
	SessionID string        `json:"sessionId"`
	Version   string        `json:"version,omitempty"`
	Full      bool          `json:"full,omitempty"`
	Complete  *bool         `json:"complete,omitempty"`
	Items     []HistoryItem `json:"items"`
}

// HistoryAck confirms a transcript snapshot after its database transaction.
type HistoryAck struct {
	SessionID string `json:"sessionId"`
	Version   string `json:"version"`
	Full      bool   `json:"full,omitempty"`
	Complete  bool   `json:"complete,omitempty"`
}

// HistoryItem is one message or tool call read from a transcript.
type HistoryItem struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`   // "message" | "tool"
	Role      string          `json:"role"`   // user | assistant | tool
	Status    string          `json:"status"` // done | failed
	Text      string          `json:"text,omitempty"`
	Tool      string          `json:"tool,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Timestamp int64           `json:"ts"`
}

// Generic request/response pair for calls the server makes into the desktop.
//
// The connector is the only side that can touch the user's files or start a
// terminal, so those features arrive as RPCs rather than server-side work.
const (
	TypeRPCRequest  = "rpc.request"
	TypeRPCResponse = "rpc.response"
)

// RPCRequest names a connector method and carries its parameters.
type RPCRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

// RPCResponse answers one request.
type RPCResponse struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError describes a failed call.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// TypeAgentActivity reports the subagents running beside the main turn.
const TypeAgentActivity = "agent.activity"

// AgentActivity is the status line the phone shows under the conversation.
type AgentActivity struct {
	SessionID string         `json:"sessionId"`
	Agents    []AgentRunning `json:"agents"`
}

// AgentRunning is one subagent's state.
//
// Tool, Detail and Text are the subagent's live activity, captured from the
// loop's own events: the tool it is running now, the one-line argument the
// terminal would show under it, and its latest visible text. Each is absent
// when the runtime exposes no such fact; an empty value is not sent.
type AgentRunning struct {
	AgentID   string `json:"agentId"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Tool      string `json:"tool,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Text      string `json:"text,omitempty"`
}
