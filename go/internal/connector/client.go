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
		URL:     url,
		Token:   token,
		Name:    name,
		Bridge:  b,
		Log:     log,
		pending: map[string]*pendingAsk{},
		turns:   make(chan protocol.Submit, 32),
	}
}

// Run connects and stays connected until ctx ends, backing off between tries.
func (c *Client) Run(ctx context.Context) {
	go c.runTurns(ctx)

	backoff := time.Second
	for ctx.Err() == nil {
		err := c.session(ctx)
		if ctx.Err() != nil {
			return
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

// session runs one connection until it fails.
func (c *Client) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(dialCtx, c.URL, &websocket.DialOptions{
		HTTPHeader: map[string][]string{"Authorization": {"Bearer " + c.Token}},
	})
	if err != nil {
		return fmt.Errorf("dial: %w", err)
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
		return err
	}
	c.Log.Info("connector link established", "url", c.URL)

	beatCtx, stopBeat := context.WithCancel(ctx)
	defer stopBeat()
	go c.beat(beatCtx)
	if c.Discovery != nil {
		go c.inventoryLoop(beatCtx)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
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
		if c.Bridge != nil {
			if err := c.Bridge.RequestAbort(); err != nil {
				c.Log.Warn("could not write the abort request", "error", err)
			}
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
	var sub *bridge.Submission
	var err error
	if action == "command" {
		// A command carries a name and an argument tail; the plugin runs it
		// through $.command.run instead of submitting it as prompt text.
		name, args := splitCommand(msg.Text)
		sub, err = c.Bridge.SubmitCommand(msg.SessionID, msg.RequestID, name, args)
	} else {
		sub, err = c.Bridge.Submit(msg.SessionID, msg.ExternalSessionID, msg.RequestID, msg.Text, action)
	}
	if err != nil {
		c.Log.Warn("could not queue the message", "error", err)
		return
	}
	watcher := NewTurnWatcher(c.Bridge, c, msg.SessionID, sub)
	watcher.Run(ctx)
}

// TurnBegin implements TurnSink.
func (c *Client) TurnBegin(msg protocol.TurnBegin) { c.emit(protocol.TypeTurnBegin, msg) }

// TurnDelta implements TurnSink.
func (c *Client) TurnDelta(msg protocol.TurnDelta) { c.emit(protocol.TypeTurnDelta, msg) }

// TurnEnd implements TurnSink.
func (c *Client) TurnEnd(msg protocol.TurnEnd) { c.emit(protocol.TypeTurnEnd, msg) }

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
	kind := stringField(request, "kind")
	if kind == "" {
		kind = "permission"
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

	if !exists {
		raw, _ := json.Marshal(request["input"])
		if err := c.send(protocol.TypeApprovalRequest, protocol.ApprovalRequest{
			SessionID: stringField(request, "sessionId"),
			ToolUseID: toolUseID,
			Tool:      stringField(request, "tool"),
			Kind:      kind,
			Input:     raw,
		}); err != nil {
			return nil, err
		}
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
