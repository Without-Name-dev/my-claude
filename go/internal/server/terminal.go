package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

const (
	// terminalReplayChunks / terminalReplayBytes bound the in-memory scrollback
	// kept for reconnects. Bytes matter more than chunks: a compiler emits tens
	// of tiny lines, a progress bar emits few huge ones.
	terminalReplayChunks = 512
	terminalReplayBytes  = 1 << 20
	// terminalSubBuffer is the per-viewer queue. A viewer that falls this far
	// behind has its oldest frame dropped rather than stalling every other
	// viewer and the connector link.
	terminalSubBuffer = 128
	// terminalCreateWait is how long POST waits for the connector to confirm
	// the PTY started. On timeout the terminal is still returned as
	// "starting"; the stream works as soon as the PTY comes up.
	terminalCreateWait = 10 * time.Second
	// terminalProbeWait bounds how long an attaching viewer waits for the
	// connector to confirm a terminal is still alive. The connector has to
	// bring up its output link first, which is local and fast; anything beyond
	// this means the PTY (or the connector) is gone.
	terminalProbeWait = 5 * time.Second
	terminalMaxLabel  = 64
	// terminalUnavailableMessage is shown verbatim by the web panel, which
	// renders stream error messages as-is.
	terminalUnavailableCode    = "terminal_unavailable"
	terminalUnavailableMessage = "终端已失效，请新建"
)

// TerminalView is the terminal shape clients render (rest-api.md 1.7).
type TerminalView struct {
	TerminalID      string  `json:"terminalId"`
	SessionID       string  `json:"sessionId"`
	Label           string  `json:"label"`
	Root            string  `json:"root"`
	CWD             string  `json:"cwd"`
	Cols            int     `json:"cols"`
	Rows            int     `json:"rows"`
	Purpose         string  `json:"purpose"`
	PID             *int    `json:"pid"`
	Status          string  `json:"status"`
	ExitCode        *int    `json:"exitCode"`
	ScrollbackBytes int     `json:"scrollbackBytes"`
	ScrollbackSeq   int64   `json:"scrollbackSeq"`
	Persistent      bool    `json:"persistent"`
	EphemeralGroup  *string `json:"ephemeralGroupId"`
	CreatedAt       string  `json:"createdAt"`
}

// terminalReplayLimits lets tests shrink the window without changing the
// production constants.
type terminalReplayLimits struct {
	chunks    int
	bytes     int
	subBuffer int
}

func defaultTerminalLimits() terminalReplayLimits {
	return terminalReplayLimits{
		chunks:    terminalReplayChunks,
		bytes:     terminalReplayBytes,
		subBuffer: terminalSubBuffer,
	}
}

// terminalSubscriber is one viewer's bounded queue. Frames are already encoded
// JSON, so fan-out does no marshaling per viewer.
type terminalSubscriber struct {
	ch      chan []byte
	dropped atomic.Uint64
}

// push enqueues without blocking. A full queue loses its oldest frame, which
// is the least surprising behaviour for a terminal: the newest screen matters.
func (s *terminalSubscriber) push(frame []byte) {
	select {
	case s.ch <- frame:
		return
	default:
	}
	select {
	case <-s.ch:
		s.dropped.Add(1)
	default:
	}
	select {
	case s.ch <- frame:
	default:
		s.dropped.Add(1)
	}
}

// terminalStream is the live state of one terminal: its replay window, its
// viewers and its exit status.
type terminalStream struct {
	mu          sync.Mutex
	limits      terminalReplayLimits
	replay      []protocol.TerminalOutput
	replayBytes int
	lastSeq     uint64
	exited      bool
	exitCode    *int
	exitReason  string
	subs        map[*terminalSubscriber]struct{}
}

func newTerminalStream(limits terminalReplayLimits) *terminalStream {
	return &terminalStream{
		limits: limits,
		subs:   map[*terminalSubscriber]struct{}{},
	}
}

// terminalReplay is the coalesced scrollback handed to a reconnecting viewer.
type terminalReplay struct {
	Data []byte
	Seq  uint64
}

// terminalExitState is the exit status known at subscribe time.
type terminalExitState struct {
	Exited   bool
	ExitCode *int
	Reason   string
}

// terminalOpenResult answers a pending create.
type terminalOpenResult struct {
	PID int
	Err *protocol.TerminalError
}

// terminalLinkConn is the live connector-opened output socket.
type terminalLinkConn struct {
	connectorID string
	ws          *websocket.Conn
}

// terminalHub owns every terminal stream of one App.
//
// It is stored in a package map rather than an App field because the only
// allowed edit to app.go is the route registration line; keeping the state
// here leaves the existing struct untouched.
type terminalHub struct {
	mu      sync.Mutex
	limits  terminalReplayLimits
	streams map[string]*terminalStream
	// owners maps terminal id to the connector that hosts the PTY. It is the
	// authority that stops one connector's link from injecting output into
	// another connector's terminal.
	owners map[string]string
	links  map[string]*terminalLinkConn
	// waiters receive the connector's opened/error reply for a create.
	waiters map[string]chan terminalOpenResult
	// stateWaiters receive the connector's answer to a liveness probe. Several
	// viewers may attach to a dead terminal at once, so every waiter is kept.
	stateWaiters map[string][]chan protocol.TerminalState
}

func newTerminalHub() *terminalHub {
	return newTerminalHubWithLimits(defaultTerminalLimits())
}

func newTerminalHubWithLimits(limits terminalReplayLimits) *terminalHub {
	if limits.chunks <= 0 {
		limits.chunks = terminalReplayChunks
	}
	if limits.bytes <= 0 {
		limits.bytes = terminalReplayBytes
	}
	if limits.subBuffer <= 0 {
		limits.subBuffer = terminalSubBuffer
	}
	return &terminalHub{
		limits:       limits,
		streams:      map[string]*terminalStream{},
		owners:       map[string]string{},
		links:        map[string]*terminalLinkConn{},
		waiters:      map[string]chan terminalOpenResult{},
		stateWaiters: map[string][]chan protocol.TerminalState{},
	}
}

// appTerminalHubs binds one hub to one App. The map is process-local, like the
// ticket manager and the connector hub; this deployment runs a single server
// process.
var appTerminalHubs sync.Map // *App -> *terminalHub

// terminalHub returns the App's hub, creating it on first use.
func (a *App) terminalHub() *terminalHub {
	if value, ok := appTerminalHubs.Load(a); ok {
		return value.(*terminalHub)
	}
	hub := newTerminalHub()
	actual, _ := appTerminalHubs.LoadOrStore(a, hub)
	return actual.(*terminalHub)
}

// bind records which connector hosts a terminal.
func (h *terminalHub) bind(terminalID, connectorID string) {
	h.mu.Lock()
	h.owners[terminalID] = connectorID
	h.mu.Unlock()
}

// ownedBy reports whether connectorID hosts terminalID.
func (h *terminalHub) ownedBy(connectorID, terminalID string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	owner, ok := h.owners[terminalID]
	return ok && owner == connectorID
}

// getStream returns the stream for a terminal, creating an empty one so a
// viewer can subscribe before the first output chunk arrives.
func (h *terminalHub) getStream(terminalID string) *terminalStream {
	h.mu.Lock()
	defer h.mu.Unlock()
	stream := h.streams[terminalID]
	if stream == nil {
		stream = newTerminalStream(h.limits)
		h.streams[terminalID] = stream
	}
	return stream
}

// evict drops a terminal's replay window and ownership once it can no longer
// produce output. Viewers already attached were sent the exit frame before
// this runs; without eviction the hub keeps every terminal ever opened in
// memory (replay bytes included) for the life of the process.
func (h *terminalHub) evict(terminalID string) {
	h.mu.Lock()
	delete(h.streams, terminalID)
	delete(h.owners, terminalID)
	h.mu.Unlock()
}

// evictIfUnused drops a deleted terminal's stream unless a viewer is still
// attached. An attached viewer must receive the connector's exit frame first,
// and that path evicts the stream itself.
func (h *terminalHub) evictIfUnused(terminalID string) {
	h.mu.Lock()
	if stream := h.streams[terminalID]; stream != nil {
		stream.mu.Lock()
		watched := len(stream.subs) > 0
		stream.mu.Unlock()
		if watched {
			h.mu.Unlock()
			return
		}
	}
	delete(h.streams, terminalID)
	delete(h.owners, terminalID)
	h.mu.Unlock()
}

// applyState folds a connector snapshot into the stream. It returns false when
// the server already has newer output, so a stale snapshot cannot move the
// replay cursor backwards.
func (h *terminalHub) applyState(connectorID string, msg protocol.TerminalState) bool {
	h.mu.Lock()
	owner, ok := h.owners[msg.TerminalID]
	var stream *terminalStream
	if ok && owner == connectorID {
		stream = h.streams[msg.TerminalID]
		if stream == nil {
			stream = newTerminalStream(h.limits)
			h.streams[msg.TerminalID] = stream
		}
	}
	h.mu.Unlock()
	if stream == nil {
		return false
	}
	return stream.applySnapshot(msg)
}

// publishOutput fans one chunk out to every viewer.
func (h *terminalHub) publishOutput(connectorID string, msg protocol.TerminalOutput) bool {
	h.mu.Lock()
	owner, ok := h.owners[msg.TerminalID]
	var stream *terminalStream
	if ok && owner == connectorID {
		stream = h.streams[msg.TerminalID]
		if stream == nil {
			stream = newTerminalStream(h.limits)
			h.streams[msg.TerminalID] = stream
		}
	}
	h.mu.Unlock()
	if stream == nil {
		return false
	}
	stream.publish(msg)
	return true
}

// publish appends to the replay window and fans out.
func (st *terminalStream) publish(msg protocol.TerminalOutput) {
	frame, err := json.Marshal(map[string]any{
		"type": "output",
		"data": msg.Data,
		"seq":  msg.Seq,
	})
	if err != nil {
		return
	}
	if _, err := base64.StdEncoding.DecodeString(msg.Data); err != nil {
		return
	}

	st.mu.Lock()
	defer st.mu.Unlock()

	// A duplicate or an out-of-order chunk after a link reconnect is dropped:
	// viewers already saw it, and sequences must only move forward.
	if msg.Seq <= st.lastSeq {
		return
	}
	st.lastSeq = msg.Seq
	st.replay = append(st.replay, msg)
	// Count and release the same quantity. Adding the decoded length while
	// subtracting the stored base64 length made the window hold about twice the
	// configured ceiling: the counter drifted low and trimming stopped early.
	st.replayBytes += len(msg.Data)
	for (len(st.replay) > st.limits.chunks || st.replayBytes > st.limits.bytes) &&
		len(st.replay) > 1 {
		st.replayBytes -= len(st.replay[0].Data)
		st.replay = st.replay[1:]
	}
	for sub := range st.subs {
		sub.push(frame)
	}
}

// applySnapshot replaces the replay window with the connector's bounded
// scrollback. A viewer that is already attached receives a replay frame (the
// web panel resets the terminal and repaints), so a link reconnect heals both
// the stored history and what the person sees.
//
// A snapshot at the same sequence is accepted when it carries more history
// than the server holds: after a server restart the queued live chunks can
// reach the server before the snapshot does, leaving lastSeq already caught up
// while the older output is still missing.
func (st *terminalStream) applySnapshot(msg protocol.TerminalState) bool {
	if _, err := base64.StdEncoding.DecodeString(msg.Data); err != nil {
		return false
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if msg.Seq < st.lastSeq {
		return false
	}
	// replayBytes counts the stored base64 text, exactly like publish does.
	if msg.Seq == st.lastSeq && len(msg.Data) <= st.replayBytes {
		return false
	}
	st.lastSeq = msg.Seq
	st.replay = []protocol.TerminalOutput{{
		TerminalID: msg.TerminalID,
		Seq:        msg.Seq,
		Data:       msg.Data,
	}}
	st.replayBytes = len(msg.Data)
	if msg.Data == "" {
		return true
	}
	frame, err := json.Marshal(map[string]any{
		"type": "replay",
		"data": msg.Data,
		"seq":  msg.Seq,
	})
	if err != nil {
		return true
	}
	for sub := range st.subs {
		sub.push(frame)
	}
	return true
}

// publishExit records the final state and tells every viewer.
func (h *terminalHub) publishExit(terminalID string, code *int, reason string) {
	stream := h.getStream(terminalID)
	frame, err := json.Marshal(map[string]any{
		"type":     "exit",
		"exitCode": code,
		"reason":   reason,
	})
	if err != nil {
		return
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.exited = true
	stream.exitCode = code
	stream.exitReason = reason
	for sub := range stream.subs {
		sub.push(frame)
	}
}

// publishError forwards a connector-side failure to viewers. The spec allows
// an error code to be a string or an integer; connector codes are strings.
func (h *terminalHub) publishError(terminalID, code, message string) {
	frame, err := json.Marshal(map[string]any{
		"type":    "error",
		"code":    code,
		"message": message,
	})
	if err != nil {
		return
	}
	stream := h.getStream(terminalID)
	stream.mu.Lock()
	defer stream.mu.Unlock()
	for sub := range stream.subs {
		sub.push(frame)
	}
}

// subscribe registers a viewer and snapshots the replay window atomically, so
// no chunk can slip between the snapshot and the live queue.
func (h *terminalHub) subscribe(terminalID string, afterSeq uint64) (*terminalSubscriber, terminalReplay, terminalExitState) {
	stream := h.getStream(terminalID)
	sub := &terminalSubscriber{ch: make(chan []byte, h.limits.subBuffer)}

	stream.mu.Lock()
	defer stream.mu.Unlock()
	stream.subs[sub] = struct{}{}

	seq := stream.lastSeq
	if seq < afterSeq {
		seq = afterSeq
	}
	var data []byte
	for _, chunk := range stream.replay {
		if chunk.Seq <= afterSeq {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(chunk.Data)
		if err != nil {
			continue
		}
		data = append(data, raw...)
		seq = chunk.Seq
	}

	var exitCode *int
	if stream.exitCode != nil {
		value := *stream.exitCode
		exitCode = &value
	}
	return sub, terminalReplay{Data: data, Seq: seq}, terminalExitState{
		Exited:   stream.exited,
		ExitCode: exitCode,
		Reason:   stream.exitReason,
	}
}

// unsubscribe removes a viewer.
func (h *terminalHub) unsubscribe(terminalID string, sub *terminalSubscriber) {
	stream := h.getStream(terminalID)
	stream.mu.Lock()
	delete(stream.subs, sub)
	stream.mu.Unlock()
}

// streamState is what the HTTP views need to know about live output.
func (h *terminalHub) streamState(terminalID string) (uint64, bool, *int, string) {
	h.mu.Lock()
	stream := h.streams[terminalID]
	h.mu.Unlock()
	if stream == nil {
		return 0, false, nil, ""
	}
	stream.mu.Lock()
	defer stream.mu.Unlock()
	var exitCode *int
	if stream.exitCode != nil {
		value := *stream.exitCode
		exitCode = &value
	}
	return stream.lastSeq, stream.exited, exitCode, stream.exitReason
}

// expectOpen registers interest in a create's reply before the command is
// sent, so a fast connector cannot answer into the void.
func (h *terminalHub) expectOpen(terminalID string) chan terminalOpenResult {
	ch := make(chan terminalOpenResult, 1)
	h.mu.Lock()
	h.waiters[terminalID] = ch
	h.mu.Unlock()
	return ch
}

// forgetOpen cleans up a create that timed out.
func (h *terminalHub) forgetOpen(terminalID string) {
	h.mu.Lock()
	delete(h.waiters, terminalID)
	h.mu.Unlock()
}

// resolveOpen delivers a successful start to the waiting create.
func (h *terminalHub) resolveOpen(terminalID string, pid int) {
	h.mu.Lock()
	ch := h.waiters[terminalID]
	delete(h.waiters, terminalID)
	h.mu.Unlock()
	if ch != nil {
		select {
		case ch <- terminalOpenResult{PID: pid}:
		default:
		}
	}
}

// resolveOpenError delivers a start failure to the waiting create.
func (h *terminalHub) resolveOpenError(terminalID string, failure protocol.TerminalError) {
	h.mu.Lock()
	ch := h.waiters[terminalID]
	delete(h.waiters, terminalID)
	h.mu.Unlock()
	if ch != nil {
		select {
		case ch <- terminalOpenResult{Err: &failure}:
		default:
		}
	}
}

// expectState registers interest in a probe reply before the command is sent.
func (h *terminalHub) expectState(terminalID string) chan protocol.TerminalState {
	ch := make(chan protocol.TerminalState, 1)
	h.mu.Lock()
	h.stateWaiters[terminalID] = append(h.stateWaiters[terminalID], ch)
	h.mu.Unlock()
	return ch
}

// forgetState removes one probe waiter that timed out.
func (h *terminalHub) forgetState(terminalID string, ch chan protocol.TerminalState) {
	h.mu.Lock()
	waiters := h.stateWaiters[terminalID]
	for index, waiter := range waiters {
		if waiter == ch {
			waiters = append(waiters[:index], waiters[index+1:]...)
			break
		}
	}
	if len(waiters) == 0 {
		delete(h.stateWaiters, terminalID)
	} else {
		h.stateWaiters[terminalID] = waiters
	}
	h.mu.Unlock()
}

// resolveState delivers a probe reply to every waiting viewer.
func (h *terminalHub) resolveState(msg protocol.TerminalState) {
	h.mu.Lock()
	waiters := h.stateWaiters[msg.TerminalID]
	delete(h.stateWaiters, msg.TerminalID)
	h.mu.Unlock()
	for _, ch := range waiters {
		select {
		case ch <- msg:
		default:
		}
	}
}

// attachLink installs a connector's output socket, closing the previous one.
func (h *terminalHub) attachLink(connectorID string, ws *websocket.Conn) *terminalLinkConn {
	link := &terminalLinkConn{connectorID: connectorID, ws: ws}
	h.mu.Lock()
	previous := h.links[connectorID]
	h.links[connectorID] = link
	h.mu.Unlock()
	if previous != nil && previous.ws != nil {
		previous.ws.CloseNow()
	}
	return link
}

// detachLink removes a link only if it is still the current one.
func (h *terminalHub) detachLink(link *terminalLinkConn) {
	h.mu.Lock()
	if h.links[link.connectorID] == link {
		delete(h.links, link.connectorID)
	}
	h.mu.Unlock()
}

// registerTerminalRoutes installs the terminals-v2 API and both websockets.
//
// The connector link is registered here (not in app.go) so the only change to
// the existing router is one call.
func (a *App) registerTerminalRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/terminals-v2",
		a.requireUser(a.handleTerminalCreateV2))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/terminals-v2",
		a.requireUser(a.handleTerminalListV2))
	mux.HandleFunc("PATCH /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}",
		a.requireUser(a.handleTerminalRenameV2))
	// The task names a /rename suffix and the spec names the bare id; both are
	// cheap and different clients have shipped with each.
	mux.HandleFunc("PATCH /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}/rename",
		a.requireUser(a.handleTerminalRenameV2))
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}/close",
		a.requireUser(a.handleTerminalCloseV2))
	mux.HandleFunc("DELETE /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}",
		a.requireUser(a.handleTerminalDeleteV2))
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}/ticket",
		a.requireUser(a.handleTerminalTicket))
	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/terminals-v2/{terminal_id}/stream",
		a.handleTerminalStream)
	mux.HandleFunc("GET "+protocol.TerminalLinkPath, a.handleTerminalLinkWS)
}

// ownedTerminal resolves the connector and terminal, answering 404 for either
// one that does not belong to the caller.
func (a *App) ownedTerminal(
	w http.ResponseWriter, r *http.Request, user store.User,
) (store.Connector, store.Terminal, bool) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return store.Connector{}, store.Terminal{}, false
	}
	terminal, err := a.db.TerminalByID(r.Context(), r.PathValue("terminal_id"))
	if err != nil || terminal.UserID != user.ID || terminal.ConnectorID != connector.ID {
		httpx.Fail(w, httpx.NotFound("terminal not found"))
		return store.Connector{}, store.Terminal{}, false
	}
	return connector, terminal, true
}

// handleTerminalCreateV2 starts a PTY on the connector.
func (a *App) handleTerminalCreateV2(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	if !a.connectors.Online(connector.ID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}

	var body struct {
		SessionID string `json:"sessionId"`
		CWD       string `json:"cwd"`
		Root      string `json:"root"`
		Shell     string `json:"shell"`
		Label     string `json:"label"`
		Cols      int    `json:"cols"`
		Rows      int    `json:"rows"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}

	root := strings.TrimSpace(r.URL.Query().Get("root"))
	if root == "" {
		root = strings.TrimSpace(body.Root)
	}
	cols, rows := clampTerminalDimensions(body.Cols, body.Rows)
	label := truncateLabel(strings.TrimSpace(body.Label))
	if label == "" {
		label = "Terminal"
	}
	sessionID := strings.TrimSpace(body.SessionID)
	if sessionID == "" {
		sessionID = "browse_" + connector.ID
	}
	cwd := resolveTerminalCWD(root, body.CWD)

	terminal, err := a.db.CreateTerminal(r.Context(), store.Terminal{
		ConnectorID: connector.ID,
		UserID:      user.ID,
		SessionID:   sessionID,
		Label:       label,
		Root:        root,
		CWD:         cwd,
		Shell:       strings.TrimSpace(body.Shell),
		Cols:        cols,
		Rows:        rows,
		Status:      "starting",
	})
	if err != nil {
		httpx.Fail(w, err)
		return
	}

	hub := a.terminalHub()
	hub.bind(terminal.ID, connector.ID)
	opened := hub.expectOpen(terminal.ID)
	defer hub.forgetOpen(terminal.ID)

	sent := a.connectors.Send(connector.ID, protocol.TypeTerminal, protocol.TerminalCommand{
		Op:         protocol.TerminalOpOpen,
		TerminalID: terminal.ID,
		SessionID:  sessionID,
		// Send the pieces unresolved: the connector joins them using the
		// desktop's path rules, which may differ from the server's.
		Root:  root,
		CWD:   strings.TrimSpace(body.CWD),
		Shell: terminal.Shell,
		Cols:  cols,
		Rows:  rows,
	})
	if !sent {
		_ = a.db.DeleteTerminal(r.Context(), terminal.ID, user.ID)
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}

	select {
	case result := <-opened:
		if result.Err != nil {
			_ = a.db.DeleteTerminal(r.Context(), terminal.ID, user.ID)
			httpx.JSON(w, http.StatusBadGateway, map[string]any{
				"detail": map[string]any{
					"code":    result.Err.Code,
					"message": result.Err.Message,
				},
			})
			return
		}
	case <-time.After(terminalCreateWait):
		a.log.Warn("terminal did not confirm startup", "terminal", terminal.ID, "connector", connector.ID)
	case <-r.Context().Done():
		return
	}

	if stored, err := a.db.TerminalByID(r.Context(), terminal.ID); err == nil {
		terminal = stored
	}
	httpx.OK(w, map[string]any{
		"ok":     true,
		"result": a.terminalView(terminal),
		"error":  nil,
	})
}

// handleTerminalListV2 lists the terminals the caller created on a connector.
func (a *App) handleTerminalListV2(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	terminals, err := a.db.ListTerminals(r.Context(), connector.ID, user.ID)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	hub := a.terminalHub()
	views := make([]TerminalView, 0, len(terminals))
	for _, terminal := range terminals {
		hub.bind(terminal.ID, connector.ID)
		views = append(views, a.terminalView(terminal))
	}
	httpx.OK(w, map[string]any{
		"ok":     true,
		"result": map[string]any{"terminals": views},
		"error":  nil,
	})
}

// handleTerminalRenameV2 changes a terminal's label.
func (a *App) handleTerminalRenameV2(w http.ResponseWriter, r *http.Request, user store.User) {
	_, terminal, ok := a.ownedTerminal(w, r, user)
	if !ok {
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	label := truncateLabel(strings.TrimSpace(body.Label))
	if label == "" {
		httpx.Fail(w, httpx.BadRequest("label must not be empty"))
		return
	}
	renamed, err := a.db.RenameTerminal(r.Context(), terminal.ID, user.ID, label)
	if err != nil {
		if err == store.ErrNotFound {
			httpx.Fail(w, httpx.NotFound("terminal not found"))
			return
		}
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{"ok": true, "result": a.terminalView(renamed), "error": nil})
}

// handleTerminalCloseV2 stops the PTY but keeps the row as exited.
func (a *App) handleTerminalCloseV2(w http.ResponseWriter, r *http.Request, user store.User) {
	a.closeTerminalV2(w, r, user, false)
}

// handleTerminalDeleteV2 stops the PTY and removes the row. The live stream
// keeps its subscription, so a viewer still receives the exit frame.
func (a *App) handleTerminalDeleteV2(w http.ResponseWriter, r *http.Request, user store.User) {
	a.closeTerminalV2(w, r, user, true)
}

func (a *App) closeTerminalV2(w http.ResponseWriter, r *http.Request, user store.User, deleteRow bool) {
	connector, terminal, ok := a.ownedTerminal(w, r, user)
	if !ok {
		return
	}
	if !a.connectors.Online(connector.ID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	if !a.connectors.Send(connector.ID, protocol.TypeTerminal, protocol.TerminalCommand{
		Op:         protocol.TerminalOpClose,
		TerminalID: terminal.ID,
	}) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	if deleteRow {
		_ = a.db.DeleteTerminal(r.Context(), terminal.ID, user.ID)
		a.terminalHub().evictIfUnused(terminal.ID)
	} else {
		_ = a.db.MarkTerminalExited(r.Context(), terminal.ID, terminal.ExitCode)
	}
	httpx.OK(w, map[string]any{
		"ok": true,
		"result": map[string]any{
			"terminalId": terminal.ID,
			"closed":     true,
		},
		"error": nil,
	})
}

// handleTerminalTicket mints a single-use stream ticket scoped to one
// terminal. It uses the same TicketManager as session and dashboard sockets;
// the web client also accepts a plain user token in ?token=.
func (a *App) handleTerminalTicket(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, terminal, ok := a.ownedTerminal(w, r, user)
	if !ok {
		return
	}
	var body struct {
		ClientID string `json:"clientId"`
	}
	_ = httpx.DecodeOptional(r, &body)
	ticket, expires := a.tickets.Issue(user.ID, body.ClientID, TicketScope{
		ConnectorID: connector.ID,
		TerminalID:  terminal.ID,
	})
	if ticket == "" {
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, "could not issue a ticket"))
		return
	}
	httpx.OK(w, map[string]any{
		"ticket":     ticket,
		"expiresAt":  isoTime(expires.UnixMilli()),
		"serverTime": timestampNow(),
	})
}

// terminalView renders one row plus the live sequence the hub knows.
func (a *App) terminalView(terminal store.Terminal) TerminalView {
	lastSeq, exited, liveExitCode, _ := a.terminalHub().streamState(terminal.ID)

	status := terminal.Status
	exitCode := terminal.ExitCode
	if exited {
		status = "exited"
		if liveExitCode != nil {
			exitCode = liveExitCode
		}
	}
	var pid *int
	if terminal.PID > 0 {
		value := terminal.PID
		pid = &value
	}
	seq := terminal.ScrollbackSeq
	if int64(lastSeq) > seq {
		seq = int64(lastSeq)
	}
	return TerminalView{
		TerminalID:      terminal.ID,
		SessionID:       terminal.SessionID,
		Label:           terminal.Label,
		Root:            terminal.Root,
		CWD:             terminal.CWD,
		Cols:            terminal.Cols,
		Rows:            terminal.Rows,
		Purpose:         "user",
		PID:             pid,
		Status:          status,
		ExitCode:        exitCode,
		ScrollbackBytes: 0,
		ScrollbackSeq:   seq,
		Persistent:      false,
		CreatedAt:       isoTime(terminal.CreatedAt),
	}
}

// handleTerminalLinkWS receives a connector's terminal output socket.
func (a *App) handleTerminalLinkWS(w http.ResponseWriter, r *http.Request) {
	connector, ok := a.authenticateConnector(r)
	if !ok {
		httpx.Fail(w, httpx.Unauthorized("connector token is required"))
		return
	}
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: true, // authenticated by the connector token above
	})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(4 << 20)

	hub := a.terminalHub()
	// After a server restart the in-memory ownership map is empty; rebuild it
	// before accepting output for terminals that are still running.
	a.bindConnectorTerminals(r.Context(), connector.ID)
	link := hub.attachLink(connector.ID, ws)
	defer hub.detachLink(link)

	for {
		_, data, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		var env protocol.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			a.log.Debug("dropping malformed terminal link frame", "error", err)
			continue
		}
		a.handleTerminalLinkFrame(r.Context(), connector.ID, env)
	}
}

// bindConnectorTerminals rebuilds the hub's ownership map from SQLite.
func (a *App) bindConnectorTerminals(ctx context.Context, connectorID string) {
	terminals, err := a.db.ListTerminals(ctx, connectorID, "")
	if err != nil {
		a.log.Warn("could not load terminals for the link", "connector", connectorID, "error", err)
		return
	}
	hub := a.terminalHub()
	for _, terminal := range terminals {
		hub.bind(terminal.ID, connectorID)
	}
}

// handleTerminalLinkFrame applies one connector → server terminal frame.
func (a *App) handleTerminalLinkFrame(ctx context.Context, connectorID string, env protocol.Envelope) {
	hub := a.terminalHub()
	switch env.Type {
	case protocol.TypeTerminalOpened:
		var msg protocol.TerminalOpened
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		if !hub.ownedBy(connectorID, msg.TerminalID) {
			return
		}
		if err := a.db.MarkTerminalRunning(ctx, msg.TerminalID, msg.PID); err != nil {
			a.log.Debug("could not record a terminal pid", "terminal", msg.TerminalID, "error", err)
		}
		hub.resolveOpen(msg.TerminalID, msg.PID)

	case protocol.TypeTerminalOutput:
		var msg protocol.TerminalOutput
		if err := json.Unmarshal(env.Data, &msg); err != nil || msg.Seq == 0 {
			return
		}
		if _, err := base64.StdEncoding.DecodeString(msg.Data); err != nil {
			return
		}
		if !hub.publishOutput(connectorID, msg) {
			a.log.Debug("terminal output for an unbound terminal",
				"connector", connectorID, "terminal", msg.TerminalID)
		}

	case protocol.TypeTerminalState:
		var msg protocol.TerminalState
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		if !hub.ownedBy(connectorID, msg.TerminalID) {
			return
		}
		if !msg.Running {
			// The connector process restarted (or the shell is gone) and this
			// row is a leftover. Mark it dead so later attaches report the
			// terminal as unavailable instead of waiting forever.
			if err := a.db.MarkTerminalExited(ctx, msg.TerminalID, nil); err != nil {
				a.log.Debug("could not record a missing terminal", "terminal", msg.TerminalID, "error", err)
			}
			hub.publishError(msg.TerminalID, terminalUnavailableCode, terminalUnavailableMessage)
			hub.publishExit(msg.TerminalID, nil, "lost")
			hub.resolveState(msg)
			hub.evict(msg.TerminalID)
			return
		}
		if _, err := base64.StdEncoding.DecodeString(msg.Data); err != nil {
			return
		}
		hub.applyState(connectorID, msg)
		hub.resolveState(msg)

	case protocol.TypeTerminalExit:
		var msg protocol.TerminalExit
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		if !hub.ownedBy(connectorID, msg.TerminalID) {
			return
		}
		if err := a.db.MarkTerminalExited(ctx, msg.TerminalID, msg.ExitCode); err != nil {
			a.log.Debug("could not record a terminal exit", "terminal", msg.TerminalID, "error", err)
		}
		if lastSeq, _, _, _ := hub.streamState(msg.TerminalID); lastSeq > 0 {
			_ = a.db.UpdateTerminalScrollback(ctx, msg.TerminalID, lastSeq)
		}
		hub.resolveOpenError(msg.TerminalID, protocol.TerminalError{
			Code:    "terminal_exited",
			Message: "terminal exited before it finished starting",
		})
		hub.publishExit(msg.TerminalID, msg.ExitCode, msg.Reason)
		hub.evict(msg.TerminalID)

	case protocol.TypeTerminalError:
		var msg protocol.TerminalError
		if err := json.Unmarshal(env.Data, &msg); err != nil {
			return
		}
		if !hub.ownedBy(connectorID, msg.TerminalID) {
			return
		}
		a.log.Warn("connector reported a terminal error",
			"terminal", msg.TerminalID, "code", msg.Code, "error", msg.Message)
		hub.resolveOpenError(msg.TerminalID, msg)
		hub.publishError(msg.TerminalID, msg.Code, msg.Message)
		// A failed open leaves no process behind. Errors on a live terminal
		// (bad input, a failed write) must not mark it dead.
		if msg.Code == "terminal_open_failed" {
			_ = a.db.MarkTerminalExited(ctx, msg.TerminalID, nil)
			hub.evict(msg.TerminalID)
		}

	default:
		a.log.Debug("ignoring unknown terminal link frame", "type", env.Type)
	}
}

// resolveTerminalCWD interprets cwd relative to the workspace root the client
// picked, which is how the dashboard sends it.
func resolveTerminalCWD(root, cwd string) string {
	root = strings.TrimSpace(root)
	cwd = strings.TrimSpace(cwd)
	switch {
	case cwd == "":
		return root
	case root == "" || filepath.IsAbs(cwd):
		return cwd
	default:
		return filepath.Join(root, cwd)
	}
}

func clampTerminalDimensions(cols, rows int) (int, int) {
	if cols < 1 {
		cols = 80
	}
	if cols > 500 {
		cols = 500
	}
	if rows < 1 {
		rows = 24
	}
	if rows > 200 {
		rows = 200
	}
	return cols, rows
}

func truncateLabel(label string) string {
	runes := []rune(label)
	if len(runes) <= terminalMaxLabel {
		return label
	}
	return string(runes[:terminalMaxLabel])
}
