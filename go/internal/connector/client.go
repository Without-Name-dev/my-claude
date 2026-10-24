package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
	URL    string
	Token  string
	Name   string
	Bridge *bridge.Bridge
	Log    *slog.Logger
	// Discovery, when set, tells the server which sessions exist on this machine.
	Discovery *Discovery

	invMu             sync.Mutex
	seenSessions      map[string]int64
	lastFullInventory time.Time

	connMu sync.Mutex
	conn   *websocket.Conn
	seq    atomic.Uint64

	pendingMu sync.Mutex
	pending   map[string]*pendingAsk

	// turnSessions maps a runtime turn id to the server session it belongs to,
	// so an approval request that carries only the turn id can be addressed.
	turnMu       sync.RWMutex
	turnSessions map[string]string

	// pendingEnd keeps the last turn.end that could not be sent, so it can be
	// replayed after the next hello.
	endMu      sync.Mutex
	pendingEnd *protocol.TurnEnd

	// activeMu guards the session whose submitted message is being followed,
	// which is how an abort is routed to the plugin instance that owns it.
	activeMu       sync.Mutex
	activeSession  string
	activeExternal string

	// turns is drained by a single goroutine, so one turn runs at a time even
	// though the server may submit several.
	turns chan protocol.Submit
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
		URL:          url,
		Token:        token,
		Name:         name,
		Bridge:       b,
		Log:          log,
		pending:      map[string]*pendingAsk{},
		turnSessions: map[string]string{},
		turns:        make(chan protocol.Submit, 32),
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

	if err := c.send(protocol.TypeHello, protocol.Hello{
		Version:      protocol.ConnectorProtocolVersion,
		Name:         c.Name,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		Capabilities: map[string]bool{"bridge": c.Bridge != nil && c.Bridge.Available()},
		Runtimes:     []protocol.RuntimeInfo{{Type: "claude", Ready: c.Bridge != nil}},
	}); err != nil {
		return false, err
	}
	reachedHello = true
	c.Log.Info("connector link established", "url", c.URL)
	// A turn that ended while the link was down is replayed now that the
	// server can hear it; turn.end is idempotent there.
	c.replayTurnEnd()

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
func (c *Client) beat(ctx context.Context) {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = c.send(protocol.TypeBeat, map[string]any{
				"bridge": c.Bridge != nil && c.Bridge.Available(),
			})
		}
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
		select {
		case c.turns <- msg:
		default:
			c.Log.Warn("turn queue full; dropping submit", "session", msg.SessionID)
		}
	case protocol.TypeAbort:
		var msg protocol.Abort
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		if c.Bridge == nil {
			return
		}
		external, ok := c.abortTarget(msg.SessionID)
		if !ok {
			c.Log.Debug("ignoring abort for a session this connector is not driving", "session", msg.SessionID)
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
	case protocol.TypePing:
		_ = c.send(protocol.TypeBeat, map[string]any{"pong": true})
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
	if c.Bridge == nil {
		c.Log.Warn("submit received but the plugin bridge is not configured")
		return
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
		if _, err := c.Bridge.SubmitCommand(msg.SessionID, msg.RequestID, name, args); err != nil {
			c.Log.Warn("could not queue the command", "error", err)
		}
		return
	}
	sub, err := c.Bridge.Submit(msg.SessionID, msg.ExternalSessionID, msg.RequestID, msg.Text, action)
	if err != nil {
		c.Log.Warn("could not queue the message", "error", err)
		return
	}
	c.setActive(msg.SessionID, msg.ExternalSessionID)
	defer c.clearActive(msg.SessionID)
	watcher := NewTurnWatcher(c.Bridge, c, msg.SessionID, msg.ExternalSessionID, sub)
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

// TurnEnd implements TurnSink.
//
// The final text lives only in this frame, so a send failure is not just
// logged: the frame is kept and replayed after the next hello.
func (c *Client) TurnEnd(msg protocol.TurnEnd) {
	if err := c.send(protocol.TypeTurnEnd, msg); err != nil {
		c.endMu.Lock()
		c.pendingEnd = &msg
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

// replayTurnEnd re-sends the final turn.end that a disconnect swallowed.
func (c *Client) replayTurnEnd() {
	c.endMu.Lock()
	msg := c.pendingEnd
	c.pendingEnd = nil
	c.endMu.Unlock()
	if msg == nil {
		return
	}
	if err := c.send(protocol.TypeTurnEnd, *msg); err != nil {
		c.endMu.Lock()
		if c.pendingEnd == nil {
			c.pendingEnd = msg
		}
		c.endMu.Unlock()
		c.Log.Debug("could not replay turn.end", "error", err)
	}
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

// abortTarget returns the runtime session id to stamp on an abort, and whether
// the abort is addressed to the message this connector is driving.
func (c *Client) abortTarget(sessionID string) (string, bool) {
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	if sessionID == "" || c.activeSession != sessionID {
		return "", false
	}
	return c.activeExternal, true
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
		SessionID: sessionID,
		TurnID:    turnID,
		ToolUseID: toolUseID,
		Tool:      stringField(request, "tool"),
		Kind:      kind,
		Input:     raw,
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
	env := protocol.Envelope{Type: frameType, Seq: c.seq.Add(1), Data: data}
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
		c.lastFullInventory = time.Now()
	}
	c.seenSessions = seen
	c.invMu.Unlock()

	if len(changed) == 0 && len(removed) == 0 && !full {
		return
	}
	if err := c.send(protocol.TypeInventory, protocol.Inventory{
		Sessions: changed, Full: full, Removed: removed,
	}); err != nil {
		c.Log.Debug("could not send inventory", "error", err)
		return
	}
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
	items, err := ReadHistory(path, req.Limit)
	if err != nil {
		c.Log.Warn("could not read transcript", "error", err)
		return
	}
	if err := c.send(protocol.TypeHistory, protocol.History{SessionID: req.SessionID, Items: items}); err != nil {
		c.Log.Debug("could not send history", "error", err)
		return
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
