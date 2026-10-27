package connector

import (
	"context"
	"log/slog"
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
//
// It is a variable so a test can shorten a wait that would otherwise take two
// minutes to observe.
var pickupTimeout = 2 * time.Minute

// turnStallTTL is how long a running turn may show no sign of life before the
// connector declares it dead.
//
// The plugin's own heartbeat is the signal, and a long tool call can keep the
// Claude Code process busy long enough to miss several beats while the turn is
// genuinely still running. Ending the turn on the first missed heartbeat is what
// made the phone stop mid-turn while the terminal kept working; the phone only
// gives up once the desktop has been silent for this long.
var turnStallTTL = 3 * time.Minute

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
	messageID     string
	previousID    string
	queuedAt      int64
	requestScoped bool
	log           *slog.Logger
	// Closed, when set, reports that the person ended this conversation's
	// terminal from the phone. The watcher then closes the turn instead of
	// polling for an answer that can never arrive.
	Closed func() bool

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
	// Liveness belongs to this conversation, not to the machine. The shared
	// heartbeat is rewritten by every Claude process, so it kept a dead
	// session's watcher polling forever while another plugin was still alive.
	alive := func() bool {
		if w.externalSessionID != "" {
			return w.bridge.Live(w.externalSessionID, bridge.DefaultLiveTTL)
		}
		return w.bridge.Available()
	}
	stalled := func(box *bridge.Outbox) bool {
		if w.turnID == "" {
			return false
		}
		if alive() {
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
		return time.Since(unavailableSince) > turnStallTTL
	}

	for {
		select {
		case <-ctx.Done():
			return w.currentText()
		case <-ticker.C:
		}

		// The person closed the terminal. Nothing will ever finish this turn,
		// so it is ended now rather than three minutes later when the missing
		// heartbeat would have been noticed.
		if w.Closed != nil && w.Closed() {
			w.cancelQueued()
			w.sink.TurnEnd(protocol.TurnEnd{
				SessionID: w.sessionID,
				TurnID:    w.currentTurnID(),
				RequestID: w.messageID,
				Status:    "aborted",
				Error:     "the terminal was closed from the phone",
			})
			return w.currentText()
		}
		if receipt, err := w.bridge.ReadRequestReceipt(w.externalSessionID, w.messageID); err == nil &&
			receipt != nil && receipt.Status == "failed" {
			w.cancelQueued()
			w.sink.TurnEnd(protocol.TurnEnd{
				SessionID: w.sessionID,
				TurnID:    w.messageID,
				RequestID: w.messageID,
				Status:    "error",
				Error:     firstNonEmpty(receipt.Error, "Claude Code did not accept the message"),
			})
			return ""
		}

		box, err := w.readOutbox()
		if w.requestScoped && box == nil && err == nil {
			// The per-request file is the preferred channel, not the only one.
			// A reloaded plugin, a lost correlation or an older build still
			// writes the answer to the per-session outbox; a turn that began
			// after this message was queued is this message's answer, and
			// adopting it beats reporting a two-minute timeout for work the
			// desktop already finished.
			current, currentErr := w.bridge.ReadOutbox(w.externalSessionID)
			if currentErr == nil && current != nil && alive() {
				if current.TurnID != "" && current.TurnID != w.previousID &&
					(current.TS == 0 || current.TS >= w.queuedAt) {
					w.requestScoped = false
					box, err = current, nil
				} else if current.Running() {
					// A prompt may wait behind a long-running terminal turn.
					lastActivity = time.Now()
					deadline = lastActivity.Add(pickupTimeout)
				}
			}
		}
		if err != nil || box == nil {
			if stalled(nil) {
				w.sink.TurnEnd(protocol.TurnEnd{
					SessionID: w.sessionID,
					TurnID:    w.turnID,
					RequestID: w.messageID,
					Status:    "error",
					Error:     "the desktop stopped responding while the turn was running",
				})
				return ""
			}
			if time.Now().After(deadline) && w.turnID == "" {
				if w.log != nil {
					w.log.Error("turn watcher timed out before correlated outbox appeared",
						"session", w.sessionID, "external", w.externalSessionID, "request", w.messageID,
						"request_scoped", w.requestScoped, "shared_turn", outboxTurnID(w.bridge, w.externalSessionID),
					)
				}
				w.cancelQueued()
				w.sink.TurnEnd(protocol.TurnEnd{
					SessionID: w.sessionID,
					TurnID:    w.messageID,
					RequestID: w.messageID,
					Status:    "error",
					Error:     "the desktop did not pick up the message",
				})
				return ""
			}
			continue
		}
		// While another turn is running, the queue is simply waiting its turn:
		// sending a second message mid-answer is normal, and a long tool call
		// can hold the plugin for many minutes without writing a byte. As long
		// as the plugin still answers its heartbeat, that wait is not a
		// failure and the deadline keeps sliding. A plugin that stopped
		// answering keeps the old deadline, so a dead desktop is still
		// reported instead of holding the message forever.
		if box.Running() && box.TurnID != w.turnID {
			if alive() {
				lastActivity = time.Now()
				deadline = lastActivity.Add(pickupTimeout)
			}
		}

		// The outbox still shows the turn that was running when this message
		// was queued, another plugin instance wrote it, or it predates the
		// message itself; none of those is this message's answer.
		stale := box.TurnID == "" || (!w.requestScoped && box.TurnID == w.previousID) ||
			(w.requestScoped && box.RequestID != w.messageID) ||
			// A fallback/shared snapshot still names its message. Accepting one
			// tagged with a different message is how the next queued answer was
			// reported against this one.
			(!w.requestScoped && box.RequestID != "" && box.RequestID != w.messageID) ||
			!box.BelongsTo(w.sessionID, w.externalSessionID) ||
			(w.turnID == "" && box.TS > 0 && box.TS < w.queuedAt)
		if stale {
			if time.Now().After(deadline) && w.turnID == "" {
				if w.log != nil {
					w.log.Error("turn watcher timed out without matching outbox",
						"session", w.sessionID, "external", w.externalSessionID, "request", w.messageID,
						"request_scoped", w.requestScoped, "outbox_request", box.RequestID,
						"outbox_turn", box.TurnID, "outbox_status", box.Status,
					)
				}
				w.cancelQueued()
				w.sink.TurnEnd(protocol.TurnEnd{
					SessionID: w.sessionID,
					TurnID:    w.messageID,
					RequestID: w.messageID,
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
			if w.log != nil {
				w.log.Info("turn watcher correlated outbox",
					"session", w.sessionID, "external", w.externalSessionID, "request", w.messageID,
					"turn", box.TurnID, "outbox_request", box.RequestID, "request_scoped", w.requestScoped,
				)
			}
			w.sink.TurnBegin(protocol.TurnBegin{
				SessionID: w.sessionID,
				TurnID:    box.TurnID,
				RequestID: w.messageID,
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
				RequestID: w.messageID,
				Status:    "done",
				Text:      w.full,
				State:     box.State,
			})
			return w.full
		}
		if box.Aborted() {
			w.sink.TurnEnd(protocol.TurnEnd{
				SessionID: w.sessionID,
				TurnID:    box.TurnID,
				RequestID: w.messageID,
				Status:    "aborted",
			})
			return w.currentText()
		}

		if stalled(box) {
			w.sink.TurnEnd(protocol.TurnEnd{
				SessionID: w.sessionID,
				TurnID:    w.turnID,
				RequestID: w.messageID,
				Status:    "error",
				Error:     "the desktop stopped responding while the turn was running",
			})
			return ""
		}
	}
}

func outboxTurnID(b *bridge.Bridge, externalSessionID string) string {
	box, err := b.ReadOutbox(externalSessionID)
	if err != nil || box == nil {
		return ""
	}
	return box.TurnID
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

// currentTurnID is the turn the watcher would have to close when it stops
// without an answer: the one it adopted, else the one the outbox is showing for
// this conversation (never the stale turn it was queued behind).
func (w *TurnWatcher) currentTurnID() string {
	if w.turnID != "" {
		return w.turnID
	}
	box, err := w.readOutbox()
	if err == nil && box != nil &&
		box.BelongsTo(w.sessionID, w.externalSessionID) && box.TurnID != w.previousID {
		return box.TurnID
	}
	return w.messageID
}

// currentText is what the caller gets when a turn is interrupted mid-flight.
// The accumulated text is preferred over the snapshot, which may have lost its
// head to the plugin's tail-only buffer.
func (w *TurnWatcher) currentText() string {
	if w.full != "" {
		return w.full
	}
	box, err := w.readOutbox()
	if err != nil || box == nil || !box.BelongsTo(w.sessionID, w.externalSessionID) {
		return w.sent
	}
	return box.Text
}

func (w *TurnWatcher) readOutbox() (*bridge.Outbox, error) {
	if w.requestScoped {
		return w.bridge.ReadRequestOutbox(w.externalSessionID, w.messageID)
	}
	return w.bridge.ReadOutbox(w.externalSessionID)
}

// emitTools reports tool calls the plugin recorded since the last poll.
//
// The plugin writes an entry when the tool starts and patches the same entry
// when it finishes, so begin and end are tracked separately: an entry first
// seen while running must still be closed on a later poll.
func (w *TurnWatcher) emitTools(turnID string) {
	entries := w.bridge.Tools(w.sessionID, w.externalSessionID)
	if w.requestScoped {
		entries = w.bridge.RequestTools(w.externalSessionID, w.messageID)
	}
	for _, entry := range entries {
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
// snapshot changes, because a status line that redraws on every poll is noise.
// The live fields are part of the comparison: a new tool, its detail, or more
// streamed text is exactly what the phone needs to see update.
func (w *TurnWatcher) emitAgents(turnID string) {
	entries := w.bridge.Agents(w.sessionID, w.externalSessionID)
	if w.requestScoped {
		entries = w.bridge.RequestAgents(w.externalSessionID, w.messageID)
	}
	key := ""
	for _, entry := range entries {
		key += entry.AgentID + "\x00" +
			entry.Status + "\x00" +
			entry.Label + "\x00" +
			entry.Tool + "\x00" +
			entry.Detail + "\x00" +
			entry.Text + "\x1e"
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
			Tool:      entry.Tool,
			Detail:    entry.Detail,
			Text:      entry.Text,
		})
	}
	w.sink.AgentActivity(protocol.AgentActivity{SessionID: w.sessionID, Agents: agents})
}
