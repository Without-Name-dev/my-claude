package connector

import (
	"context"
	"strings"
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
const pickupTimeout = 2 * time.Minute

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
	bridge            *bridge.Bridge
	sink              TurnSink
	sessionID         string
	externalSessionID string
	// messageID is the submit's request id; the queue entry carries it as its
	// id, so it is how an undelivered prompt is withdrawn again.
	messageID  string
	previousID string
	queuedAt   int64

	turnID    string
	sent      string
	full      string
	rev       int
	seenTool  map[string]bool
	endedTool map[string]bool
	agentsKey string
}

// NewTurnWatcher prepares a watcher for a message that was just queued.
func NewTurnWatcher(b *bridge.Bridge, sink TurnSink, sessionID, externalSessionID, messageID string, sub *bridge.Submission) *TurnWatcher {
	w := &TurnWatcher{
		bridge:            b,
		sink:              sink,
		sessionID:         sessionID,
		externalSessionID: externalSessionID,
		messageID:         messageID,
		seenTool:          map[string]bool{},
		endedTool:         map[string]bool{},
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

	// A turn that has been picked up but whose plugin has gone away never
	// closes its outbox entry. Once the heartbeat is past its TTL and the
	// snapshot has stopped changing, report the failure instead of holding
	// every later submit behind this watcher.
	var unavailableSince time.Time
	var stalledTS int64
	var behindTurn string
	var behindTS int64
	stalled := func(box *bridge.Outbox) bool {
		if w.turnID == "" {
			return false
		}
		if w.bridge.Available() {
			unavailableSince = time.Time{}
			return false
		}
		ts := int64(0)
		if box != nil {
			ts = box.TS
		}
		if unavailableSince.IsZero() || ts != stalledTS {
			unavailableSince = time.Now()
			stalledTS = ts
			return false
		}
		return time.Since(unavailableSince) > bridge.DefaultHeartbeatTTL
	}

	for {
		select {
		case <-ctx.Done():
			return w.currentText()
		case <-ticker.C:
		}

		box, err := w.bridge.ReadOutbox()
		if err != nil || box == nil {
			if stalled(nil) {
				w.sink.TurnEnd(protocol.TurnEnd{
					SessionID: w.sessionID,
					TurnID:    w.turnID,
					Status:    "error",
					Error:     "the desktop stopped responding while the turn was running",
				})
				return ""
			}
			if time.Now().After(deadline) && w.turnID == "" {
				w.cancelQueued()
				w.sink.TurnEnd(protocol.TurnEnd{
					SessionID: w.sessionID,
					TurnID:    w.messageID,
					Status:    "error",
					Error:     "the desktop did not pick up the message",
				})
				return ""
			}
			continue
		}
		// While another turn is streaming, the queue is simply waiting its
		// turn; that is activity, not a stalled pickup. Only a snapshot that
		// changed counts, though: a turn that has stopped producing output
		// must not hold this message behind it forever.
		if box.Running() && box.TurnID != w.turnID {
			if box.TurnID != behindTurn || box.TS != behindTS {
				behindTurn, behindTS = box.TurnID, box.TS
				lastActivity = time.Now()
				deadline = lastActivity.Add(pickupTimeout)
			}
		}

		// The outbox still shows the turn that was running when this message
		// was queued, another plugin instance wrote it, or it predates the
		// message itself; none of those is this message's answer.
		stale := box.TurnID == "" || box.TurnID == w.previousID ||
			!box.BelongsTo(w.sessionID, w.externalSessionID) ||
			(w.turnID == "" && box.TS > 0 && box.TS < w.queuedAt)
		if stale {
			if time.Now().After(deadline) && w.turnID == "" {
				w.cancelQueued()
				w.sink.TurnEnd(protocol.TurnEnd{
					SessionID: w.sessionID,
					TurnID:    w.messageID,
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
			w.full = ""
			w.rev = 0
			w.seenTool = map[string]bool{}
			w.endedTool = map[string]bool{}
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
			w.full += delta
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
			// The plugin keeps only the tail of a long answer, so the final
			// frame carries what was accumulated from the deltas instead of
			// the truncated snapshot; otherwise the phone's stored answer is
			// overwritten with a head-less fragment.
			w.sink.TurnEnd(protocol.TurnEnd{
				SessionID: w.sessionID,
				TurnID:    box.TurnID,
				Status:    "done",
				Text:      w.full,
				Chunks:    box.Chunks,
				State:     box.State,
			})
			return w.full
		}

		if stalled(box) {
			w.sink.TurnEnd(protocol.TurnEnd{
				SessionID: w.sessionID,
				TurnID:    w.turnID,
				Status:    "error",
				Error:     "the desktop stopped responding while the turn was running",
			})
			return ""
		}
	}
}

// cancelQueued withdraws a prompt the plugin never picked up. The phone has
// already been told it failed, so leaving the file in the queue would let a
// later plugin instance run it long after the fact.
func (w *TurnWatcher) cancelQueued() {
	if w.messageID == "" {
		return
	}
	_ = w.bridge.CancelQueued(w.messageID)
}

// currentText is what the caller gets when a turn is interrupted mid-flight.
// The accumulated text is preferred over the snapshot, which may have lost its
// head to the plugin's tail-only buffer.
func (w *TurnWatcher) currentText() string {
	if w.full != "" {
		return w.full
	}
	box, err := w.bridge.ReadOutbox()
	if err != nil || box == nil || !box.BelongsTo(w.sessionID, w.externalSessionID) {
		return w.sent
	}
	return box.Text
}

// emitTools reports tool calls the plugin recorded since the last poll.
//
// The plugin writes an entry when the tool starts and patches the same entry
// when it finishes, so begin and end are tracked separately: an entry first
// seen while running must still be closed on a later poll.
func (w *TurnWatcher) emitTools(turnID string) {
	for _, entry := range w.bridge.Tools(w.sessionID, w.externalSessionID) {
		if entry.ID == "" {
			continue
		}
		if !w.seenTool[entry.ID] {
			w.seenTool[entry.ID] = true
			w.sink.ToolBegin(protocol.ToolBegin{
				SessionID: w.sessionID,
				TurnID:    turnID,
				ToolUseID: entry.ID,
				Tool:      entry.Tool,
				Input:     entry.Input,
			})
		}
		if entry.Done && !w.endedTool[entry.ID] {
			w.endedTool[entry.ID] = true
			w.sink.ToolEnd(protocol.ToolEnd{
				SessionID: w.sessionID,
				ToolUseID: entry.ID,
				OK:        !entry.IsError,
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
	entries := w.bridge.Agents(w.sessionID, w.externalSessionID)
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
