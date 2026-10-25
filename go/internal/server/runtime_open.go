package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// autoOpenCooldown bounds how often the automatic send path asks one session's
// connector to start a terminal. A retried message, or a burst of messages to
// the same cold conversation, must not queue a second terminal while the first
// is still starting. The connector throttles spawns too, but its memory does
// not survive a restart and the server can see a reconnect before the new
// process has any history.
const autoOpenCooldown = 60 * time.Second

// claimAutoOpen reserves this session's automatic open for one cooldown window
// and reports whether the caller may send the frame now.
func (a *App) claimAutoOpen(sessionID string, now time.Time) bool {
	a.openMu.Lock()
	defer a.openMu.Unlock()
	for id, at := range a.lastAutoOpen {
		if now.Sub(at) >= autoOpenCooldown {
			delete(a.lastAutoOpen, id)
		}
	}
	if at, ok := a.lastAutoOpen[sessionID]; ok && now.Sub(at) < autoOpenCooldown {
		return false
	}
	a.lastAutoOpen[sessionID] = now
	return true
}

// releaseAutoOpen gives back a reservation whose frame could not be queued, so
// the next attempt may ask again instead of waiting out a cooldown for an open
// that never happened.
func (a *App) releaseAutoOpen(sessionID string) {
	a.openMu.Lock()
	delete(a.lastAutoOpen, sessionID)
	a.openMu.Unlock()
}

// sessionNotLiveCode is the machine-readable reason a send could not reach a
// conversation. Clients offer "open on the computer" for it instead of asking
// the person to retype the message.
const sessionNotLiveCode = "session_not_live"

// errSessionNotLive reports that a message was addressed to a conversation the
// connector could not be asked to open, so nothing on the desktop will run it.
var errSessionNotLive = errors.New("session is not live")

// sessionOpenResult is what the explicit open endpoint reports.
type sessionOpenResult struct {
	Sent bool `json:"sent"`
	// ExternalSessionID is the runtime id the connector was told to use. It is
	// minted for a conversation that has never run, so the client can show the
	// id the desktop just picked up.
	ExternalSessionID string `json:"externalSessionId"`
	Resume            bool   `json:"resume"`
	// Live is the presence the server saw before sending the request. A live
	// conversation needs no terminal; the connector refuses to spawn one.
	Live bool `json:"live"`
}

// handleOpenRuntime opens a conversation in a terminal on its connector's
// machine.
//
// This is the explicit "open on the computer" action. It is also the fallback a
// message send uses when the addressed conversation has no live plugin: the
// person gets a terminal instead of a message that sits unanswered.
func (a *App) handleOpenRuntime(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	if !a.connectors.Online(session.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	// An older connector ignores session.open (its frame router only logs
	// unknown types). Reporting the terminal as opened would leave the person
	// waiting on a desktop that never does anything.
	if !a.connectors.SupportsSessionOpen(session.ConnectorID) {
		httpx.Fail(w, httpx.Conflict("this connector cannot open sessions on the computer"))
		return
	}

	live := a.connectors.Live(session.ConnectorID, session.ExternalID)
	frame, updated, err := a.sessionOpenFrame(r.Context(), session)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if updated.ExternalID != session.ExternalID {
		// Another window (or the dashboard list) has to learn the id that was
		// just assigned; the next send will address it.
		a.publishSessionMeta(r.Context(), updated)
	}
	sent := a.connectors.Send(session.ConnectorID, protocol.TypeSessionOpen, frame)
	httpx.OK(w, map[string]any{
		"ok":    true,
		"error": nil,
		"result": sessionOpenResult{
			Sent:              sent,
			ExternalSessionID: frame.ExternalSessionID,
			Resume:            frame.Resume,
			Live:              live,
		},
	})
}

// sessionOpenForDelivery asks the connector to open a conversation that has no
// live plugin, before a message is submitted to it.
//
// It reports sent=false when the frame could not be queued, which means the
// conversation still has nowhere to run. A conversation that is already live
// needs no frame and reports sent=true.
func (a *App) sessionOpenForDelivery(ctx context.Context, session store.Session) (store.Session, bool, error) {
	if a.connectors.Live(session.ConnectorID, session.ExternalID) {
		return session, true, nil
	}
	// A connector that cannot start a terminal, or has no bridge directory to
	// queue into, can only deliver through a plugin that is already running.
	// Keep the pre-spawn contract for those: never acknowledge a message that
	// nothing on the desktop can pick up.
	if !a.connectors.SupportsSessionOpen(session.ConnectorID) || !a.connectors.CanQueue(session.ConnectorID) {
		if session.ExternalID != "" && a.connectors.CanDeliver(session.ConnectorID) {
			return session, true, nil
		}
		return session, false, nil
	}
	frame, updated, err := a.sessionOpenFrame(ctx, session)
	if err != nil {
		return session, false, err
	}
	if !a.claimAutoOpen(session.ID, time.Now()) {
		// This conversation's terminal was already asked for moments ago and is
		// still starting; queue the message for it rather than opening another.
		return updated, true, nil
	}
	if updated.ExternalID != session.ExternalID {
		a.publishSessionMeta(ctx, updated)
	}
	sent := a.connectors.Send(updated.ConnectorID, protocol.TypeSessionOpen, frame)
	if !sent {
		a.releaseAutoOpen(session.ID)
		return updated, false, nil
	}
	a.log.Info("asked the connector to open a session",
		"session", updated.ID,
		"external", frame.ExternalSessionID,
		"resume", frame.Resume)
	return updated, true, nil
}

// sessionOpenFrame builds the frame for one conversation, assigning a runtime id
// first when the conversation has never run.
func (a *App) sessionOpenFrame(ctx context.Context, session store.Session) (protocol.SessionOpen, store.Session, error) {
	frame := protocol.SessionOpen{
		SessionID:         session.ID,
		ExternalSessionID: session.ExternalID,
		CWD:               session.CWD,
		Title:             session.Title,
		// A session that already carries a runtime id is one the connector
		// found in a transcript (or a client bound to an existing one), so the
		// terminal must resume it rather than create a second conversation.
		Resume: session.ExternalID != "",
	}
	if frame.ExternalSessionID != "" {
		return frame, session, nil
	}

	id, err := newRuntimeSessionID()
	if err != nil {
		return protocol.SessionOpen{}, session, fmt.Errorf("mint runtime session id: %w", err)
	}
	updated, err := a.db.AssignSessionExternalID(ctx, session.ID, id)
	if err != nil {
		return protocol.SessionOpen{}, session, err
	}
	frame.ExternalSessionID = updated.ExternalID
	// Another request (or the connector's inventory) may have supplied an id
	// first. That id already names a transcript, so it must be resumed; only an
	// id this call minted is a conversation that does not exist yet.
	frame.Resume = updated.ExternalID != id
	return frame, updated, nil
}

// newRuntimeSessionID returns a random UUID v4, the shape Claude Code expects
// for --session-id.
func newRuntimeSessionID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = (raw[6] & 0x0f) | 0x40 // version 4
	raw[8] = (raw[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

// failSessionNotLive answers a send that has nowhere to run. The detail is an
// object so both clients can read the machine-readable code while still
// showing the message.
func failSessionNotLive(w http.ResponseWriter, message string) {
	httpx.JSON(w, http.StatusConflict, map[string]any{
		"detail": map[string]any{
			"message": message,
			"code":    sessionNotLiveCode,
		},
	})
}
