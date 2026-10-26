package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// connectorBeatTimeout is how long a connector socket may stay silent. The
// connector beats every 20 seconds, so this is three missed beats; a half-open
// socket that never delivers another frame is closed and taken offline.
const connectorBeatTimeout = 60 * time.Second

// connectorConn is one live connector socket.
type connectorConn struct {
	id           string
	userID       string
	connectionID string
	ws           *websocket.Conn

	outbound chan []byte
	// done is closed once when the connection is replaced or revoked. The
	// outbound channel stays open: SendRaw may still hold a pointer to a
	// connection another goroutine just removed, and closing it would panic.
	done      chan struct{}
	closeOnce sync.Once

	// bridgeMu guards the capabilities this socket reported in its hello.
	//
	// bridgeOK reports whether the phone-pane plugin is loaded in a live
	// session. bridgeConfigured is true when the connector has a bridge
	// directory at all, even if no plugin answers right now, so its queued
	// message would be picked up by a terminal this server asks it to open.
	// sessionOpenOK is true only for connectors that can actually start a
	// terminal; an older build omits the capability and must never be sent a
	// session.open frame it would silently ignore.
	bridgeMu         sync.RWMutex
	bridgeOK         bool
	bridgeConfigured bool
	sessionOpenOK    bool
}

func (c *connectorConn) close() {
	c.closeOnce.Do(func() {
		if c.done != nil {
			close(c.done)
		}
	})
}

func (c *connectorConn) closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func (c *connectorConn) setBridge(ok bool) {
	c.bridgeMu.Lock()
	c.bridgeOK = ok
	c.bridgeMu.Unlock()
}

func (c *connectorConn) bridgeReady() bool {
	c.bridgeMu.RLock()
	defer c.bridgeMu.RUnlock()
	return c.bridgeOK
}

// setCapabilities applies a hello. A beat only refreshes the live bridge flag;
// the rest describe the build and stay as hello reported them.
func (c *connectorConn) setCapabilities(capabilities map[string]bool) {
	c.bridgeMu.Lock()
	c.bridgeOK = capabilities[protocol.CapabilityBridge]
	c.bridgeConfigured = capabilities[protocol.CapabilityBridgeConfigured]
	c.sessionOpenOK = capabilities[protocol.CapabilitySessionOpen]
	c.bridgeMu.Unlock()
}

func (c *connectorConn) bridgeQueueReady() bool {
	c.bridgeMu.RLock()
	defer c.bridgeMu.RUnlock()
	return c.bridgeConfigured
}

func (c *connectorConn) sessionOpenReady() bool {
	c.bridgeMu.RLock()
	defer c.bridgeMu.RUnlock()
	return c.sessionOpenOK
}

// ConnectorHub tracks the live connector sockets and delivers frames to them.
//
// One connector may only have one socket: a reconnect replaces the old one,
// which is what stops a half-dead socket from receiving orders it will never
// act on.
type ConnectorHub struct {
	mu   sync.RWMutex
	byID map[string]*connectorConn
	// live holds the runtime session ids each connector last reported as able
	// to accept a prompt. It belongs to the socket, not the database: a
	// reconnect restates it on hello, and going offline clears it.
	live map[string]map[string]struct{}
	log  *slog.Logger
}

// NewConnectorHub returns an empty hub.
func NewConnectorHub(log *slog.Logger) *ConnectorHub {
	return &ConnectorHub{
		byID: map[string]*connectorConn{},
		live: map[string]map[string]struct{}{},
		log:  log,
	}
}

// Live reports whether a message sent to this session now would actually be
// picked up: the connector has to be the current socket, its bridge heartbeat
// has to be fresh, and the session has to be in the last presence report. A
// session without a runtime id can never be live.
func (h *ConnectorHub) Live(connectorID, externalSessionID string) bool {
	if connectorID == "" || externalSessionID == "" {
		return false
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	conn := h.byID[connectorID]
	if conn == nil || !conn.bridgeReady() {
		return false
	}
	_, ok := h.live[connectorID][externalSessionID]
	return ok
}

// LiveByConnector snapshots every connector's live external session ids.
func (h *ConnectorHub) LiveByConnector() map[string]map[string]struct{} {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make(map[string]map[string]struct{}, len(h.live))
	for id, set := range h.live {
		if len(set) == 0 {
			continue
		}
		copySet := make(map[string]struct{}, len(set))
		for sessionID := range set {
			copySet[sessionID] = struct{}{}
		}
		out[id] = copySet
	}
	return out
}

// SetLiveSessions replaces one connection's live set and returns the external
// session ids whose membership changed, so the caller can publish just those.
// It ignores a frame from a socket another connection has replaced.
func (h *ConnectorHub) SetLiveSessions(conn *connectorConn, externalSessionIDs []string) []string {
	next := make(map[string]struct{}, len(externalSessionIDs))
	for _, id := range externalSessionIDs {
		if id != "" {
			next[id] = struct{}{}
		}
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	if conn == nil || h.byID[conn.id] != conn {
		return nil
	}
	previous := h.live[conn.id]
	if sameIDSet(previous, next) {
		return nil
	}
	changed := make([]string, 0)
	for id := range next {
		if _, ok := previous[id]; !ok {
			changed = append(changed, id)
		}
	}
	for id := range previous {
		if _, ok := next[id]; !ok {
			changed = append(changed, id)
		}
	}
	if len(next) == 0 {
		delete(h.live, conn.id)
	} else {
		h.live[conn.id] = next
	}
	sort.Strings(changed)
	return changed
}

// sameIDSet reports whether two presence sets hold the same ids.
func sameIDSet(left, right map[string]struct{}) bool {
	if len(left) != len(right) {
		return false
	}
	for id := range left {
		if _, ok := right[id]; !ok {
			return false
		}
	}
	return true
}

// CanDeliver reports whether a message sent now would actually be picked up.
func (h *ConnectorHub) CanDeliver(connectorID string) bool {
	h.mu.RLock()
	conn := h.byID[connectorID]
	h.mu.RUnlock()
	return conn != nil && conn.bridgeReady()
}

// SupportsSessionOpen reports whether this connector's build can start a
// terminal. It is false for a connector that predates session.open, so the
// server must not report a terminal as opened when the frame would be ignored.
func (h *ConnectorHub) SupportsSessionOpen(connectorID string) bool {
	h.mu.RLock()
	conn := h.byID[connectorID]
	h.mu.RUnlock()
	return conn != nil && conn.sessionOpenReady()
}

// CanQueue reports whether the connector has a bridge directory a submit could
// be written into. Opening a terminal for a message is only useful when this is
// true: the spawned Claude session would otherwise have nothing to read.
func (h *ConnectorHub) CanQueue(connectorID string) bool {
	h.mu.RLock()
	conn := h.byID[connectorID]
	h.mu.RUnlock()
	return conn != nil && conn.bridgeQueueReady()
}

// Online reports whether a connector currently has a socket.
func (h *ConnectorHub) Online(connectorID string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	_, ok := h.byID[connectorID]
	return ok
}

// OnlineIDs returns every connector with a live socket.
func (h *ConnectorHub) OnlineIDs() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]string, 0, len(h.byID))
	for id := range h.byID {
		out = append(out, id)
	}
	return out
}

// Send delivers one frame. It reports false when the connector is offline.
func (h *ConnectorHub) Send(connectorID, frameType string, value any) bool {
	data, err := json.Marshal(value)
	if err != nil {
		h.log.Error("encode connector frame", "error", err)
		return false
	}
	body, err := json.Marshal(protocol.Envelope{Type: frameType, Data: data})
	if err != nil {
		return false
	}
	return h.SendRaw(connectorID, body)
}

// SendRaw delivers an already-encoded frame.
func (h *ConnectorHub) SendRaw(connectorID string, body []byte) bool {
	h.mu.RLock()
	conn := h.byID[connectorID]
	h.mu.RUnlock()
	if conn == nil || conn.closed() {
		return false
	}
	select {
	case conn.outbound <- body:
		return true
	default:
		// A connector that cannot keep up is not something a phone should wait
		// on; dropping the frame is visible as a missing event and the client
		// recovers through the session log.
		h.log.Warn("connector outbound queue full; dropping frame", "connector", connectorID)
		return false
	}
}

// drop closes and removes a connector's socket, used when its credential is
// revoked or the connector is deleted. It returns the socket it removed so the
// caller can republish the sessions it was making live.
func (h *ConnectorHub) drop(connectorID string) *connectorConn {
	h.mu.Lock()
	conn := h.byID[connectorID]
	delete(h.byID, connectorID)
	// A revoked connector is offline; its presence set is no longer true.
	delete(h.live, connectorID)
	h.mu.Unlock()
	if conn != nil {
		conn.close()
	}
	return conn
}

// register installs a socket, replacing any previous one.
//
// The previous process's presence set is dropped here, not only in the old
// socket's cleanup: that cleanup sees a replaced socket and does nothing, so a
// reconnect would otherwise show the sessions of a process that is already gone
// as live until the new hello arrives.
func (h *ConnectorHub) register(conn *connectorConn) *connectorConn {
	h.mu.Lock()
	previous := h.byID[conn.id]
	h.byID[conn.id] = conn
	delete(h.live, conn.id)
	h.mu.Unlock()
	if previous != nil {
		previous.close()
	}
	return previous
}

// isCurrent reports whether conn is still the registered socket for its
// connector. A replaced socket can deliver a last buffered frame before its
// reader unwinds; that frame describes the process the new socket replaced, so
// it must not rewrite the new connection's state.
func (h *ConnectorHub) isCurrent(conn *connectorConn) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.byID[conn.id] == conn
}

// unregister removes a socket only if it is still the current one.
func (h *ConnectorHub) unregister(conn *connectorConn) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.byID[conn.id] != conn {
		return false
	}
	delete(h.byID, conn.id)
	// The connector is offline, so none of the sessions it reported can still
	// accept a prompt. The caller republishes their views from the offline row.
	delete(h.live, conn.id)
	return true
}

// ServeConnectorWS upgrades a request to the connector link.
//
// The connector authenticates with its pairing token in the Authorization
// header; the connector id comes from that token, never from the URL, so a
// token cannot be replayed against another connector.
func (a *App) ServeConnectorWS(w http.ResponseWriter, r *http.Request) {
	connector, ok := a.authenticateConnector(r)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // origin is checked by the token, not the browser
	})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(8 << 20)

	connectionID := NewConnectionID()
	conn := &connectorConn{
		id:           connector.ID,
		userID:       connector.UserID,
		connectionID: connectionID,
		ws:           ws,
		outbound:     make(chan []byte, 256),
		done:         make(chan struct{}),
	}

	previous := a.connectors.register(conn)
	if previous != nil {
		a.log.Info("replacing a stale connector socket", "connector", connector.ID)
	}
	if err := a.db.SetConnectorPresence(r.Context(), connector.ID, connectionID, true); err != nil {
		a.log.Warn("could not record connector presence", "error", err)
	}

	// A socket that stops sending beats without closing is still "online" to
	// the hub. Close it so the web views learn the truth through the same
	// disconnect path as a clean close.
	watchdog := time.AfterFunc(connectorBeatTimeout, func() {
		a.log.Warn("connector heartbeat timed out; closing socket", "connector", connector.ID)
		conn.ws.CloseNow()
	})
	defer watchdog.Stop()

	defer func() {
		if a.connectors.unregister(conn) {
			conn.close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// SetConnectorPresence also moves any session left mid-turn back
			// to idle, because nothing can still be running on that machine.
			if err := a.db.SetConnectorPresence(ctx, connector.ID, connectionID, false); err != nil {
				a.log.Warn("could not clear connector presence", "error", err)
			}
			// Questions this connector was blocked on can no longer be
			// answered; retract them instead of leaving dead cards behind.
			a.expireConnectorApprovals(ctx, connector.ID)
			// unregister already removed the socket, so these views render the
			// connector as offline. publishConnectorPresence also wakes the
			// dashboard lists, which read a snapshot rather than this broker.
			a.publishConnectorPresence(ctx, conn.userID, connector.ID, false)
		}
	}()

	go a.writeConnectorLoop(conn)

	for {
		_, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		watchdog.Reset(connectorBeatTimeout)
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			a.log.Debug("dropping malformed connector frame", "error", err)
			continue
		}
		a.handleConnectorFrame(r.Context(), conn, env)
	}
}

// writeConnectorLoop drains the outbound queue into the socket.
func (a *App) writeConnectorLoop(conn *connectorConn) {
	for {
		var body []byte
		select {
		case <-conn.done:
			// The socket was replaced or revoked. close() already ran, and the
			// reader will notice the closed websocket and unwind.
			conn.ws.CloseNow()
			return
		case body = <-conn.outbound:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		err := conn.ws.Write(ctx, websocket.MessageText, body)
		cancel()
		if err != nil {
			conn.ws.CloseNow()
			return
		}
	}
}

// handleConnectorFrame routes one inbound frame.
func (a *App) handleConnectorFrame(ctx context.Context, conn *connectorConn, env protocol.Envelope) {
	// A replaced socket may still hand one buffered frame to its reader. The
	// new socket is the only authority on bridge and presence state, so a stale
	// hello, beat or pong must not overwrite what it reported.
	if !a.connectors.isCurrent(conn) {
		return
	}
	switch env.Type {
	case protocol.TypeHello:
		var hello protocol.Hello
		if err := json.Unmarshal(env.Data, &hello); err != nil {
			return
		}
		conn.setCapabilities(hello.Capabilities)
		a.log.Info("connector hello",
			"connector", conn.id,
			"version", hello.Version,
			"platform", hello.Platform,
			"bridge", hello.Capabilities[protocol.CapabilityBridge],
			"bridgeConfigured", hello.Capabilities[protocol.CapabilityBridgeConfigured],
			"sessionOpen", hello.Capabilities[protocol.CapabilitySessionOpen],
			"liveSessions", len(hello.LiveSessions))
		a.applyLiveSessions(ctx, conn, hello.LiveSessions)
		a.publishConnectorPresence(ctx, conn.userID, conn.id, true)
		a.healStalledSessions(ctx, conn.id, conn.userID)
	case protocol.TypeInventory:
		var inv protocol.Inventory
		if err := json.Unmarshal(env.Data, &inv); err != nil {
			return
		}
		a.applyInventory(ctx, conn, inv)
	case protocol.TypeAgentActivity:
		var msg protocol.AgentActivity
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		a.onAgentActivity(ctx, conn, msg)
	case protocol.TypeRPCResponse:
		a.deliverRPC(env.Data)
	case protocol.TypeHistory:
		var msg protocol.History
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		complete := msg.Complete == nil || *msg.Complete
		if a.applyHistory(ctx, conn, msg) && msg.Version != "" && complete {
			a.connectors.Send(conn.id, protocol.TypeHistoryAck, protocol.HistoryAck{
				SessionID: msg.SessionID,
				Version:   msg.Version,
				Full:      msg.Full,
				Complete:  true,
			})
		}
	case protocol.TypeBeat:
		// Both facts are optional on the wire. An older connector's ping reply
		// is {"pong":true}: it carries no bridge flag and no presence list, and
		// treating either absence as "false"/"empty" would clear the state its
		// previous beat reported.
		var beat struct {
			Bridge       *bool           `json:"bridge"`
			LiveSessions json.RawMessage `json:"liveSessions"`
		}
		if len(env.Data) > 0 {
			if err := json.Unmarshal(env.Data, &beat); err == nil {
				if beat.Bridge != nil {
					conn.setBridge(*beat.Bridge)
				}
				// The key itself is the signal, not its value: "liveSessions":
				// null or [] means the connector confirms nothing is live,
				// while an absent key is an older build that cannot say.
				if len(beat.LiveSessions) > 0 {
					var live []string
					if err := json.Unmarshal(beat.LiveSessions, &live); err == nil {
						a.applyLiveSessions(ctx, conn, live)
					}
				}
			}
		}
		if err := a.db.TouchConnector(ctx, conn.id); err != nil {
			a.log.Debug("could not refresh connector presence", "error", err)
		}
	case protocol.TypeTurnBegin:
		var msg protocol.TurnBegin
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		a.onTurnBegin(ctx, conn, msg)
	case protocol.TypeTurnDelta:
		var msg protocol.TurnDelta
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		a.onTurnDelta(ctx, conn, msg)
	case protocol.TypeTurnEnd:
		var msg protocol.TurnEnd
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		a.onTurnEnd(ctx, conn, msg)
	case protocol.TypeToolBegin:
		var msg protocol.ToolBegin
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		a.onToolBegin(ctx, conn, msg)
	case protocol.TypeToolEnd:
		var msg protocol.ToolEnd
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		a.onToolEnd(ctx, conn, msg)
	case protocol.TypeApprovalRequest:
		var msg protocol.ApprovalRequest
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		a.onApprovalRequest(ctx, conn, msg)
	case protocol.TypeApprovalCancel:
		var msg protocol.ApprovalCancel
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		a.onApprovalCancel(ctx, conn, msg)
	default:
		a.log.Debug("ignoring unknown connector frame", "type", env.Type)
	}
}

// authenticateConnector resolves the bearer token to a connector.
func (a *App) authenticateConnector(r *http.Request) (connectorRecord, bool) {
	token := bearerToken(r)
	if token == "" {
		return connectorRecord{}, false
	}
	hash := HashToken(token)
	conn, err := a.db.ConnectorByTokenHash(r.Context(), hash)
	if err != nil {
		if !errors.Is(err, errNotFound) {
			a.log.Warn("connector lookup failed", "error", err)
		}
		return connectorRecord{}, false
	}
	return connectorRecord{ID: conn.ID, UserID: conn.UserID, Name: conn.Name}, true
}

// applyLiveSessions records the connector's latest presence report and tells
// the sessions whose live flag moved. A beat that repeats the same set costs one
// comparison and no frames.
func (a *App) applyLiveSessions(ctx context.Context, conn *connectorConn, externalIDs []string) {
	changed := a.connectors.SetLiveSessions(conn, externalIDs)
	if len(changed) == 0 {
		return
	}
	sessions, err := a.db.SessionsForConnector(ctx, conn.id)
	if err != nil {
		a.log.Debug("could not list sessions for presence", "connector", conn.id, "error", err)
		return
	}
	changedSet := make(map[string]struct{}, len(changed))
	for _, id := range changed {
		changedSet[id] = struct{}{}
	}
	for _, session := range sessions {
		if _, ok := changedSet[session.ExternalID]; !ok {
			continue
		}
		a.publishSessionMeta(ctx, session)
	}
}

// publishConnectorPresence tells every open session view that its connector
// came online or went away, so the dashboard's status dot follows reality.
func (a *App) publishConnectorPresence(ctx context.Context, userID, connectorID string, online bool) {
	sessions, err := a.db.SessionsForConnector(ctx, connectorID)
	if err != nil {
		return
	}
	for _, session := range sessions {
		frame, err := newEnvelope(session.ID, session.UpdatedSeq, EventSessionMeta, map[string]any{
			"session": a.sessionView(ctx, session),
		})
		if err != nil {
			continue
		}
		a.broker.Publish(session.ID, frame)
	}
	// The list and sidebar read the dashboard snapshot, not a per-session
	// socket. A connector coming online changes both connectorStatus and live,
	// so the person's open lists have to be told to refetch as well.
	a.dashboards.notify(userID)
}

// healStalledSessions clears turn state left behind by the previous connection.
func (a *App) healStalledSessions(ctx context.Context, connectorID string, userID string) {
	ids, err := a.db.ResetStalledSessions(ctx, connectorID)
	if err != nil {
		a.log.Warn("could not reset stalled sessions", "error", err)
		return
	}
	for _, id := range ids {
		session, err := a.db.SessionByIDAny(ctx, a.db.Read(), id)
		if err != nil {
			continue
		}
		a.publishSessionMeta(ctx, session)
	}
	if len(ids) > 0 {
		a.log.Info("cleared stalled sessions", "connector", connectorID, "count", len(ids))
		a.dashboards.notify(userID)
	}
}
