package connector

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/bridge"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// pollInterval is how often the watcher reads the outbox. The plugin rewrites
// the file as text arrives, so this is the resolution of the phone's stream;
// 120ms is below what a person notices and cheap enough to run per turn.
const pollInterval = 120 * time.Millisecond

// pickupTimeout is how long a queued message may sit before the connector gives
// up on it.
//
// The plugin only drains its queue between turns, so a message sent while the
// session is answering waits for that answer to finish. Treating that wait as a
// failure is what made the phone show "failed, retry" mid-conversation; the
// deadline now only catches a session that never picks anything up at all.
const pickupTimeout = 15 * time.Minute

// turnSink receives what the watcher observes.
type TurnSink interface {
	TurnBegin(msg protocol.TurnBegin)
	TurnDelta(msg protocol.TurnDelta)
	TurnEnd(msg protocol.TurnEnd)
	ToolBegin(msg protocol.ToolBegin)
	ToolEnd(msg protocol.ToolEnd)
	AgentActivity(msg protocol.AgentActivity)
}

// TurnWatcher follows one submitted message until the plugin closes it.
type TurnWatcher struct {
	bridge     *bridge.Bridge
	sink       TurnSink
	sessionID  string
	previousID string
	queuedAt   int64

	mu        sync.Mutex
	turnID    string
	sent      string
	rev       int
	seenTool  map[string]bool
	agentsKey string
}

// NewTurnWatcher prepares a watcher for a message that was just queued.
func NewTurnWatcher(b *bridge.Bridge, sink TurnSink, sessionID string, sub *bridge.Submission) *TurnWatcher {
	w := &TurnWatcher{
		bridge:    b,
		sink:      sink,
		sessionID: sessionID,
		seenTool:  map[string]bool{},
	}
	if sub != nil {
		w.previousID = sub.PreviousTurnID
		w.queuedAt = sub.QueuedAt
	}
	return w
}

// Run polls until the turn completes or ctx ends. It returns the final text.
func (w *TurnWatcher) Run(ctx context.Context) string {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	deadline := time.Now().Add(pickupTimeout)
	lastActivity := time.Now()

	for {
		select {
		case <-ctx.Done():
			return w.currentText()
		case <-ticker.C:
		}

		box, err := w.bridge.ReadOutbox()
		if err != nil || box == nil {
			if time.Now().After(deadline) && w.turnID == "" {
				w.sink.TurnEnd(protocol.TurnEnd{
					SessionID: w.sessionID,
					Status:    "error",
					Error:     "the desktop did not pick up the message",
				})
				return ""
			}
			continue
		}
		// While another turn is streaming, the queue is simply waiting its
		// turn; that is activity, not a stalled pickup.
		if box.Running() && box.TurnID != w.turnID && time.Since(lastActivity) > 0 {
			lastActivity = time.Now()
			deadline = lastActivity.Add(pickupTimeout)
		}

		// The outbox still shows the turn that was running when this message
		// was queued; ignore it until the plugin moves on.
		if box.TurnID == "" || box.TurnID == w.previousID {
			if time.Now().After(deadline) && w.turnID == "" {
				w.sink.TurnEnd(protocol.TurnEnd{
					SessionID: w.sessionID,
					Status:    "error",
					Error:     "the session did not start a turn",
				})
				return ""
			}
			continue
		}

		if w.turnID != box.TurnID {
			w.turnID = box.TurnID
			w.sent = ""
			w.rev = 0
			w.sink.TurnBegin(protocol.TurnBegin{
				SessionID: w.sessionID,
				TurnID:    box.TurnID,
				QueuedAt:  w.queuedAt,
			})
		}

		w.emitTools(box.TurnID)
		w.emitAgents(box.TurnID)

		if delta := advance(w.sent, box.Text); delta != "" {
			w.sent = box.Text
			w.rev++
			w.sink.TurnDelta(protocol.TurnDelta{
				SessionID: w.sessionID,
				TurnID:    box.TurnID,
				Rev:       w.rev,
				Text:      delta,
			})
		}

		// Only "done" closes the turn: a tool-only step briefly reports a
		// status that is neither streaming nor done, and ending on that would
		// cut the answer short.
		if box.Finished() {
			w.sink.TurnEnd(protocol.TurnEnd{
				SessionID: w.sessionID,
				TurnID:    box.TurnID,
				Status:    "done",
				Text:      box.Text,
				Chunks:    box.Chunks,
				State:     box.State,
			})
			return box.Text
		}
	}
}

// currentText is what the caller gets when a turn is interrupted mid-flight.
func (w *TurnWatcher) currentText() string {
	box, err := w.bridge.ReadOutbox()
	if err != nil || box == nil {
		return w.sent
	}
	return box.Text
}

// emitTools reports tool calls the plugin recorded since the last poll.
func (w *TurnWatcher) emitTools(turnID string) {
	for _, entry := range w.bridge.Tools() {
		if entry.ToolUseID == "" || w.seenTool[entry.ToolUseID] {
			continue
		}
		w.seenTool[entry.ToolUseID] = true
		w.sink.ToolBegin(protocol.ToolBegin{
			SessionID: w.sessionID,
			TurnID:    turnID,
			ToolUseID: entry.ToolUseID,
			Tool:      entry.Tool,
			Input:     entry.Input,
		})
		if entry.Status == "done" || entry.Status == "error" || len(entry.Output) > 0 {
			w.sink.ToolEnd(protocol.ToolEnd{
				SessionID: w.sessionID,
				ToolUseID: entry.ToolUseID,
				OK:        entry.Status != "error",
				Output:    entry.Output,
			})
		}
	}
}

// advance returns the part of next that has not been reported yet.
//
// The plugin keeps only the last 20,000 characters of a turn, so next is not
// always an extension of prev: once the head is dropped, the overlap has to be
// found before the difference. A long identical prefix is the normal case and
// is handled without a scan.
func advance(prev, next string) string {
	if next == prev {
		return ""
	}
	if strings.HasPrefix(next, prev) {
		return next[len(prev):]
	}
	// Longest suffix of prev that is also a prefix of next.
	max := len(prev)
	if len(next) < max {
		max = len(next)
	}
	for n := max; n > 0; n-- {
		if prev[len(prev)-n:] == next[:n] {
			return next[n:]
		}
	}
	return next
}

// emitAgents reports the subagents running beside the main turn.
//
// The plugin writes them while they work; the connector forwards only when the
// set changes, because a status line that redraws on every poll is noise.
func (w *TurnWatcher) emitAgents(turnID string) {
	entries := w.bridge.Agents()
	key := ""
	for _, entry := range entries {
		key += entry.AgentID + ":" + entry.Status + ":" + entry.Label + ";"
	}
	if key == w.agentsKey {
		return
	}
	w.agentsKey = key

	agents := make([]protocol.AgentRunning, 0, len(entries))
	for _, entry := range entries {
		agents = append(agents, protocol.AgentRunning{
			AgentID:   entry.AgentID,
			Label:     entry.Label,
			Status:    entry.Status,
			StartedAt: entry.StartedAt,
			UpdatedAt: entry.UpdatedAt,
		})
	}
	w.sink.AgentActivity(protocol.AgentActivity{SessionID: w.sessionID, Agents: agents})
}
