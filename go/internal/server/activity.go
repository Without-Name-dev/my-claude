package server

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// EventActivity is the event that carries the subagent status line.
const EventActivity = "runtime.activity"

// activityStaleAfter bounds a status line whose connector went silent without
// closing its socket. A running subagent refreshes UpdatedAt on every tool call
// and streamed chunk, so silence this long means the run is over and the entry
// is a ghost. Without this bound a crashed connector left "运行中" on the phone
// until the server restarted.
const activityStaleAfter = 20 * time.Minute

// activityEntry remembers who reported it, so a connector that disconnects can
// take its status lines with it.
type activityEntry struct {
	agents    []protocol.AgentRunning
	connector string
}

// activityRegistry keeps, per session, the subagents the connector last
// reported. It is process memory on purpose: the status line describes work
// that is happening now, and a restart has nothing to restore.
type activityRegistry struct {
	mu   sync.Mutex
	byID map[string]activityEntry
}

func newActivityRegistry() *activityRegistry {
	return &activityRegistry{byID: map[string]activityEntry{}}
}

func (r *activityRegistry) set(sessionID, connectorID string, agents []protocol.AgentRunning) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(agents) == 0 {
		delete(r.byID, sessionID)
		return
	}
	r.byID[sessionID] = activityEntry{agents: agents, connector: connectorID}
}

// get returns the agents that are still plausibly running. An entry whose
// agents have all gone quiet for activityStaleAfter is dropped, so a connector
// that died mid-turn cannot keep a finished subagent on screen.
func (r *activityRegistry) get(sessionID string) []protocol.AgentRunning {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.byID[sessionID]
	if !ok {
		return nil
	}
	live := make([]protocol.AgentRunning, 0, len(entry.agents))
	for _, agent := range entry.agents {
		// UpdatedAt is zero only for a connector that never sent it; those are
		// kept, because guessing staleness from a missing field would hide a
		// genuinely running agent.
		if agent.UpdatedAt > 0 && time.Since(time.UnixMilli(agent.UpdatedAt)) > activityStaleAfter {
			continue
		}
		live = append(live, agent)
	}
	if len(live) == 0 {
		delete(r.byID, sessionID)
		return nil
	}
	if len(live) != len(entry.agents) {
		entry.agents = live
		r.byID[sessionID] = entry
	}
	return live
}

// clearConnector removes every status line the connector reported and returns
// the sessions they belonged to, so their viewers can be told the agents ended
// instead of waiting for the staleness bound.
func (r *activityRegistry) clearConnector(connectorID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var sessions []string
	for sessionID, entry := range r.byID {
		if entry.connector != connectorID {
			continue
		}
		sessions = append(sessions, sessionID)
		delete(r.byID, sessionID)
	}
	return sessions
}

// onAgentActivity records and forwards the subagents running beside a turn.
func (a *App) onAgentActivity(ctx context.Context, conn *connectorConn, msg protocol.AgentActivity) {
	sessionID := msg.SessionID
	// A background poll reports by runtime id, because it is not answering a
	// queued message and therefore has no server session id in hand.
	if sessionID == "" && msg.ExternalSessionID != "" {
		session, err := a.db.SessionByExternalID(ctx, conn.id, msg.ExternalSessionID)
		if err != nil {
			return
		}
		sessionID = session.ID
	}
	if sessionID == "" {
		return
	}
	if _, err := a.ownedSessionForConnector(ctx, conn, sessionID); err != nil {
		return
	}
	a.activity.set(sessionID, conn.id, msg.Agents)

	frame, err := newEnvelope(sessionID, a.currentSeq(ctx, sessionID), EventActivity, map[string]any{
		"agents": msg.Agents,
	})
	if err != nil {
		return
	}
	a.broker.Publish(sessionID, frame)
}

// clearConnectorActivity ends the status lines a disconnected connector owned.
// Nothing can still be running on a machine that is no longer connected.
func (a *App) clearConnectorActivity(ctx context.Context, connectorID string) {
	for _, sessionID := range a.activity.clearConnector(connectorID) {
		frame, err := newEnvelope(sessionID, a.currentSeq(ctx, sessionID), EventActivity, map[string]any{
			"agents": []protocol.AgentRunning{},
		})
		if err != nil {
			continue
		}
		a.broker.Publish(sessionID, frame)
	}
}

// handleSessionActivity returns the current subagent list.
func (a *App) handleSessionActivity(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	agents := a.activity.get(session.ID)
	if agents == nil {
		agents = []protocol.AgentRunning{}
	}
	httpx.OK(w, map[string]any{"agents": agents, "serverTime": timestampNow()})
}
