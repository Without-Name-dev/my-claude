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
// a test can observe a close without ending a real terminal. startedBefore
// bounds the search to processes that already existed when the person asked:
// a replacement terminal opened by the next message must not be killed by a
// scan that was still running.
var closeSessionProcessFunc = func(externalSessionID string, startedBefore time.Time) (int, error) {
	return closeSessionProcess(externalSessionID, startedBefore)
}

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
	// The request is the point in time the person meant; a terminal opened
	// after it belongs to a newer message and must survive this scan.
	startedBefore := time.Now()
	generation := c.beginClose(msg.SessionID)
	killed, err := closeSessionProcessFunc(externalID, startedBefore)
	if err != nil {
		// Nothing was stopped. Marking it closed here told the watcher the
		// turn was aborted while the process kept working, and the phone
		// showed a stopped conversation that was still running.
		c.Log.Warn("could not close the terminal",
			"session", msg.SessionID, "external", externalID, "error", err)
		return
	}
	if killed == 0 {
		// The search succeeded but matched nothing (the terminal may run under
		// a different id). Reporting a close here still aborted the watcher
		// while the process kept working.
		c.Log.Warn("no terminal matched the runtime session id; leaving it open",
			"session", msg.SessionID, "external", externalID)
		return
	}
	// A message queued while this scan was running superseded the close: the
	// person changed their mind, and marking the session closed now would abort
	// the turn that message is about to start.
	if !c.finishClose(msg.SessionID, generation) {
		c.Log.Info("close superseded by a newer message",
			"session", msg.SessionID, "external", externalID, "processes", killed)
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

// beginClose records a new close request and returns its generation.
func (c *Client) beginClose(sessionID string) int {
	if sessionID == "" {
		return 0
	}
	c.closedMu.Lock()
	defer c.closedMu.Unlock()
	if c.closeGen == nil {
		c.closeGen = map[string]int{}
	}
	c.closeGen[sessionID]++
	return c.closeGen[sessionID]
}

// finishClose reports whether this close is still the latest action on the
// session.
func (c *Client) finishClose(sessionID string, generation int) bool {
	if sessionID == "" {
		return false
	}
	c.closedMu.Lock()
	defer c.closedMu.Unlock()
	return c.closeGen[sessionID] == generation
}

// supersedeClose invalidates a close that is still scanning the process table.
func (c *Client) supersedeClose(sessionID string) {
	if sessionID == "" {
		return
	}
	c.closedMu.Lock()
	defer c.closedMu.Unlock()
	if c.closeGen == nil {
		c.closeGen = map[string]int{}
	}
	c.closeGen[sessionID]++
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
