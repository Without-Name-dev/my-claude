package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
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

// approvalRateLimit is the maximum number of approval requests per session
// within the rate window to prevent abuse.
const approvalRateLimit = 100

// approvalRateWindow is the time window for rate limiting approval requests.
const approvalRateWindow = time.Minute

// historyStableWindow keeps an in-progress transcript out of the timeline.
// This check runs on the connector because file modification times belong to
// the connector host's clock, which may differ from the server's clock.
const historyStableWindow = 10 * time.Second

const historyWorkerCount = 2

const historyQueueSize = 128

// Client is the connector's link to the server.
type Client struct {
	URL   string
	Token string
	// BootID stays the same for the life of this process, so a reconnect does
	// not look like a restart to the server.
	BootID string
	// ServerURL is the HTTP origin this connector was paired with. It is the
	// base for the attachment downloads a message can ask for; when empty it
	// is derived from URL.
	ServerURL  string
	Name       string
	Bridge     *bridge.Bridge
	Log        *slog.Logger
	DurableDir string
	// Discovery, when set, tells the server which sessions exist on this machine.
	Discovery *Discovery

	invMu             sync.Mutex
	seenSessions      map[string]string
	lastFullInventory time.Time

	connMu sync.Mutex
	conn   *websocket.Conn

	pendingMu sync.Mutex
	pending   map[string]*pendingAsk
	// pendingTools tracks which toolUseIds were created by this connector's Ask
	// calls, preventing external processes from injecting arbitrary decisions.
	pendingTools map[string]struct{}
	// approvalRateLimiter tracks approval request times per session for rate limiting.
	approvalRateLimiter map[string][]time.Time

	// turnSessions maps a runtime turn id to the server session it belongs to,
	// so an approval request that carries only the turn id can be addressed.
	turnMu       sync.RWMutex
	turnSessions map[string]string

	endMu sync.Mutex

	watchMu  sync.Mutex
	watching map[string]struct{}

	// activeMu guards accepted session work and cancels that arrive before a
	// message reaches the plugin queue.
	activeMu  sync.Mutex
	active    map[string]string
	queued    map[string]int
	cancelled map[string]int

	// turns preserves server arrival order while messages are written to the
	// plugin bridge. Watch jobs are routed separately so a streaming response
	// never blocks later submissions.
	turns       chan protocol.Submit
	turnWatches chan turnWatchJob

	// spawnMu guards spawnRequests: runtime session ids the server asked us to
	// open since the last submit for them. queueTurn uses it to prime the new
	// conversation's queue cursor before the message is written.
	spawnMu       sync.Mutex
	spawnRequests map[string]time.Time

	// historyMu guards historyStamps: versions the server confirmed committing.
	// They are mirrored to DurableDir so a restart does not re-upload every
	// transcript it had already sent (a burst of hundreds of items that made
	// the phone crawl right after every restart).
	historyMu          sync.Mutex
	historyStamps      map[string]string
	historyFullStamps  map[string]string
	historyRetryTimers map[string]*time.Timer
	historyRetryReqs   map[string]protocol.HistoryRequest
	historyQueue       chan string
	historyJobs        map[string]*historyJob

	// closedMu guards closed: server sessions whose terminal the person closed
	// from the phone. A turn watcher stops on its next poll instead of waiting
	// for the heartbeat to expire. closeGen versions each close request so a
	// message queued while the process scan runs can supersede it.
	closedMu sync.Mutex
	closed   map[string]time.Time
	closeGen map[string]int
}

type historyJob struct {
	req     protocol.HistoryRequest
	queued  bool
	running bool
	rerun   bool
}

type turnWatchJob struct {
	msg protocol.Submit
	sub *bridge.Submission
}

const turnWatchQueueSize = 256
const sessionTurnWatchQueueSize = 128
const pluginProtocolWait = 30 * time.Second

// pendingAsk is one question awaiting an answer from the phone.
type pendingAsk struct {
	answers chan protocol.ApprovalAnswer
	// settled keeps a late re-ask from blocking again after the person has
	// already answered.
	settled *protocol.ApprovalAnswer
}

// NewClient prepares a client. Run opens the connection.
func NewClient(url, token, name string, b *bridge.Bridge, log *slog.Logger) *Client {
	durableDir := ""
	if b != nil {
		durableDir = b.Dir
	}
	client := &Client{
		URL:                 url,
		Token:               token,
		BootID:              strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Name:                name,
		Bridge:              b,
		Log:                 log,
		DurableDir:          durableDir,
		pending:             map[string]*pendingAsk{},
		pendingTools:        map[string]struct{}{},
		approvalRateLimiter: map[string][]time.Time{},
		turnSessions:        map[string]string{},
		active:              map[string]string{},
		queued:              map[string]int{},
		cancelled:           map[string]int{},
		turns:               make(chan protocol.Submit, 32),
		turnWatches:         make(chan turnWatchJob, turnWatchQueueSize),
		spawnRequests:       map[string]time.Time{},
		closed:              map[string]time.Time{},
		closeGen:            map[string]int{},
		historyStamps:       map[string]string{},
		historyFullStamps:   map[string]string{},
		historyRetryTimers:  map[string]*time.Timer{},
		historyRetryReqs:    map[string]protocol.HistoryRequest{},
		historyQueue:        make(chan string, historyQueueSize),
		historyJobs:         map[string]*historyJob{},
		watching:            map[string]struct{}{},
	}
	client.loadHistoryStamps()
	return client
}

// SetDurableDir configures connector-owned state when plugin delivery is off.
func (c *Client) SetDurableDir(dir string) { c.DurableDir = dir }

// Run connects and stays connected until ctx ends, backing off between tries.
func (c *Client) Run(ctx context.Context) {
	go c.runTurns(ctx)
	go c.runTurnWatchers(ctx)
	go c.watchAgentActivity(ctx)
	go c.watchRuntimeState(ctx)
	go c.watchRebinds(ctx)
	for range historyWorkerCount {
		go c.runHistory(ctx)
	}
	c.recoverPendingWatches(ctx)

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

func (c *Client) recoverPendingWatches(ctx context.Context) {
	if c.Bridge == nil {
		return
	}
	records, err := c.Bridge.PendingAcceptedRequests()
	if err != nil {
		c.Log.Warn("could not read accepted requests for recovery", "error", err)
		return
	}
	for _, record := range records {
		if c.hasTurnEndSpool(record.SessionID, record.RequestID) {
			continue
		}
		msg := protocol.Submit{
			SessionID: record.SessionID, ExternalSessionID: record.ExternalSessionID,
			RequestID: record.RequestID, Action: record.Action,
		}
		if !c.markWatching(msg.SessionID, msg.RequestID) {
			continue
		}
		if record.Action == "command" {
			go c.watchCommand(ctx, msg)
			continue
		}
		job := turnWatchJob{msg: msg, sub: &bridge.Submission{
			QueuedAt: record.QueuedAt, PreviousTurnID: record.PreviousTurnID, Duplicate: true,
		}}
		select {
		case c.turnWatches <- job:
		case <-ctx.Done():
			c.unmarkWatching(msg.SessionID, msg.RequestID)
			return
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
		BootID:       c.BootID,
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
		protocol.CapabilitySubmitAck:        c.Bridge != nil,
		protocol.CapabilityTurnEndAck:       c.DurableDir != "",
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
			// The worker is busy. Leave the submit unacknowledged: the server
			// replays pending submits, and it lands once the queue drains.
			// Failing it here made a transient overload permanent — the same
			// clientMessageId could not be retried, and the phone showed a
			// failure for work that would have run seconds later.
			c.finishQueued(msg.SessionID)
			c.Log.Warn("turn queue full; leaving the submit pending for replay",
				"session", msg.SessionID, "request", msg.RequestID)
		}
	case protocol.TypeTurnEndAck:
		var ack protocol.TurnEndAck
		if err := json.Unmarshal(env.Data, &ack); err != nil {
			return
		}
		c.acknowledgeTurnEnd(ack)
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
		c.queueHistory(msg)
	case protocol.TypeHistoryAck:
		var msg protocol.HistoryAck
		if err := json.Unmarshal(env.Data, &msg); err != nil ||
			msg.SessionID == "" || msg.Version == "" || !msg.Complete {
			return
		}
		c.historyMu.Lock()
		if msg.Full {
			c.historyFullStamps[msg.SessionID] = msg.Version
		} else {
			c.historyStamps[msg.SessionID] = msg.Version
		}
		c.historyMu.Unlock()
		// Persist what the server confirmed, so a restart does not re-upload
		// every transcript it already sent.
		c.saveHistoryStamps()
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

// queueHistory keeps transcript scans off the websocket read loop while
// limiting concurrent work and coalescing overlapping requests per session.
func (c *Client) queueHistory(req protocol.HistoryRequest) {
	if req.SessionID == "" {
		return
	}
	if req.Limit <= 0 {
		req.Limit = maxHistoryItems
	}

	c.historyMu.Lock()
	if job := c.historyJobs[req.SessionID]; job != nil {
		if req.Limit > job.req.Limit {
			job.req.Limit = req.Limit
		}
		if req.Full {
			job.req.Full = true
		}
		if req.Force {
			job.req.Force = true
		}
		if job.running {
			job.rerun = true
		}
		c.historyMu.Unlock()
		return
	}
	job := &historyJob{req: req, queued: true}
	c.historyJobs[req.SessionID] = job
	select {
	case c.historyQueue <- req.SessionID:
		c.historyMu.Unlock()
	default:
		delete(c.historyJobs, req.SessionID)
		c.historyMu.Unlock()
		c.Log.Warn("history queue full; request deferred", "session", req.SessionID)
		c.scheduleHistoryRetry(req, time.Second)
	}
}

func (c *Client) runHistory(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case sessionID := <-c.historyQueue:
			c.historyMu.Lock()
			job := c.historyJobs[sessionID]
			if job == nil || !job.queued {
				c.historyMu.Unlock()
				continue
			}
			job.queued = false
			job.running = true
			req := job.req
			c.historyMu.Unlock()

			c.sendHistory(req)

			c.historyMu.Lock()
			job = c.historyJobs[sessionID]
			if job == nil {
				c.historyMu.Unlock()
				continue
			}
			job.running = false
			if !job.rerun {
				delete(c.historyJobs, sessionID)
				c.historyMu.Unlock()
				continue
			}
			job.rerun = false
			job.queued = true
			req = job.req
			select {
			case c.historyQueue <- sessionID:
				c.historyMu.Unlock()
			default:
				delete(c.historyJobs, sessionID)
				c.historyMu.Unlock()
				c.Log.Warn("history queue full; follow-up deferred", "session", sessionID)
				c.scheduleHistoryRetry(req, time.Second)
			}
		}
	}
}

// runTurns preserves FIFO order while writing every accepted prompt into the
// durable plugin queue. Reply polling happens on a separate worker.
func (c *Client) runTurns(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-c.turns:
			c.queueTurn(ctx, msg)
		}
	}
}

// queueTurn submits into Claude immediately and leaves a request-scoped
// watcher to stream the matching response without blocking later submissions.
func (c *Client) queueTurn(ctx context.Context, msg protocol.Submit) {
	handedOff := false
	defer func() {
		if !handedOff {
			c.finishQueued(msg.SessionID)
		}
	}()
	requestHash := submitRequestHash(msg)
	if c.Bridge == nil {
		message := "the connector has no plugin bridge configured, so the message cannot run on the desktop"
		if msg.Action == "command" {
			c.rejectCommand(msg, message)
		} else {
			c.rejectSubmit(msg, message)
		}
		return
	}
	if c.consumeAbort(msg.SessionID) {
		message := "the message was cancelled before it reached the desktop queue"
		if msg.Action == "command" {
			c.rejectCommand(msg, message)
		} else {
			c.rejectSubmit(msg, message)
		}
		return
	}
	c.consumeSpawnRequest(msg.ExternalSessionID)
	if err := c.Bridge.SeedSessionCursor(msg.ExternalSessionID); err != nil {
		c.Log.Warn("could not prime the session's queue cursor",
			"session", msg.SessionID, "external", msg.ExternalSessionID, "error", err)
	}
	action := msg.Action
	if action == "" {
		action = "prompt"
	}
	if action == "command" {
		name, args := splitCommand(msg.Text)
		sub, err := c.Bridge.SubmitCommandWithRequestHash(msg.SessionID, msg.ExternalSessionID, msg.RequestID, name, args, requestHash)
		if err != nil {
			c.Log.Warn("could not queue the command", "error", err)
			c.rejectCommand(msg, err.Error())
			return
		}
		c.ackCommand(msg, true, "")
		if sub.TerminalAcked || !c.markWatching(msg.SessionID, msg.RequestID) {
			return
		}
		go c.watchCommand(ctx, msg)
		return
	}
	text := msg.Text
	if len(msg.Attachments) > 0 {
		files, failures := c.materializeAttachments(ctx, msg)
		text = promptWithAttachments(text, files, failures)
	}
	sub, err := c.Bridge.SubmitWithRequestHash(msg.SessionID, msg.ExternalSessionID, msg.RequestID, text, action, requestHash)
	if err != nil {
		c.Log.Warn("could not queue the message", "error", err)
		if isPermanentSubmitError(err) {
			c.rejectSubmit(msg, err.Error())
		}
		return
	}
	c.Log.Info("submit written to plugin queue", "session", msg.SessionID, "external", msg.ExternalSessionID,
		"request", msg.RequestID, "queued_at", sub.QueuedAt, "duplicate", sub.Duplicate)
	c.ackSubmit(msg, true, "")
	c.setActive(msg.SessionID, msg.ExternalSessionID)
	c.clearSessionClosed(msg.SessionID)
	// A close that is still scanning for its process must not kill the
	// terminal this message is about to start, nor mark the new turn closed.
	c.supersedeClose(msg.SessionID)
	if c.consumeAbort(msg.SessionID) {
		if err := c.Bridge.RequestAbort(msg.ExternalSessionID); err != nil {
			c.Log.Warn("could not write the abort request", "error", err)
		}
	}
	if sub.TerminalAcked || c.hasTurnEndSpool(msg.SessionID, msg.RequestID) {
		return
	}
	if !c.markWatching(msg.SessionID, msg.RequestID) {
		return
	}
	select {
	case c.turnWatches <- turnWatchJob{msg: msg, sub: sub}:
		handedOff = true
	case <-ctx.Done():
		c.unmarkWatching(msg.SessionID, msg.RequestID)
	}
}

func submitRequestHash(msg protocol.Submit) string {
	body, err := json.Marshal(struct {
		SessionID         string                      `json:"sessionId"`
		ExternalSessionID string                      `json:"externalSessionId,omitempty"`
		RequestID         string                      `json:"requestId"`
		Text              string                      `json:"text"`
		Action            string                      `json:"action,omitempty"`
		Attachments       []protocol.SubmitAttachment `json:"attachments,omitempty"`
	}{msg.SessionID, msg.ExternalSessionID, msg.RequestID, msg.Text, msg.Action, msg.Attachments})
	if err != nil {
		return ""
	}
	hash := sha256.Sum256(body)
	return hex.EncodeToString(hash[:])
}

func isPermanentSubmitError(err error) bool {
	return errors.Is(err, bridge.ErrRequestConflict) || err.Error() == "empty message" || err.Error() == "empty command"
}

func (c *Client) ackSubmit(msg protocol.Submit, accepted bool, message string) {
	if msg.RequestID == "" {
		return
	}
	if err := c.send(protocol.TypeSubmitAck, protocol.SubmitAck{
		SessionID: msg.SessionID, ExternalSessionID: msg.ExternalSessionID,
		RequestID: msg.RequestID, Accepted: accepted, Error: message,
	}); err != nil {
		c.Log.Debug("could not forward submit acknowledgement", "request", msg.RequestID, "error", err)
	}
}

func (c *Client) ackCommand(msg protocol.Submit, accepted bool, message string) {
	if msg.RequestID == "" {
		return
	}
	if err := c.send(protocol.TypeCommandAck, protocol.CommandAck{
		SessionID: msg.SessionID, ExternalSessionID: msg.ExternalSessionID,
		RequestID: msg.RequestID, Accepted: accepted, Error: message,
	}); err != nil {
		c.Log.Debug("could not forward command acknowledgement", "request", msg.RequestID, "error", err)
	}
}

func (c *Client) rejectCommand(msg protocol.Submit, message string) {
	c.ackCommand(msg, false, message)
}

func (c *Client) watchCommand(ctx context.Context, msg protocol.Submit) {
	defer c.unmarkWatching(msg.SessionID, msg.RequestID)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	// Without a deadline this loop outlived the plugin: a missing receipt (the
	// process was killed, /clear changed the id, the write failed) leaked one
	// goroutine and one watching entry per failed command, forever, and the
	// same request could never be watched again.
	deadline := time.Now().Add(pickupTimeout)
	for {
		receipt, err := c.Bridge.ReadRequestReceipt(msg.ExternalSessionID, msg.RequestID)
		if err == nil && receipt != nil && (receipt.Status == "done" || receipt.Status == "failed") {
			if receipt.Status == "failed" {
				c.Log.Warn("the desktop rejected the command",
					"request", msg.RequestID, "error", receipt.Error)
			}
			if markErr := c.Bridge.MarkTurnEndAcked(msg.SessionID, msg.RequestID); markErr != nil {
				c.Log.Warn("could not record completed command", "request", msg.RequestID, "error", markErr)
			} else {
				if err := c.Bridge.RemoveRequestArtifacts(msg.ExternalSessionID, msg.RequestID); err != nil {
					c.Log.Warn("could not remove completed command artifacts", "request", msg.RequestID, "error", err)
				}
				return
			}
		}
		if time.Now().After(deadline) {
			c.Log.Warn("command receipt never arrived",
				"session", msg.SessionID, "request", msg.RequestID)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *Client) rejectSubmit(msg protocol.Submit, message string) {
	c.ackSubmit(msg, false, message)
	c.sendTurnEnd(protocol.TurnEnd{
		SessionID: msg.SessionID, TurnID: msg.RequestID, RequestID: msg.RequestID,
		Status: "error", Error: message,
	}, msg.ExternalSessionID)
}

func watchKey(sessionID, requestID string) string {
	return sessionID + "\x00" + requestID
}

func (c *Client) markWatching(sessionID, requestID string) bool {
	key := watchKey(sessionID, requestID)
	c.watchMu.Lock()
	defer c.watchMu.Unlock()
	if _, exists := c.watching[key]; exists {
		return false
	}
	c.watching[key] = struct{}{}
	return true
}

func (c *Client) unmarkWatching(sessionID, requestID string) {
	c.watchMu.Lock()
	delete(c.watching, watchKey(sessionID, requestID))
	c.watchMu.Unlock()
}

// runTurnWatchers keeps reply tracking independent across sessions. Each
// session has one FIFO watcher so its turn lifecycle stays ordered.
func (c *Client) runTurnWatchers(ctx context.Context) {
	queues := make(map[string]chan turnWatchJob)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-c.turnWatches:
			queue := queues[job.msg.SessionID]
			if queue == nil {
				queue = make(chan turnWatchJob, sessionTurnWatchQueueSize)
				queues[job.msg.SessionID] = queue
				go c.runSessionTurnWatchers(ctx, job.msg.SessionID, queue)
			}
			select {
			case queue <- job:
			case <-ctx.Done():
				return
			}
		}
	}
}

func (c *Client) runSessionTurnWatchers(ctx context.Context, sessionID string, jobs <-chan turnWatchJob) {
	lastTurnIDs := make(map[string]string)
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-jobs:
			requestScoped := c.waitForRequestOutbox(ctx, job.msg.ExternalSessionID)
			watcher := NewTurnWatcher(c.Bridge, c, job.msg.SessionID, job.msg.ExternalSessionID,
				job.msg.RequestID, job.sub)
			watcher.log = c.Log
			watcher.Closed = func() bool { return c.sessionWasClosed(job.msg.SessionID) }
			c.Log.Info("watching submitted message", "session", job.msg.SessionID,
				"external", job.msg.ExternalSessionID, "request", job.msg.RequestID)
			if requestScoped {
				watcher.requestScoped = true
				c.Log.Info("using request-scoped plugin outbox", "session", job.msg.SessionID,
					"external", job.msg.ExternalSessionID, "request", job.msg.RequestID)
				go func(watcher *TurnWatcher, serverSessionID, externalSessionID, requestID string) {
					watcher.Run(ctx)
					c.unmarkWatching(serverSessionID, requestID)
					c.clearActive(sessionID)
					c.finishQueued(sessionID)
				}(watcher, job.msg.SessionID, job.msg.ExternalSessionID, job.msg.RequestID)
				continue
			}
			if previous := lastTurnIDs[job.msg.SessionID]; previous != "" {
				watcher.previousID = previous
			}
			watcher.Run(ctx)
			if !requestScoped && watcher.turnID != "" {
				lastTurnIDs[job.msg.SessionID] = watcher.turnID
			}
			c.unmarkWatching(job.msg.SessionID, job.msg.RequestID)
			c.clearActive(sessionID)
			c.finishQueued(sessionID)
		}
	}
}

func (c *Client) waitForRequestOutbox(ctx context.Context, sessionID string) bool {
	if c.Bridge == nil || sessionID == "" {
		return false
	}
	deadline := time.NewTimer(pluginProtocolWait)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if c.Bridge.RequestOutboxSupported(sessionID) {
			return true
		}
		if sessionIsLive(c.Bridge, sessionID) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-ticker.C:
		}
	}
}

// TurnBegin implements TurnSink.
func (c *Client) TurnBegin(msg protocol.TurnBegin) {
	if msg.TurnID != "" && msg.SessionID != "" {
		c.turnMu.Lock()
		c.turnSessions[msg.TurnID] = msg.SessionID
		c.turnMu.Unlock()
	}
	c.Log.Info("runtime turn began", "session", msg.SessionID, "request", msg.RequestID, "turn", msg.TurnID)
	c.emit(protocol.TypeTurnBegin, msg)
}

// TurnDelta implements TurnSink.
func (c *Client) TurnDelta(msg protocol.TurnDelta) { c.emit(protocol.TypeTurnDelta, msg) }

type pendingTurnEnd struct {
	Message           protocol.TurnEnd `json:"message"`
	ExternalSessionID string           `json:"externalSessionId,omitempty"`
	CreatedAt         int64            `json:"createdAt"`
}

func (c *Client) turnEndDir() string {
	if c.DurableDir == "" {
		return ""
	}
	return filepath.Join(c.DurableDir, "turn-ends")
}

func turnEndKey(sessionID, requestID, turnID string) string {
	key := sessionID + "\x00" + requestID
	if requestID == "" {
		key += "\x00" + turnID
	}
	hash := sha256.Sum256([]byte(key))
	return hex.EncodeToString(hash[:]) + ".json"
}

func (c *Client) turnEndPath(sessionID, requestID, turnID string) string {
	dir := c.turnEndDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, turnEndKey(sessionID, requestID, turnID))
}

func (c *Client) hasTurnEndSpool(sessionID, requestID string) bool {
	if requestID == "" {
		return false
	}
	path := c.turnEndPath(sessionID, requestID, "")
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func (c *Client) persistTurnEnd(msg protocol.TurnEnd, externalSessionID string) error {
	path := c.turnEndPath(msg.SessionID, msg.RequestID, msg.TurnID)
	if path == "" {
		return nil
	}
	c.endMu.Lock()
	defer c.endMu.Unlock()
	if raw, err := os.ReadFile(path); err == nil {
		var existing pendingTurnEnd
		if err := json.Unmarshal(raw, &existing); err != nil {
			return fmt.Errorf("decode queued turn.end: %w", err)
		}
		if existing.Message.SessionID != msg.SessionID || existing.Message.RequestID != msg.RequestID || existing.Message.TurnID != msg.TurnID {
			return errors.New("queued turn.end identity conflict")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	entry := pendingTurnEnd{
		Message: msg, ExternalSessionID: externalSessionID,
		CreatedAt: time.Now().UnixMilli(),
	}
	body, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return writeDurableAtomic(path, body)
}

func writeDurableAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(body); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// TurnEnd implements TurnSink.
//
// The spool remains until the server confirms its database commit.
func (c *Client) TurnEnd(msg protocol.TurnEnd) {
	c.Log.Info("runtime turn ended", "session", msg.SessionID, "request", msg.RequestID,
		"turn", msg.TurnID, "status", msg.Status, "text_bytes", len(msg.Text), "has_error", msg.Error != "")
	externalSessionID := ""
	if c.Bridge != nil && msg.RequestID != "" {
		if accepted, err := c.Bridge.AcceptedRequest(msg.SessionID, msg.RequestID); err == nil && accepted != nil {
			externalSessionID = accepted.ExternalSessionID
		}
	}
	c.sendTurnEnd(msg, externalSessionID)
	c.forgetTurn(msg.TurnID)
}

func (c *Client) sendTurnEnd(msg protocol.TurnEnd, externalSessionID string) {
	if err := c.persistTurnEnd(msg, externalSessionID); err != nil {
		c.Log.Warn("could not persist turn.end before forwarding", "request", msg.RequestID, "error", err)
	}
	if err := c.send(protocol.TypeTurnEnd, msg); err != nil {
		c.Log.Debug("could not forward frame", "type", protocol.TypeTurnEnd, "error", err)
	}
}

func (c *Client) acknowledgeTurnEnd(ack protocol.TurnEndAck) {
	if ack.SessionID == "" || ack.RequestID == "" || ack.TurnID == "" {
		return
	}
	path := c.turnEndPath(ack.SessionID, ack.RequestID, ack.TurnID)
	if path == "" {
		return
	}
	c.endMu.Lock()
	defer c.endMu.Unlock()
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			c.Log.Debug("could not read acknowledged turn.end", "request", ack.RequestID, "error", err)
		}
		return
	}
	var entry pendingTurnEnd
	if err := json.Unmarshal(raw, &entry); err != nil || entry.Message.SessionID != ack.SessionID || entry.Message.RequestID != ack.RequestID || entry.Message.TurnID != ack.TurnID {
		c.Log.Warn("ignoring mismatched turn.end acknowledgement", "request", ack.RequestID)
		return
	}
	externalSessionID := entry.ExternalSessionID
	if externalSessionID == "" && c.Bridge != nil {
		if accepted, err := c.Bridge.AcceptedRequest(ack.SessionID, ack.RequestID); err == nil && accepted != nil {
			externalSessionID = accepted.ExternalSessionID
		}
	}
	if c.Bridge != nil {
		if err := c.Bridge.MarkTurnEndAcked(ack.SessionID, ack.RequestID); err != nil {
			c.Log.Warn("could not record committed turn.end acknowledgement", "request", ack.RequestID, "error", err)
			return
		}
		if err := c.Bridge.RemoveRequestArtifacts(externalSessionID, ack.RequestID); err != nil {
			c.Log.Warn("could not remove committed request artifacts", "request", ack.RequestID, "error", err)
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		c.Log.Warn("could not remove committed turn.end spool", "request", ack.RequestID, "error", err)
	}
}

// ToolBegin implements TurnSink.
func (c *Client) ToolBegin(msg protocol.ToolBegin) { c.emit(protocol.TypeToolBegin, msg) }

// ToolEnd implements TurnSink.
func (c *Client) ToolEnd(msg protocol.ToolEnd) { c.emit(protocol.TypeToolEnd, msg) }

// AgentActivity implements TurnSink.
func (c *Client) AgentActivity(msg protocol.AgentActivity) {
	c.emit(protocol.TypeAgentActivity, msg)
}

// agentPollInterval is how often a live conversation's subagent snapshot is
// read when no turn watcher is running. A turn watcher only exists for a
// message this connector queued, so without this poll a background subagent
// (and the moment it finishes) reached the phone only when the person sent
// something — the status line kept the count it had when the last turn ended.
const agentPollInterval = 700 * time.Millisecond

// watchAgentActivity forwards subagent snapshots outside turn watchers.
func (c *Client) watchAgentActivity(ctx context.Context) {
	if c.Bridge == nil {
		return
	}
	ticker := time.NewTicker(agentPollInterval)
	defer ticker.Stop()
	// Key per runtime session; empty means "reported, nothing running".
	snapshots := map[string]string{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		live := map[string]bool{}
		for _, id := range c.Bridge.LiveSessions(bridge.DefaultLiveTTL) {
			live[id] = true
		}
		ids := make([]string, 0, len(live)+len(snapshots))
		for id := range live {
			ids = append(ids, id)
		}
		for id := range snapshots {
			if !live[id] {
				ids = append(ids, id)
			}
		}
		for _, externalID := range ids {
			// A conversation whose plugin stopped stamping itself live has no
			// running subagents; its file may still say otherwise.
			var entries []bridge.AgentEntry
			if live[externalID] {
				entries = c.Bridge.Agents(externalID)
			}
			key := agentSnapshotKey(entries)
			previous, seen := snapshots[externalID]
			if seen && key == previous {
				if key == "" && !live[externalID] {
					delete(snapshots, externalID)
				}
				continue
			}
			// The first observation is sent even when empty: the server may
			// still hold a snapshot from before this connector started, and
			// nothing else would ever retract it.
			snapshots[externalID] = key
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
			c.emit(protocol.TypeAgentActivity, protocol.AgentActivity{
				ExternalSessionID: externalID,
				Agents:            agents,
			})
		}
	}
}

// runtimePollInterval is how often a live conversation's outbox is checked for
// a turn that no message of the phone's started.
const runtimePollInterval = 900 * time.Millisecond

// watchRuntimeState reports turns begun in the terminal.
//
// A turn that was not queued here has no turn watcher, so the server never
// learned it was running: the phone showed idle, with no progress line and no
// stop button, while the machine was working. The plugin writes the turn's
// status to its outbox either way.
func (c *Client) watchRuntimeState(ctx context.Context) {
	if c.Bridge == nil {
		return
	}
	ticker := time.NewTicker(runtimePollInterval)
	defer ticker.Stop()
	// Only sessions this connector has reported as running are tracked: an
	// initial "idle" must not overwrite the server's waiting/stopping state.
	running := map[string]bool{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		live := map[string]bool{}
		for _, id := range c.Bridge.LiveSessions(bridge.DefaultLiveTTL) {
			live[id] = true
		}
		ids := make([]string, 0, len(live)+len(running))
		for id := range live {
			ids = append(ids, id)
		}
		for id := range running {
			if !live[id] {
				ids = append(ids, id)
			}
		}
		for _, externalID := range ids {
			streaming := false
			if live[externalID] {
				if box, err := c.Bridge.ReadOutbox(externalID); err == nil && box != nil {
					streaming = box.Status == "streaming"
				}
			}
			if streaming {
				if running[externalID] {
					continue
				}
				running[externalID] = true
				c.emit(protocol.TypeRuntimeState, protocol.RuntimeState{
					ExternalSessionID: externalID,
					Status:            "running",
				})
				continue
			}
			if !running[externalID] {
				continue
			}
			delete(running, externalID)
			c.emit(protocol.TypeRuntimeState, protocol.RuntimeState{
				ExternalSessionID: externalID,
				Status:            "idle",
			})
		}
	}
}

// rebindPollInterval is how often the plugin's /clear records are checked.
const rebindPollInterval = 1500 * time.Millisecond

// watchRebinds forwards the plugin's /clear records.
//
// After /clear the terminal keeps running under a new transcript id. Nothing
// on the server knows that transition, so the conversation the person is
// looking at refused every message addressed to the old id. The plugin writes
// a rebind record; this forwards it and removes it once the server has it.
func (c *Client) watchRebinds(ctx context.Context) {
	if c.Bridge == nil {
		return
	}
	ticker := time.NewTicker(rebindPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, rebind := range c.Bridge.RebindRequests() {
			c.Log.Info("conversation changed its transcript id",
				"from", rebind.From, "to", rebind.To)
			if err := c.send(protocol.TypeSessionRebind, protocol.SessionRebind{
				FromExternalID: rebind.From,
				ToExternalID:   rebind.To,
			}); err != nil {
				// The socket is down; leave the record for the next attempt.
				c.Log.Debug("could not forward a session rebind", "error", err)
				continue
			}
			if err := c.Bridge.RemoveRebind(rebind.Path); err != nil {
				c.Log.Warn("could not remove a processed rebind record", "error", err)
			}
		}
	}
}

// agentSnapshotKey is the change key that decides whether a snapshot is worth
// sending; the live fields are part of it on purpose.
func agentSnapshotKey(entries []bridge.AgentEntry) string {
	var builder strings.Builder
	for _, entry := range entries {
		builder.WriteString(entry.AgentID)
		builder.WriteByte(0)
		builder.WriteString(entry.Status)
		builder.WriteByte(0)
		builder.WriteString(entry.Label)
		builder.WriteByte(0)
		builder.WriteString(entry.Tool)
		builder.WriteByte(0)
		builder.WriteString(entry.Detail)
		builder.WriteByte(0)
		builder.WriteString(entry.Text)
		builder.WriteByte(0x1e)
	}
	return builder.String()
}

func (c *Client) emit(frameType string, value any) {
	if err := c.send(frameType, value); err != nil {
		c.Log.Debug("could not forward frame", "type", frameType, "error", err)
	}
}

// replayTurnEnds re-sends unacknowledged durable turn.end frames oldest first.
func (c *Client) replayTurnEnds() {
	dir := c.turnEndDir()
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			c.Log.Warn("could not read turn.end spool", "error", err)
		}
		return
	}
	queued := make([]pendingTurnEnd, 0, len(entries))
	for _, file := range entries {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		var entry pendingTurnEnd
		raw, readErr := os.ReadFile(filepath.Join(dir, file.Name()))
		if readErr != nil || json.Unmarshal(raw, &entry) != nil {
			c.Log.Warn("ignoring malformed turn.end spool entry", "file", file.Name())
			continue
		}
		queued = append(queued, entry)
	}
	sort.Slice(queued, func(i, j int) bool { return queued[i].CreatedAt < queued[j].CreatedAt })
	for _, entry := range queued {
		if err := c.send(protocol.TypeTurnEnd, entry.Message); err != nil {
			c.Log.Debug("could not replay turn.end", "request", entry.Message.RequestID, "error", err)
			return
		}
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
		delete(c.active, sessionID)
	} else {
		c.queued[sessionID]--
	}
	c.activeMu.Unlock()
}

// setActive records which submitted session the connector is following and
// the runtime id the plugin knows it by.
func (c *Client) setActive(sessionID, externalSessionID string) {
	c.activeMu.Lock()
	c.active[sessionID] = externalSessionID
	c.activeMu.Unlock()
}

// clearActive forgets the session unless another submit has already claimed
// it.
func (c *Client) clearActive(sessionID string) {
	c.activeMu.Lock()
	if c.queued[sessionID] <= 1 {
		delete(c.active, sessionID)
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
	if activeExternal, active := c.active[sessionID]; active {
		if externalSessionID != "" {
			return externalSessionID, false, true
		}
		return activeExternal, false, true
	}
	if _, queued := c.queued[sessionID]; queued {
		c.cancelled[sessionID]++
		return externalSessionID, true, true
	}
	// A turn started in the terminal was never queued here, so neither map
	// knows it. The server names the runtime session, and that is enough: the
	// plugin ignores an abort for a conversation it is not running. Without
	// this, stopping a terminal turn from the phone did nothing at all.
	if externalSessionID != "" {
		return externalSessionID, false, true
	}
	return "", false, false
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

// checkApprovalRateLimit enforces a per-session rate limit on approval requests
// to prevent abuse. Returns true if the request is allowed, false if rate limited.
func (c *Client) checkApprovalRateLimit(sessionID string) bool {
	if sessionID == "" {
		return true
	}

	now := time.Now()
	cutoff := now.Add(-approvalRateWindow)

	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()

	// Clean up old entries
	timestamps := c.approvalRateLimiter[sessionID]
	validTimestamps := make([]time.Time, 0, len(timestamps))
	for _, ts := range timestamps {
		if ts.After(cutoff) {
			validTimestamps = append(validTimestamps, ts)
		}
	}

	// Check if rate limit exceeded
	if len(validTimestamps) >= approvalRateLimit {
		return false
	}

	// Add current request
	validTimestamps = append(validTimestamps, now)
	c.approvalRateLimiter[sessionID] = validTimestamps
	return true
}

// cleanupApproval removes tracking data for a settled approval to prevent memory leaks.
func (c *Client) cleanupApproval(toolUseID string) {
	c.pendingMu.Lock()
	delete(c.pending, toolUseID)
	delete(c.pendingTools, toolUseID)
	c.pendingMu.Unlock()
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

	// The plugin withdraws a question it can no longer wait for — the person
	// answered it in the terminal, or the hook was abandoned. Retract the card
	// instead of leaving it on the phone until the turn ends.
	if stringField(request, "action") == "cancel" {
		c.cancelApproval(toolUseID, stringField(request, "sessionId"), stringField(request, "externalSessionId"))
		return map[string]any{"decision": "cancelled"}, nil
	}

	// The pane answers by posting the decision to this same endpoint. Settle
	// the hook that is waiting on this tool instead of treating the decision
	// as another question.
	if decision := stringField(request, "decision"); decision == "allow" || decision == "deny" {
		// SECURITY: Only allow settling decisions for toolUseIds created by this connector.
		c.pendingMu.Lock()
		_, isOurs := c.pendingTools[toolUseID]
		c.pendingMu.Unlock()
		if !isOurs {
			c.Log.Warn("rejecting settlement attempt for unknown toolUseId", "toolUseId", toolUseID)
			return nil, errors.New("toolUseId not recognized")
		}
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

	// SECURITY: Rate limit approval requests per session to prevent abuse.
	if sessionID != "" && !c.checkApprovalRateLimit(sessionID) {
		c.Log.Warn("approval request rate limit exceeded", "session", sessionID, "toolUseId", toolUseID)
		return nil, errors.New("rate limit exceeded")
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
		// SECURITY: Track this toolUseId as owned by this connector.
		c.pendingTools[toolUseID] = struct{}{}
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

// cancelApproval forgets a pending question and tells the server to retract it.
func (c *Client) cancelApproval(toolUseID, sessionID, externalSessionID string) {
	c.pendingMu.Lock()
	_, ok := c.pending[toolUseID]
	delete(c.pending, toolUseID)
	delete(c.pendingTools, toolUseID)
	c.pendingMu.Unlock()
	if !ok {
		return
	}
	c.emit(protocol.TypeApprovalCancel, protocol.ApprovalCancel{
		SessionID:         sessionID,
		ExternalSessionID: externalSessionID,
		ToolUseID:         toolUseID,
	})
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
	// SECURITY: Verify this toolUseId belongs to this connector before delivering.
	c.pendingMu.Lock()
	ask, ok := c.pending[msg.ToolUseID]
	_, isOurs := c.pendingTools[msg.ToolUseID]
	var settled *protocol.ApprovalAnswer
	if ok && ask.settled != nil {
		settled = ask.settled
	}
	c.pendingMu.Unlock()

	if !isOurs {
		c.Log.Warn("ignoring approval for unknown toolUseId", "toolUseId", msg.ToolUseID)
		return
	}
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
	var updated map[string]any
	if len(answer.UpdatedInput) > 0 {
		if err := json.Unmarshal(answer.UpdatedInput, &updated); err == nil && updated != nil {
			out["updatedInput"] = updated
		}
	}
	if len(answer.Answers) > 0 {
		if updated == nil {
			updated = map[string]any{}
		}
		if _, exists := updated["answers"]; !exists {
			updated["answers"] = answer.Answers
		}
		out["updatedInput"] = updated
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
		c.seenSessions = map[string]string{}
	}
	full := time.Since(c.lastFullInventory) >= fullInventoryEvery
	changed := make([]protocol.InventorySession, 0, 16)
	seen := make(map[string]string, len(records))
	for _, record := range records {
		version := record.Version
		if version == "" {
			version = strconv.FormatInt(record.UpdatedAt, 10)
		}
		seen[record.ExternalID] = version
		if previous, ok := c.seenSessions[record.ExternalID]; ok && previous == version && !full {
			continue
		}
		changed = append(changed, protocol.InventorySession{
			ExternalID: record.ExternalID,
			CWD:        record.CWD,
			Title:      record.Title,
			Model:      record.Model,
			UpdatedAt:  record.UpdatedAt,
			Version:    version,
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
	version := ""
	if info, err := os.Stat(path); err == nil {
		version = strconv.FormatInt(info.Size(), 10) + ":" + strconv.FormatInt(info.ModTime().UnixNano(), 10)
		if delay := historyRetryDelay(info.ModTime(), time.Now()); delay > 0 {
			c.scheduleHistoryRetry(req, delay)
			return
		}
		c.historyMu.Lock()
		previousTail := c.historyStamps[req.SessionID]
		previousFull := c.historyFullStamps[req.SessionID]
		c.historyMu.Unlock()
		if (!req.Force && req.Full && previousFull == version) ||
			(!req.Full && (previousTail == version || previousFull == version)) {
			return
		}
	}
	if req.Full {
		sentItems := 0
		err := ReadHistoryPages(path, historyPageItems, func(items []protocol.HistoryItem, complete bool) error {
			frame := protocol.History{
				SessionID: req.SessionID,
				Version:   version,
				Full:      true,
				Complete:  &complete,
				Items:     items,
			}
			if err := c.send(protocol.TypeHistory, frame); err != nil {
				return err
			}
			sentItems += len(items)
			return nil
		})
		if err != nil {
			c.Log.Debug("could not send full history", "session", req.SessionID, "error", err)
			return
		}
		c.Log.Info("sent full history", "session", req.SessionID, "items", sentItems)
		return
	}
	items, err := ReadHistory(path, req.Limit)
	if err != nil {
		c.Log.Warn("could not read transcript", "error", err)
		return
	}
	frame := protocol.History{SessionID: req.SessionID, Version: version, Items: items}
	if err := c.send(protocol.TypeHistory, frame); err != nil {
		c.Log.Debug("could not send history", "error", err)
		return
	}
	c.Log.Info("sent history", "session", req.SessionID, "items", len(items))
}

// historyStampsLimit bounds the persisted map so a long-lived connector cannot
// grow the file without limit.
const historyStampsLimit = 1000

type historyStampsFile struct {
	Tail map[string]string `json:"tail"`
	Full map[string]string `json:"full"`
}

func (c *Client) historyStampsPath() string {
	if c.DurableDir == "" {
		return ""
	}
	return filepath.Join(c.DurableDir, "history-stamps.json")
}

// loadHistoryStamps restores the versions this connector already sent and the
// server acknowledged. Without it, every restart made the server request every
// transcript again and the connector uploaded hundreds of items per session.
func (c *Client) loadHistoryStamps() {
	path := c.historyStampsPath()
	if path == "" {
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var body historyStampsFile
	if json.Unmarshal(raw, &body) != nil {
		return
	}
	c.historyMu.Lock()
	defer c.historyMu.Unlock()
	for id, version := range body.Tail {
		if id != "" && version != "" {
			c.historyStamps[id] = version
		}
	}
	for id, version := range body.Full {
		if id != "" && version != "" {
			c.historyFullStamps[id] = version
		}
	}
}

func (c *Client) saveHistoryStamps() {
	path := c.historyStampsPath()
	if path == "" {
		return
	}
	c.historyMu.Lock()
	body := historyStampsFile{Tail: map[string]string{}, Full: map[string]string{}}
	for id, version := range c.historyStamps {
		if len(body.Tail) >= historyStampsLimit {
			break
		}
		body.Tail[id] = version
	}
	for id, version := range c.historyFullStamps {
		if len(body.Full) >= historyStampsLimit {
			break
		}
		body.Full[id] = version
	}
	c.historyMu.Unlock()
	raw, err := json.Marshal(body)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func historyRetryDelay(modifiedAt, now time.Time) time.Duration {
	delay := modifiedAt.Add(historyStableWindow).Sub(now)
	if delay < 0 {
		return 0
	}
	return delay
}

func (c *Client) scheduleHistoryRetry(req protocol.HistoryRequest, delay time.Duration) {
	c.historyMu.Lock()
	if c.historyRetryTimers[req.SessionID] != nil {
		pending := c.historyRetryReqs[req.SessionID]
		if req.Limit > pending.Limit {
			pending.Limit = req.Limit
		}
		if req.Full {
			pending.Full = true
		}
		if req.Force {
			pending.Force = true
		}
		c.historyRetryReqs[req.SessionID] = pending
		c.historyMu.Unlock()
		return
	}
	c.historyRetryReqs[req.SessionID] = req
	c.historyRetryTimers[req.SessionID] = time.AfterFunc(delay, func() {
		c.historyMu.Lock()
		delete(c.historyRetryTimers, req.SessionID)
		retry := c.historyRetryReqs[req.SessionID]
		delete(c.historyRetryReqs, req.SessionID)
		c.historyMu.Unlock()
		c.queueHistory(retry)
	})
	c.historyMu.Unlock()
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
