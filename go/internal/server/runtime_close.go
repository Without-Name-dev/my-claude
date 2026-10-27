package server

import (
	"net/http"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// handleCloseRuntime stops the terminal that runs one conversation on the
// connector's machine.
//
// Closing is how a person restarts a conversation from the phone: the
// transcript and the server's record are untouched, and the next message
// reopens the conversation through the same path that opens a cold session.
func (a *App) handleCloseRuntime(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	if session.ExternalID == "" {
		// Nothing has ever run for this conversation, so there is no window
		// to close. Answering "already closed" keeps the phone's menu honest.
		httpx.OK(w, map[string]any{
			"ok":     true,
			"error":  nil,
			"result": map[string]any{"sent": false, "live": false},
		})
		return
	}
	if !a.connectors.Online(session.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	if !a.connectors.Send(session.ConnectorID, protocol.TypeSessionClose, protocol.SessionClose{
		SessionID:         session.ID,
		ExternalSessionID: session.ExternalID,
	}) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	// Closing ends the automatic-open window with the process. Keeping the
	// reservation meant a message sent within the next minute was accepted but
	// never reached a terminal, then timed out two minutes later.
	a.releaseAutoOpen(session.ID)
	// The person ended this run deliberately; the phone must stop showing the
	// conversation as working while the desktop tears the process down.
	a.setSessionStatus(r.Context(), session.ID, "idle")
	httpx.OK(w, map[string]any{
		"ok":     true,
		"error":  nil,
		"result": map[string]any{"sent": true, "live": true},
	})
}
