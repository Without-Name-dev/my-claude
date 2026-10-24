package server

import (
	"context"
	"net/http"
	"sync"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// EventActivity is the event that carries the subagent status line.
const EventActivity = "runtime.activity"

// activityRegistry keeps, per session, the subagents the connector last
// reported. It is process memory on purpose: the status line describes work
// that is happening now, and a restart has nothing to restore.
type activityRegistry struct {
	mu   sync.Mutex
	byID map[string][]protocol.AgentRunning
}

func newActivityRegistry() *activityRegistry {
	return &activityRegistry{byID: map[string][]protocol.AgentRunning{}}
}

func (r *activityRegistry) set(sessionID string, agents []protocol.AgentRunning) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(agents) == 0 {
		delete(r.byID, sessionID)
		return
	}
	r.byID[sessionID] = agents
}

func (r *activityRegistry) get(sessionID string) []protocol.AgentRunning {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.byID[sessionID]
}

// onAgentActivity records and forwards the subagents running beside a turn.
func (a *App) onAgentActivity(ctx context.Context, conn *connectorConn, msg protocol.AgentActivity) {
	if _, err := a.ownedSessionForConnector(ctx, conn, msg.SessionID); err != nil {
		return
	}
	a.activity.set(msg.SessionID, msg.Agents)

	frame, err := newEnvelope(msg.SessionID, a.currentSeq(ctx, msg.SessionID), EventActivity, map[string]any{
		"agents": msg.Agents,
	})
	if err != nil {
		return
	}
	a.broker.Publish(msg.SessionID, frame)
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
