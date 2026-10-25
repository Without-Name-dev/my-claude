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
	// TypePing is a liveness probe; the connector answers with TypeBeat.
	TypePing = "ping"
)

// Hello is the connector's introduction.
type Hello struct {
	Version      int             `json:"version"`
	ConnectorID  string          `json:"connectorId"`
	Name         string          `json:"name"`
	Platform     string          `json:"platform"`
	Runtimes     []RuntimeInfo   `json:"runtimes"`
	Capabilities map[string]bool `json:"capabilities"`
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
	Status    string         `json:"status"` // "done" | "aborted" | "error"
	Text      string         `json:"text"`
	Chunks    int            `json:"chunks,omitempty"`
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
}

// TypeHistoryRequest asks the connector for a session's transcript.
const TypeHistoryRequest = "history.request"

// TypeHistory carries the transcript back.
const TypeHistory = "session.history"

// HistoryRequest names the session whose conversation to read.
type HistoryRequest struct {
	SessionID  string `json:"sessionId"`
	ExternalID string `json:"externalId"`
	// Limit caps how many recent records are converted. A long session can be
	// tens of thousands of lines; the phone only needs the tail.
	Limit int `json:"limit,omitempty"`
}

// History is one session's transcript, oldest first.
type History struct {
	SessionID string        `json:"sessionId"`
	Items     []HistoryItem `json:"items"`
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
type AgentRunning struct {
	AgentID   string `json:"agentId"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
	UpdatedAt int64  `json:"updatedAt"`
}
