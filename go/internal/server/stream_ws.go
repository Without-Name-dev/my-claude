package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// streamResyncEvery re-lists the person's sessions even when no dashboard
// notify arrived. A session created while no inventory event fires would
// otherwise never get a broker subscription on this socket.
var streamResyncEvery = 15 * time.Second

// streamKeepaliveEvery is how long the unified stream may stay quiet before a
// keepalive frame. It matches the session socket so clients need one rule.
//
// It is a variable rather than a constant only so tests can shorten it; the
// production default is the same 15 seconds the other sockets use.
var streamKeepaliveEvery = keepaliveEvery

// keepaliveDue reports whether a socket that last wrote at lastWrite should
// send a keepalive at now.
//
// The threshold is half the interval rather than the whole interval because a
// keepalive's own write lands just after the tick that sent it: comparing
// against the full interval makes the following tick see "just under 15s" and
// skip, so an otherwise idle socket would only hear from the server every 30s.
// Half still suppresses keepalives while real frames are flowing.
func keepaliveDue(lastWrite, now time.Time, every time.Duration) bool {
	return now.Sub(lastWrite) >= every/2
}

// handleStreamWS carries the dashboard snapshot and every owned session's
// events over one socket.
//
// The first frame is always stream.ready, which lets a client tell "connected"
// apart from "connected but silent". The dashboard snapshot follows before any
// session event so the client has inventory to render against.
func (a *App) handleStreamWS(w http.ResponseWriter, r *http.Request) {
	ticketToken := r.URL.Query().Get("ticket")
	if ticketToken == "" {
		closeWith(w, r, 1008, "missing ticket")
		return
	}
	// This socket replaces the dashboard socket on clients that opt in, so it
	// redeems the same dashboard ticket the client already knows how to ask for.
	ticket, ok := a.tickets.Consume(ticketToken, TicketScope{Dashboard: true})
	if !ok {
		closeWith(w, r, 1008, "invalid ticket")
		return
	}
	user, err := a.db.UserByID(r.Context(), ticket.UserID)
	if err != nil || user.Disabled {
		closeWith(w, r, 1008, "user not found")
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())

	dirty, unsubscribeDashboard := a.dashboards.subscribe(user.ID)
	defer unsubscribeDashboard()

	// The client sends nothing, but reading is how a disconnect is noticed.
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer cancel()
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}()

	subs := map[string]*Subscriber{}
	var pumps sync.WaitGroup
	var writer *streamWriter
	startPump := func(sub *Subscriber) {
		pumps.Add(1)
		go func() {
			defer pumps.Done()
			pumpStreamSession(ctx, sub, writer)
		}()
	}

	// Subscribe before the first frame so an event emitted while the snapshot
	// is being built waits in the broker queue instead of being lost. Pumps
	// start only after the snapshot is on the wire.
	initialSessions, listErr := a.listUserSessions(ctx, user.ID)
	if listErr == nil {
		a.syncStreamSessions(user.ID, initialSessions, subs)
	}

	// Cleanup runs before the deferred ws.CloseNow: cancel aborts blocked
	// writes and reads, unsubscribing stops each pump, and the wait guarantees
	// no goroutine outlives the connection.
	defer func() {
		cancel()
		for sessionID, sub := range subs {
			a.broker.Unsubscribe(sessionID, sub)
		}
		pumps.Wait()
		<-readerDone
	}()

	// stream.ready and the snapshot are written before the writer goroutine
	// starts, so nothing can overtake them. A websocket has one writer at a
	// time; every later frame goes through the writer below.
	ready, _ := json.Marshal(map[string]any{
		"type":       "stream.ready",
		"serverTime": timestampNow(),
	})
	writeCtx, stopWrite := context.WithTimeout(ctx, 10*time.Second)
	err = ws.Write(writeCtx, websocket.MessageText, ready)
	stopWrite()
	if err != nil {
		return
	}
	needsRefresh := false
	if listErr != nil {
		// The session listing failed, so there is nothing to snapshot yet.
		// Retry on the next refresh tick rather than leaving the client without
		// inventory until some unrelated change happens.
		needsRefresh = true
	} else if snapshot, buildErr := a.dashboardSnapshotFor(ctx, user.ID, initialSessions); buildErr == nil {
		writeCtx, stopWrite := context.WithTimeout(ctx, 15*time.Second)
		writeErr := ws.Write(writeCtx, websocket.MessageText, snapshot)
		stopWrite()
		if writeErr != nil {
			return
		}
	} else {
		needsRefresh = true
	}

	// One writer owns the socket from here on: control frames (keepalives) are
	// written ahead of session data and dashboard snapshots coalesce, so a burst
	// of session frames cannot delay them or queue stale snapshots.
	writer = newStreamWriter(ctx, ws, cancel)
	pumps.Add(1)
	go func() {
		defer pumps.Done()
		writer.run()
	}()

	// The initial subscriptions were recorded before the snapshot; now that
	// stream.ready and the snapshot are on the wire, start forwarding them.
	for _, sub := range subs {
		startPump(sub)
	}

	refresh := time.NewTicker(dashboardRefresh)
	defer refresh.Stop()
	keepalive := time.NewTicker(streamKeepaliveEvery)
	defer keepalive.Stop()
	resync := time.NewTicker(streamResyncEvery)
	defer resync.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-dirty:
			// The hub coalesces bursts into one signal, but a long answer still
			// marks the dashboard dirty several times a second. Reconcile and
			// redraw on the throttled refresh tick: doing two full inventory
			// listings per signal would undo the throttle this hub exists for.
			needsRefresh = true
		case <-resync.C:
			// This is how a session created after this socket connected gets a
			// subscription even when no dashboard notification fires for it.
			sessions, err := a.listUserSessions(ctx, user.ID)
			if err != nil {
				continue
			}
			added, changed := a.syncStreamSessions(user.ID, sessions, subs)
			for _, sessionID := range added {
				startPump(subs[sessionID])
			}
			if changed {
				needsRefresh = true
			}
		case <-refresh.C:
			if !needsRefresh {
				continue
			}
			needsRefresh = false
			// One listing serves both the subscription reconcile and the
			// snapshot it feeds.
			sessions, err := a.listUserSessions(ctx, user.ID)
			if err != nil {
				// Keep the redraw pending: a transient database error must not
				// strand the client on a stale snapshot.
				needsRefresh = true
				continue
			}
			added, _ := a.syncStreamSessions(user.ID, sessions, subs)
			for _, sessionID := range added {
				startPump(subs[sessionID])
			}
			snapshot, err := a.dashboardSnapshotFor(ctx, user.ID, sessions)
			if err != nil {
				needsRefresh = true
				continue
			}
			writer.sendSnapshot(snapshot)
		case <-keepalive.C:
			if !keepaliveDue(writer.lastWriteTime(), time.Now(), streamKeepaliveEvery) {
				continue
			}
			body, _ := json.Marshal(map[string]any{
				"type":       EventKeepalive,
				"serverTime": timestampNow(),
			})
			writer.sendControl(body)
		}
	}
}

// syncStreamSessions reconciles subs with an already-listed set of sessions the
// person owns, active and archived. It returns the ids it newly subscribed and
// whether the set changed at all (a removal counts); the caller starts pumps
// for the added ids and redraws the snapshot when anything moved. The caller
// also passes the same listing to dashboardSnapshotFor, so one database read
// feeds both the subscription set and the snapshot.
func (a *App) syncStreamSessions(userID string, sessions []store.Session, subs map[string]*Subscriber) (added []string, changed bool) {
	wanted := make(map[string]struct{}, len(sessions))
	for _, session := range sessions {
		wanted[session.ID] = struct{}{}
	}

	for sessionID, sub := range subs {
		if _, ok := wanted[sessionID]; !ok {
			a.broker.Unsubscribe(sessionID, sub)
			delete(subs, sessionID)
			changed = true
		}
	}

	for sessionID := range wanted {
		if _, ok := subs[sessionID]; ok {
			continue
		}
		subs[sessionID] = a.broker.Subscribe(sessionID)
		added = append(added, sessionID)
		changed = true
	}
	return added, changed
}

// pumpStreamSession forwards one session's frames to the shared writer. The
// writer's queue is the backpressure point: when the socket cannot keep up,
// pumps block here and the broker's per-subscriber queue overflows into a
// refetch marker rather than losing frames silently.
func pumpStreamSession(ctx context.Context, sub *Subscriber, writer *streamWriter) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			return
		case frame := <-sub.Frames():
			if !writer.sendData(frame) {
				return
			}
		}
	}
}

// streamWriter is the single owner of the unified stream's websocket writes.
//
// A websocket permits only one writer at a time. Giving every pump its own
// goroutine plus a shared mutex made the write order arbitrary: a burst of
// session frames could sit in front of a keepalive or a snapshot. Here control
// frames are taken first, snapshots coalesce to the newest, and session frames
// share one bounded queue.
type streamWriter struct {
	ctx       context.Context
	ws        *websocket.Conn
	cancel    context.CancelFunc
	control   chan []byte
	snapshot  chan []byte
	data      chan []byte
	lastWrite atomic.Int64
}

func newStreamWriter(ctx context.Context, ws *websocket.Conn, cancel context.CancelFunc) *streamWriter {
	w := &streamWriter{
		ctx:      ctx,
		ws:       ws,
		cancel:   cancel,
		control:  make(chan []byte, 4),
		snapshot: make(chan []byte, 1),
		data:     make(chan []byte, subscriberQueue),
	}
	w.lastWrite.Store(time.Now().UnixNano())
	return w
}

// lastWriteTime is when the writer last completed a frame.
func (w *streamWriter) lastWriteTime() time.Time { return time.Unix(0, w.lastWrite.Load()) }

// sendControl queues a control frame. Dropping a keepalive when the control
// queue is full is harmless: the next tick sends another one.
func (w *streamWriter) sendControl(body []byte) {
	select {
	case w.control <- body:
	case <-w.ctx.Done():
	default:
	}
}

// sendSnapshot replaces any snapshot that has not been written yet. A client
// that fell behind gets the newest dashboard state instead of a queue of stale
// ones.
func (w *streamWriter) sendSnapshot(body []byte) {
	select {
	case <-w.snapshot:
	default:
	}
	select {
	case w.snapshot <- body:
	case <-w.ctx.Done():
	default:
	}
}

// sendData queues one session frame, reporting false once the socket is done.
func (w *streamWriter) sendData(frame []byte) bool {
	select {
	case <-w.ctx.Done():
		return false
	case w.data <- frame:
		return true
	}
}

// run writes frames until the connection fails or the handler cancels.
func (w *streamWriter) run() {
	for {
		// Control first: a keepalive must not wait behind session traffic.
		select {
		case <-w.ctx.Done():
			return
		case body := <-w.control:
			if !w.write(body) {
				w.cancel()
				return
			}
			continue
		default:
		}
		select {
		case <-w.ctx.Done():
			return
		case body := <-w.control:
			if !w.write(body) {
				w.cancel()
				return
			}
		case body := <-w.snapshot:
			if !w.write(body) {
				w.cancel()
				return
			}
		case body := <-w.data:
			// A snapshot that arrived while this frame waited is the newer
			// statement of session state; write it first.
			select {
			case snap := <-w.snapshot:
				if !w.write(snap) {
					w.cancel()
					return
				}
			default:
			}
			if !w.write(body) {
				w.cancel()
				return
			}
		}
	}
}

// write sends one frame with the per-write deadline the old shared mutex used.
func (w *streamWriter) write(body []byte) bool {
	writeCtx, cancel := context.WithTimeout(w.ctx, 15*time.Second)
	defer cancel()
	if err := w.ws.Write(writeCtx, websocket.MessageText, body); err != nil {
		return false
	}
	w.lastWrite.Store(time.Now().UnixNano())
	return true
}
