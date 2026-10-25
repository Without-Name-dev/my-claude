package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/bridge"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// heartbeatInterval is how often the connector tells the server it is alive.
// The server treats three missed beats as offline.
const heartbeatInterval = 20 * time.Second

// approvalWait is how long one approval request blocks before answering
// "pending". The plugin's own request gives up at 30s and asks again, so this
// stays just under that.
const approvalWait = 25 * time.Second

// Client is the connector's link to the server.
type Client struct {
	URL   string
	Token string
	// ServerURL is the HTTP origin this connector was paired with. It is the
	// base for the attachment downloads a message can ask for; when empty it
	// is derived from URL.
	ServerURL string
	Name      string
	Bridge    *bridge.Bridge
	Log       *slog.Logger
	// Discovery, when set, tells the server which sessions exist on this machine.
	Discovery *Discovery

	invMu             sync.Mutex
	seenSessions      map[string]int64
	lastFullInventory time.Time

	connMu sync.Mutex
	conn   *websocket.Conn

	pendingMu sync.Mutex
	pending   map[string]*pendingAsk

	// turnSessions maps a runtime turn id to the server session it belongs to,
	// so an approval request that carries only the turn id can be addressed.
	turnMu       sync.RWMutex
	turnSessions map[string]string

	// pendingEnds keeps the turn.end frames that could not be sent, in order,
	// so every final text is replayed after the next hello.
	endMu       sync.Mutex
	pendingEnds []protocol.TurnEnd

	// activeMu guards the work this connector has accepted: the session whose
	// message is being followed, the sessions still queued behind it, and the
	// cancels that arrived before their message started.
	activeMu       sync.Mutex
	activeSession  string
	activeExternal string
	queued         map[string]int
	cancelled      map[string]int

	// turns is drained by a single goroutine, so one turn runs at a time even
	// though the server may submit several.
	turns chan protocol.Submit

	// spawnMu guards spawnRequests: runtime session ids the server asked us to
	// open since the last submit for them. runTurn uses it to prime the new
	// conversation's queue cursor before the message is written.
	spawnMu       sync.Mutex
	spawnRequests map[string]time.Time

	// historyMu guards historyStamps: the transcript size and mtime last sent
	// for a session, so a periodic refresh costs one stat when nothing changed.
	historyMu     sync.Mutex
	historyStamps map[string]historyStamp

	// closedMu guards closed: server sessions whose terminal the person closed
	// from the phone. A turn watcher stops on its next poll instead of waiting
	// for the heartbeat to expire.
	closedMu sync.Mutex
	closed   map[string]time.Time
}

// historyStamp identifies a transcript version by its size and write time.
type historyStamp struct {
	size     int64
	modified int64
}

// pendingAsk is one question awaiting an answer from the phone.
type pendingAsk struct {
	answers chan protocol.ApprovalAnswer
	// settled keeps a late re-ask from blocking again after the person has
	// already answered.
	settled *protocol.ApprovalAnswer
}

// NewClient prepares a client. Run opens the connection.
func NewClient(url, token, name string, b *bridge.Bridge, log *slog.Logger) *Client {
	return &Client{
		URL:           url,
		Token:         token,
		Name:          name,
		Bridge:        b,
		Log:           log,
		pending:       map[string]*pendingAsk{},
		turnSessions:  map[string]string{},
		queued:        map[string]int{},
		cancelled:     map[string]int{},
		turns:         make(chan protocol.Submit, 32),
		spawnRequests: map[string]time.Time{},
		closed:        map[string]time.Time{},
		historyStamps: map[string]historyStamp{},
	}
}

// Run connects and stays connected until ctx ends, backing off between tries.
func (c *Client) Run(ctx context.Context) {
	go c.runTurns(ctx)

	backoff := time.Second
	for ctx.Err() == nil {
		reachedHello, err := c.session(ctx)
		if ctx.Err() != nil {
			return
		}
		if reachedHello {
			// The outage is over, so the next interruption starts from the
			// shortest delay instead of inheriting the previous one.
			backoff = time.Second
		}
		c.Log.Warn("connector link lost", "error", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// session runs one connection until it fails. It reports whether the link
// reached hello, so Run can tell a failed dial from a lost connection.
func (c *Client) session(ctx context.Context) (reachedHello bool, err error) {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, c.URL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer " + c.Token}},
	})
	if err != nil {
		return false, fmt.Errorf("dial: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(8 << 20)

	c.connMu.Lock()
	c.conn = conn
	c.connMu.Unlock()
	defer func() {
		c.connMu.Lock()
		c.conn = nil
		c.connMu.Unlock()
	}()

	bridgeReady, liveSessions := c.bridgeStatus()
	if err := c.send(protocol.TypeHello, protocol.Hello{
		Version:      protocol.ConnectorProtocolVersion,
		Name:         c.Name,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		Capabilities: c.capabilitySet(bridgeReady),
		Runtimes:     []protocol.RuntimeInfo{{Type: "claude", Ready: c.Bridge != nil}},
		LiveSessions: liveSessions,
	}); err != nil {
		return false, err
	}
	reachedHello = true
	c.Log.Info("connector link established", "url", c.URL)
	// Turns that ended while the link was down are replayed now that the
	// server can hear them; turn.end is idempotent there.
	c.replayTurnEnds()

	beatCtx, stopBeat := context.WithCancel(ctx)
	defer stopBeat()
	go c.beat(beatCtx)
	if c.Discovery != nil {
		go c.inventoryLoop(beatCtx)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return reachedHello, err
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			c.Log.Warn("dropping malformed frame", "error", err)
			continue
		}
		c.dispatch(env)
	}
}

// beat keeps the presence record fresh.
// liveWatchInterval is how often presence is re-read between heartbeats.
const liveWatchInterval = time.Second

func (c *Client) beat(ctx context.Context) {
	// The ticker runs faster than the heartbeat so a conversation that starts
	// or stops shows up in the session list within seconds instead of waiting
	// for the next periodic beat. A changed presence set is sent immediately;
	// an unchanged one waits for the heartbeatInterval deadline.
	ticker := time.NewTicker(liveWatchInterval)
	defer ticker.Stop()
	lastSent := time.Now()
	lastKey := ""
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			frame := c.beatFrame()
			key := strings.Join(frame.LiveSessions, ",") + "|" + strconv.FormatBool(frame.Bridge)
			if key != lastKey || time.Since(lastSent) >= heartbeatInterval {
				_ = c.send(protocol.TypeBeat, frame)
				lastSent = time.Now()
				lastKey = key
			}
		}
	}
}

// beatFrame is every heartbeat's payload, including the answer to a ping.
func (c *Client) beatFrame() protocol.Beat {
	bridgeReady, liveSessions := c.bridgeStatus()
	return protocol.Beat{
		Bridge:       bridgeReady,
		LiveSessions: liveSessions,
	}
}

// bridgeStatus reports whether the bridge can accept a delivery and, only if it
// can, the sessions whose plugin stamped itself live.
//
// The two facts travel together: the server refuses a submit unless the bridge
// heartbeat is fresh, so reporting a per-session stamp while that heartbeat is
// stale would paint a green dot on a conversation the person cannot actually
// send to. A nil bridge (delivery switched off) can make no session
// deliverable, so nothing is reported in that case either.
func (c *Client) bridgeStatus() (ready bool, live []string) {
	if c.Bridge == nil {
		return false, nil
	}
	if !c.Bridge.Available() {
		return false, nil
	}
	return true, c.Bridge.LiveSessions(bridge.DefaultLiveTTL)
}

// capabilitySet is what hello advertises. "bridge" and "bridgeConfigured" are
// separate on purpose: the first says a plugin would pick a submit up right
// now, the second says this connector can queue one at all. A Server decides
// whether a message with no live plugin can still be made to land from the
// second, and whether asking for a terminal is worthwhile from the third.
func (c *Client) capabilitySet(bridgeReady bool) map[string]bool {
	return map[string]bool{
		protocol.CapabilityBridge:           bridgeReady,
		protocol.CapabilityBridgeConfigured: c.Bridge != nil,
		protocol.CapabilitySessionOpen:      spawnAllowed() && sessionOpenSupported(),
	}
}

// dispatch handles one inbound frame.
func (c *Client) dispatch(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeSubmit:
		var msg protocol.Submit
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			c.Log.Warn("bad submit", "error", err)
			return
		}
		c.noteQueued(msg.SessionID)
		select {
		case c.turns <- msg:
		default:
			// The queue is drained one turn at a time, so a full one means
			// this message would sit behind tens of minutes of work. Report it
			// as failed instead of dropping an acknowledged submit silently.
			c.finishQueued(msg.SessionID)
			c.Log.Warn("turn queue full; failing submit", "session", msg.SessionID, "request", msg.RequestID)
			c.TurnEnd(protocol.TurnEnd{
				SessionID: msg.SessionID,
				TurnID:    msg.RequestID,
				Status:    "error",
				Error:     "the connector is still working through earlier messages; send this one again",
			})
		}
	case protocol.TypeSessionClose:
		c.handleSessionClose(env.Data)
	case protocol.TypeAbort:
		var msg protocol.Abort
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		if c.Bridge == nil {
			return
		}
		external, queued, ok := c.abortTarget(msg.SessionID, msg.ExternalSessionID)
		if !ok {
			c.Log.Debug("ignoring abort for a session this connector is not driving", "session", msg.SessionID)
			return
		}
		if queued {
			// The message has not reached the plugin yet; runTurn drops it.
			c.Log.Debug("cancelled a queued message", "session", msg.SessionID)
			return
		}
		if err := c.Bridge.RequestAbort(external); err != nil {
			c.Log.Warn("could not write the abort request", "error", err)
		}
	case protocol.TypeApprovalAnswer:
		var msg protocol.ApprovalAnswer
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		c.deliverApproval(msg)
	case protocol.TypeRPCRequest:
		c.handleRPC(env.Data)
	case protocol.TypeHistoryRequest:
		var msg protocol.HistoryRequest
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		c.sendHistory(msg)
	case protocol.TypeTerminal:
		c.handleTerminalFrame(env.Data)
	case protocol.TypeSessionOpen:
		c.handleSessionOpen(env.Data)
	case protocol.TypePing:
		_ = c.send(protocol.TypeBeat, c.beatFrame())
	default:
		c.Log.Debug("ignoring unknown frame", "type", env.Type)
	}
}

// runTurns processes submitted messages one at a time.
func (c *Client) runTurns(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.turns:
			c.runTurn(ctx, msg)
		}
	}
}

// runTurn queues one message into the plugin and follows it to completion.
func (c *Client) runTurn(ctx context.Context, msg protocol.Submit) {
	defer c.finishQueued(msg.SessionID)
	if c.Bridge == nil {
		c.Log.Warn("submit received but the plugin bridge is not configured")
		c.TurnEnd(protocol.TurnEnd{
			SessionID: msg.SessionID,
			TurnID:    msg.RequestID,
			Status:    "error",
			Error:     "the connector has no plugin bridge configured, so the message cannot run on the desktop",
		})
		return
	}
	// A stop that arrived while this message waited behind another turn is
	// answered here; the prompt never reaches the plugin.
	if c.consumeAbort(msg.SessionID) {
		c.TurnEnd(protocol.TurnEnd{
			SessionID: msg.SessionID,
			TurnID:    msg.RequestID,
			Status:    "aborted",
		})
		return
	}
	// The server asked us to open this conversation and the first message for
	// it is being queued now. Pin the new plugin's cursor to the current queue
	// head so a later global-cursor advance cannot make it skip this message.
	if c.consumeSpawnRequest(msg.ExternalSessionID) {
		if err := c.Bridge.SeedSessionCursor(msg.ExternalSessionID); err != nil {
			c.Log.Warn("could not prime the spawned session's queue cursor",
				"session", msg.SessionID, "external", msg.ExternalSessionID, "error", err)
		}
	}
	action := msg.Action
	if action == "" {
		action = "prompt"
	}
	if action == "command" {
		// A command carries a name and an argument tail; the plugin runs it
		// through $.command.run instead of submitting it as prompt text. It
		// never writes a turn to the outbox, so there is nothing to watch.
		name, args := splitCommand(msg.Text)
		if _, err := c.Bridge.SubmitCommand(msg.SessionID, msg.ExternalSessionID, msg.RequestID, name, args); err != nil {
			c.Log.Warn("could not queue the command", "error", err)
		}
		return
	}
	// Attached files reach the model as local paths: the bridge carries text,
	// and Claude Code opens each path with its Read tool.
	text := msg.Text
	if len(msg.Attachments) > 0 {
		files, failures := c.materializeAttachments(ctx, msg)
		text = promptWithAttachments(text, files, failures)
	}
	sub, err := c.Bridge.Submit(msg.SessionID, msg.ExternalSessionID, msg.RequestID, text, action)
	if err != nil {
		// The server already acknowledged this message and painted the user's
		// bubble; a log line would leave the phone waiting forever.
		c.Log.Warn("could not queue the message", "error", err)
		c.TurnEnd(protocol.TurnEnd{
			SessionID: msg.SessionID,
			TurnID:    msg.RequestID,
			Status:    "error",
			Error:     "the desktop did not accept the message",
		})
		return
	}
	c.setActive(msg.SessionID, msg.ExternalSessionID)
	defer c.clearActive(msg.SessionID)
	// A stop that landed while the message was being written was recorded
	// rather than sent, because the plugin was not running the turn yet.
	if c.consumeAbort(msg.SessionID) {
		if err := c.Bridge.RequestAbort(msg.ExternalSessionID); err != nil {
			c.Log.Warn("could not write the abort request", "error", err)
		}
	}
	watcher := NewTurnWatcher(c.Bridge, c, msg.SessionID, msg.ExternalSessionID, msg.RequestID, sub)
	// This message is the request to run again; a close recorded for the
	// previous terminal must not end it.
	c.clearSessionClosed(msg.SessionID)
	watcher.Closed = func() bool { return c.sessionWasClosed(msg.SessionID) }
	watcher.Run(ctx)
}

// TurnBegin implements TurnSink.
func (c *Client) TurnBegin(msg protocol.TurnBegin) {
	if msg.TurnID != "" && msg.SessionID != "" {
		c.turnMu.Lock()
		c.turnSessions[msg.TurnID] = msg.SessionID
		c.turnMu.Unlock()
	}
	c.emit(protocol.TypeTurnBegin, msg)
}

// TurnDelta implements TurnSink.
func (c *Client) TurnDelta(msg protocol.TurnDelta) { c.emit(protocol.TypeTurnDelta, msg) }

// maxPendingEnds bounds what a long outage can hold. Several turns can finish
// while the link is down, so one slot would lose all but the last answer.
const maxPendingEnds = 64

// TurnEnd implements TurnSink.
//
// The final text lives only in this frame, so a send failure is not just
// logged: the frame is kept and replayed after the next hello.
func (c *Client) TurnEnd(msg protocol.TurnEnd) {
	if err := c.send(protocol.TypeTurnEnd, msg); err != nil {
		c.endMu.Lock()
		c.pendingEnds = append(c.pendingEnds, msg)
		if len(c.pendingEnds) > maxPendingEnds {
			c.pendingEnds = c.pendingEnds[len(c.pendingEnds)-maxPendingEnds:]
		}
		c.endMu.Unlock()
		c.Log.Debug("could not forward frame", "type", protocol.TypeTurnEnd, "error", err)
	}
	c.forgetTurn(msg.TurnID)
}

// ToolBegin implements TurnSink.
func (c *Client) ToolBegin(msg protocol.ToolBegin) { c.emit(protocol.TypeToolBegin, msg) }

// ToolEnd implements TurnSink.
func (c *Client) ToolEnd(msg protocol.ToolEnd) { c.emit(protocol.TypeToolEnd, msg) }

// AgentActivity implements TurnSink.
func (c *Client) AgentActivity(msg protocol.AgentActivity) {
	c.emit(protocol.TypeAgentActivity, msg)
}

func (c *Client) emit(frameType string, value any) {
	if err := c.send(frameType, value); err != nil {
		c.Log.Debug("could not forward frame", "type", frameType, "error", err)
	}
}

// replayTurnEnds re-sends the turn.end frames that a disconnect swallowed,
// oldest first, and stops at the first one that fails again.
func (c *Client) replayTurnEnds() {
	for {
		c.endMu.Lock()
		if len(c.pendingEnds) == 0 {
			c.endMu.Unlock()
			return
		}
		msg := c.pendingEnds[0]
		c.endMu.Unlock()

		if err := c.send(protocol.TypeTurnEnd, msg); err != nil {
			c.Log.Debug("could not replay turn.end", "error", err)
			return
		}

		c.endMu.Lock()
		if len(c.pendingEnds) > 0 {
			c.pendingEnds = c.pendingEnds[1:]
		}
		c.endMu.Unlock()
	}
}

// noteQueued records that a submit is waiting in the connector's queue, so an
// abort addressed to it is not mistaken for one meant for another session.
func (c *Client) noteQueued(sessionID string) {
	if sessionID == "" {
		return
	}
	c.activeMu.Lock()
	c.queued[sessionID]++
	c.activeMu.Unlock()
}

// finishQueued records that a submit left the connector's queue, consuming any
// cancel that never matched a message.
func (c *Client) finishQueued(sessionID string) {
	if sessionID == "" {
		return
	}
	c.activeMu.Lock()
	if c.queued[sessionID] <= 1 {
		delete(c.queued, sessionID)
		delete(c.cancelled, sessionID)
	} else {
		c.queued[sessionID]--
	}
	c.activeMu.Unlock()
}

// setActive records which submitted session the connector is following and
// the runtime id the plugin knows it by.
func (c *Client) setActive(sessionID, externalSessionID string) {
	c.activeMu.Lock()
	c.activeSession = sessionID
	c.activeExternal = externalSessionID
	c.activeMu.Unlock()
}

// clearActive forgets the session unless another submit has already claimed
// it.
func (c *Client) clearActive(sessionID string) {
	c.activeMu.Lock()
	if c.activeSession == sessionID {
		c.activeSession, c.activeExternal = "", ""
	}
	c.activeMu.Unlock()
}

// abortTarget resolves an abort to the runtime session to stop. queued reports
// that the message has not started yet, in which case the cancel is recorded
// for runTurn instead of being written to the plugin.
func (c *Client) abortTarget(sessionID, externalSessionID string) (target string, queued, ok bool) {
	if sessionID == "" {
		return "", false, false
	}
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	if c.activeSession == sessionID {
		if externalSessionID != "" {
			return externalSessionID, false, true
		}
		return c.activeExternal, false, true
	}
	if _, ok := c.queued[sessionID]; !ok {
		return "", false, false
	}
	c.cancelled[sessionID]++
	return externalSessionID, true, true
}

// consumeAbort reports whether a stop is waiting for this session and takes it
// off the books.
func (c *Client) consumeAbort(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	switch c.cancelled[sessionID] {
	case 0:
		return false
	case 1:
		delete(c.cancelled, sessionID)
	default:
		c.cancelled[sessionID]--
	}
	return true
}

// sessionForTurn resolves the server session that owns a runtime turn.
func (c *Client) sessionForTurn(turnID string) string {
	if turnID == "" {
		return ""
	}
	c.turnMu.RLock()
	defer c.turnMu.RUnlock()
	return c.turnSessions[turnID]
}

// forgetTurn drops a finished turn's session mapping.
func (c *Client) forgetTurn(turnID string) {
	if turnID == "" {
		return
	}
	c.turnMu.Lock()
	delete(c.turnSessions, turnID)
	c.turnMu.Unlock()
}

// Ask is the ApprovalServer callback: it forwards the plugin's question to the
// server and waits for the person.
//
// A nil answer means "ask again later"; the plugin treats that as no decision
// and re-issues the request.
func (c *Client) Ask(ctx context.Context, request map[string]any) (map[string]any, error) {
	toolUseID := stringField(request, "toolUseId")
	if toolUseID == "" {
		return nil, errors.New("approval request has no toolUseId")
	}

	// The pane answers by posting the decision to this same endpoint. Settle
	// the hook that is waiting on this tool instead of treating the decision
	// as another question.
	if decision := stringField(request, "decision"); decision == "allow" || decision == "deny" {
		return c.settle(toolUseID, protocol.ApprovalAnswer{ToolUseID: toolUseID, Decision: decision}), nil
	}

	kind := stringField(request, "kind")
	if kind == "" {
		kind = "permission"
	}
	turnID := stringField(request, "turnId")
	// The turn.begin the watcher forwarded is authoritative. The installed
	// plugin sends only the turn id; newer builds may also send a session id,
	// which covers a re-ask that arrives before the watcher saw the turn.
	// The runtime's own id is forwarded as well, so a question that cannot be
	// mapped here can still be addressed by the server.
	externalSessionID := stringField(request, "externalSessionId")
	sessionID := c.sessionForTurn(turnID)
	if sessionID == "" {
		sessionID = stringField(request, "sessionId")
	}

	c.pendingMu.Lock()
	ask, exists := c.pending[toolUseID]
	if exists && ask.settled != nil {
		settled := *ask.settled
		c.pendingMu.Unlock()
		return answerPayload(settled), nil
	}
	if !exists {
		ask = &pendingAsk{answers: make(chan protocol.ApprovalAnswer, 1)}
		c.pending[toolUseID] = ask
	}
	c.pendingMu.Unlock()

	// Re-send while the question is unanswered. The plugin asks again after
	// its own timeout and the server keys the notice by toolUseId, so a repeat
	// is idempotent and recovers a frame lost to a disconnect.
	raw, _ := json.Marshal(request["input"])
	if err := c.send(protocol.TypeApprovalRequest, protocol.ApprovalRequest{
		SessionID:         sessionID,
		ExternalSessionID: externalSessionID,
		TurnID:            turnID,
		ToolUseID:         toolUseID,
		Tool:              stringField(request, "tool"),
		Kind:              kind,
		Input:             raw,
	}); err != nil {
		return nil, err
	}

	select {
	case answer := <-ask.answers:
		c.pendingMu.Lock()
		ask.settled = &answer
		c.pendingMu.Unlock()
		return answerPayload(answer), nil
	case <-time.After(approvalWait):
		return map[string]any{"decision": "pending"}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// settle stores a decision for a pending question and hands it to the hook
// that is waiting right now, if any.
func (c *Client) settle(toolUseID string, answer protocol.ApprovalAnswer) map[string]any {
	c.pendingMu.Lock()
	ask, ok := c.pending[toolUseID]
	if ok {
		ask.settled = &answer
	}
	c.pendingMu.Unlock()
	if ok {
		select {
		case ask.answers <- answer:
		default:
		}
	}
	return answerPayload(answer)
}

// deliverApproval hands a decision to the waiting Ask, if any.
func (c *Client) deliverApproval(msg protocol.ApprovalAnswer) {
	c.pendingMu.Lock()
	ask, ok := c.pending[msg.ToolUseID]
	var settled *protocol.ApprovalAnswer
	if ok && ask.settled != nil {
		settled = ask.settled
	}
	c.pendingMu.Unlock()

	if settled != nil {
		return // already answered; a duplicate is harmless to ignore
	}
	if !ok {
		return
	}
	select {
	case ask.answers <- msg:
	default:
	}
}

func answerPayload(answer protocol.ApprovalAnswer) map[string]any {
	out := map[string]any{"decision": answer.Decision}
	if len(answer.UpdatedInput) > 0 {
		var updated map[string]any
		if err := json.Unmarshal(answer.UpdatedInput, &updated); err == nil {
			out["updatedInput"] = updated
		}
	}
	if len(answer.Answers) > 0 {
		out["answers"] = answer.Answers
	}
	if answer.Message != "" {
		out["message"] = answer.Message
	}
	return out
}

// send writes one envelope.
func (c *Client) send(frameType string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	env := protocol.Envelope{Type: frameType, Data: data}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}

	c.connMu.Lock()
	conn := c.conn
	c.connMu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return conn.Write(ctx, websocket.MessageText, body)
}

func stringField(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// inventoryInterval is how often the local transcript tree is rescanned. A new
// session shows up on the phone within about a minute of being started.
const inventoryInterval = 60 * time.Second

// fullInventoryEvery bounds how long an incremental-only stream may run before
// the connector restates everything.
const fullInventoryEvery = 10 * time.Minute

// inventoryLoop reports the sessions on this machine.
func (c *Client) inventoryLoop(ctx context.Context) {
	c.sendInventory()
	ticker := time.NewTicker(inventoryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.sendInventory()
		}
	}
}

// sendInventory scans and forwards what changed since the last report.
//
// A machine can hold hundreds of transcripts, and each record costs the server
// a project lookup and an upsert. Sending only the differences keeps a
// once-a-minute scan from competing with a live turn for the write lock; the
// occasional full report repairs anything a dropped frame hid.
func (c *Client) sendInventory() {
	if c.Discovery == nil {
		return
	}
	records, err := c.Discovery.Scan()
	if err != nil {
		c.Log.Warn("could not scan transcripts", "error", err)
		return
	}

	c.invMu.Lock()
	if c.seenSessions == nil {
		c.seenSessions = map[string]int64{}
	}
	full := time.Since(c.lastFullInventory) >= fullInventoryEvery
	changed := make([]protocol.InventorySession, 0, 16)
	seen := make(map[string]int64, len(records))
	for _, record := range records {
		seen[record.ExternalID] = record.UpdatedAt
		if previous, ok := c.seenSessions[record.ExternalID]; ok && previous == record.UpdatedAt && !full {
			continue
		}
		changed = append(changed, protocol.InventorySession{
			ExternalID: record.ExternalID,
			CWD:        record.CWD,
			Title:      record.Title,
			Model:      record.Model,
			UpdatedAt:  record.UpdatedAt,
		})
	}
	removed := make([]string, 0)
	if full {
		for id := range c.seenSessions {
			if _, ok := seen[id]; !ok {
				removed = append(removed, id)
			}
		}
	}
	c.invMu.Unlock()

	if len(changed) == 0 && len(removed) == 0 && !full {
		return
	}
	if err := c.send(protocol.TypeInventory, protocol.Inventory{
		Sessions: changed, Full: full, Removed: removed,
	}); err != nil {
		// Not delivered, so not delivered: committing the records before the
		// send acknowledged them lost whatever was discovered while the socket
		// was down, until the next full scan ten minutes later.
		c.Log.Debug("could not send inventory", "error", err)
		return
	}
	c.invMu.Lock()
	c.seenSessions = seen
	if full {
		c.lastFullInventory = time.Now()
	}
	c.invMu.Unlock()
	c.Log.Info("reported sessions", "changed", len(changed), "removed", len(removed), "full", full)
}

// sendHistory answers a request for a session's transcript.
func (c *Client) sendHistory(req protocol.HistoryRequest) {
	if c.Discovery == nil {
		return
	}
	path, ok := c.Discovery.PathFor(req.ExternalID)
	if !ok {
		c.Log.Debug("no transcript for session", "external", req.ExternalID)
		return
	}
	// The server now asks live conversations for their transcript every few
	// seconds. Reading a long transcript for an unchanged file would be pure
	// cost, so the last sent size and mtime decide.
	stamp, haveStamp := historyStamp{}, false
	if info, err := os.Stat(path); err == nil {
		stamp = historyStamp{size: info.Size(), modified: info.ModTime().UnixMilli()}
		haveStamp = true
		c.historyMu.Lock()
		previous, seen := c.historyStamps[req.SessionID]
		c.historyMu.Unlock()
		if seen && previous == stamp {
			return
		}
	}
	items, err := ReadHistory(path, req.Limit)
	if err != nil {
		c.Log.Warn("could not read transcript", "error", err)
		return
	}
	frame := protocol.History{SessionID: req.SessionID, Items: items}
	if haveStamp {
		frame.UpdatedAt = stamp.modified
	}
	if err := c.send(protocol.TypeHistory, frame); err != nil {
		c.Log.Debug("could not send history", "error", err)
		return
	}
	if haveStamp {
		c.historyMu.Lock()
		c.historyStamps[req.SessionID] = stamp
		c.historyMu.Unlock()
	}
	c.Log.Info("sent history", "session", req.SessionID, "items", len(items))
}

// splitCommand separates "/name rest of the line" into its two parts.
func splitCommand(text string) (string, string) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(text), "/"))
	if trimmed == "" {
		return "", ""
	}
	if space := strings.IndexAny(trimmed, " \t"); space >= 0 {
		return trimmed[:space], strings.TrimSpace(trimmed[space+1:])
	}
	return trimmed, ""
}
