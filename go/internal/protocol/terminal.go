package protocol

// Terminal frames carry an interactive shell between the server and the
// desktop.
//
// Control travels over the existing connector socket as one TypeTerminal frame
// whose Data is a TerminalCommand. Output travels the other way over a
// dedicated connector-opened websocket (TerminalLinkPath), because the server
// has no other inbound hook on the main link: control is request-shaped and
// rare, output is a continuous stream.
const (
	// TypeTerminal is the server → connector control frame. The operation is
	// named inside TerminalCommand.Op so one dispatch case handles all of them.
	TypeTerminal = "terminal"

	// TypeTerminalOpened acknowledges that the PTY is running.
	TypeTerminalOpened = "terminal.opened"
	// TypeTerminalOutput carries one chunk of PTY bytes, base64 encoded.
	TypeTerminalOutput = "terminal.output"
	// TypeTerminalExit reports that the process tree is gone.
	TypeTerminalExit = "terminal.exit"
	// TypeTerminalState reports whether a terminal still exists on the
	// connector, together with a bounded snapshot of its recent output. The
	// connector sends it when the output link (re)connects and in reply to a
	// probe, so a server that lost its in-memory replay can rebuild it.
	TypeTerminalState = "terminal.state"
	// TypeTerminalError reports why a terminal could not start or why the
	// terminal link failed to deliver a command.
	TypeTerminalError = "terminal.error"
)

// Terminal operations inside TerminalCommand.Op.
const (
	TerminalOpOpen   = "open"
	TerminalOpInput  = "input"
	TerminalOpResize = "resize"
	TerminalOpClose  = "close"
	// TerminalOpProbe asks whether a terminal is still running on the
	// connector. Attaching viewers use it to distinguish a live PTY from a
	// database row left over from a connector restart.
	TerminalOpProbe = "probe"
)

// TerminalLinkPath is where a connector opens the socket that carries PTY
// output. It is authenticated with the connector's own bearer token, exactly
// like the main connector socket.
const TerminalLinkPath = "/api/v2/connector/terminal-link"

// TerminalCommand is one server → connector terminal instruction. All fields
// are optional except Op and TerminalID; which ones matter depends on Op.
type TerminalCommand struct {
	Op         string `json:"op"`
	TerminalID string `json:"terminalId"`
	SessionID  string `json:"sessionId,omitempty"`
	// Root and CWD are both forwarded unresolved so the connector can join
	// them with its own filesystem rules; the server may run on a different
	// operating system than the desktop.
	Root  string `json:"root,omitempty"`
	CWD   string `json:"cwd,omitempty"`
	Shell string `json:"shell,omitempty"`
	Cols  int    `json:"cols,omitempty"`
	Rows  int    `json:"rows,omitempty"`
	// Data is base64 for Op == "input" and is written to the PTY verbatim
	// after decoding.
	Data string `json:"data,omitempty"`
}

// TerminalOpened is the connector's confirmation that a PTY started.
type TerminalOpened struct {
	TerminalID string `json:"terminalId"`
	PID        int    `json:"pid"`
}

// TerminalOutput is one chunk read from a PTY.
//
// Seq starts at 1 for each terminal and rises by one per chunk. The server
// relies on it to order fan-out and to filter duplicates after a link
// reconnect.
type TerminalOutput struct {
	TerminalID string `json:"terminalId"`
	Seq        uint64 `json:"seq"`
	// Data is standard base64 (with padding) of raw PTY bytes.
	Data string `json:"data"`
}

// TerminalExit closes a terminal. ExitCode is null when the process was killed
// before it could report one; Reason is "exit" for a natural end and "closed"
// when the person asked for it.
type TerminalExit struct {
	TerminalID string `json:"terminalId"`
	ExitCode   *int   `json:"exitCode"`
	Reason     string `json:"reason,omitempty"`
}

// TerminalState is the connector's answer to "does this terminal still exist?"
// and its replay snapshot. Seq is the highest output sequence the connector has
// emitted, and Data is standard base64 of the bounded scrollback it still
// holds. A server that still has newer output must ignore the snapshot.
type TerminalState struct {
	TerminalID string `json:"terminalId"`
	Running    bool   `json:"running"`
	Seq        uint64 `json:"seq,omitempty"`
	Data       string `json:"data,omitempty"`
}

// TerminalError reports a terminal that could not be started or driven.
type TerminalError struct {
	TerminalID string `json:"terminalId"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}
