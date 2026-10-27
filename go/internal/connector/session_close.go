package connector

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// closedSessionTTL is how long a close is remembered. It only has to outlive
// the watcher of the turn that was running when the person closed the window;
// the same conversation can be reopened afterwards and must not inherit it.
const closedSessionTTL = 2 * time.Minute

// closeSessionProcessFunc is the platform process killer. It is a variable so
// a test can observe a close without ending a real terminal.
var closeSessionProcessFunc = closeSessionProcess

// handleSessionClose runs one session.close frame off the read loop, because
// finding and ending a process can take a moment.
func (c *Client) handleSessionClose(raw json.RawMessage) {
	var msg protocol.SessionClose
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.Log.Warn("bad session.close", "error", err)
		return
	}
	go c.closeSession(msg)
}

// closeSession stops the terminal that runs one conversation.
//
// The transcript and the server's record are left alone: closing is how a
// person restarts a conversation from the phone, and the next message reopens
// it through the same path that opens a cold session.
func (c *Client) closeSession(msg protocol.SessionClose) {
	externalID := strings.TrimSpace(msg.ExternalSessionID)
	if externalID == "" || !safeRuntimeSessionID(externalID) {
		c.Log.Info("refusing to close a session",
			"reason", "no usable runtime session id", "session", msg.SessionID)
		return
	}
	killed, err := closeSessionProcessFunc(externalID)
	if err != nil {
		// Nothing was stopped. Marking it closed here told the watcher the
		// turn was aborted while the process kept working, and the phone
		// showed a stopped conversation that was still running.
		c.Log.Warn("could not close the terminal",
			"session", msg.SessionID, "external", externalID, "error", err)
		return
	}
	// Only a confirmed close ends the watcher early. Left until after the kill,
	// this is the difference between "the person stopped it" and "we hoped so".
	c.markSessionClosed(msg.SessionID)
	// Presence is evidence of a process that can take a message. It is gone
	// now, so the stamp must not keep the conversation looking live.
	if c.Bridge != nil {
		if err := c.Bridge.RemoveLiveStamp(externalID); err != nil {
			c.Log.Warn("could not clear the presence stamp",
				"session", msg.SessionID, "external", externalID, "error", err)
		}
	}
	c.Log.Info("closed the terminal for a session",
		"session", msg.SessionID, "external", externalID, "processes", killed)
}

// markSessionClosed records a deliberate close for the turn watcher.
func (c *Client) markSessionClosed(sessionID string) {
	if sessionID == "" {
		return
	}
	now := time.Now()
	c.closedMu.Lock()
	defer c.closedMu.Unlock()
	for id, at := range c.closed {
		if now.Sub(at) >= closedSessionTTL {
			delete(c.closed, id)
		}
	}
	c.closed[sessionID] = now
}

// sessionWasClosed reports whether the person closed this conversation's
// terminal in the last few minutes.
func (c *Client) sessionWasClosed(sessionID string) bool {
	c.closedMu.Lock()
	defer c.closedMu.Unlock()
	at, ok := c.closed[sessionID]
	return ok && time.Since(at) < closedSessionTTL
}

// clearSessionClosed forgets a close. Sending the next message is a request to
// run again — the server reopens the terminal on that path — so the new turn
// must not be closed by the mark left from the old one.
func (c *Client) clearSessionClosed(sessionID string) {
	if sessionID == "" {
		return
	}
	c.closedMu.Lock()
	defer c.closedMu.Unlock()
	delete(c.closed, sessionID)
}
