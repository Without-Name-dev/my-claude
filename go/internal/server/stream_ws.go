package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
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

	// Every websocket write in this handler shares one mutex: the session pumps
	// run alongside snapshots and keepalives, and a websocket permits only one
	// writer at a time. lastWrite lets the keepalive skip busy periods.
	var writeMu sync.Mutex
	var lastWrite atomic.Int64
	lastWrite.Store(time.Now().UnixNano())
	writeFrame := func(body []byte, timeout time.Duration) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		writeCtx, stop := context.WithTimeout(ctx, timeout)
		defer stop()
		if err := ws.Write(writeCtx, websocket.MessageText, body); err != nil {
			return err
		}
		lastWrite.Store(time.Now().UnixNano())
		return nil
	}

	subs := map[string]*Subscriber{}
	var pumps sync.WaitGroup
	startPump := func(sessionID string, sub *Subscriber) {
		pumps.Add(1)
		go func() {
			defer pumps.Done()
			pumpStreamSession(ctx, sub, writeFrame, cancel)
		}()
	}
	// reconcile brings the subscription set in line with the database and starts
	// a pump for each newly subscribed session. It reports whether anything
	// moved so the caller knows a snapshot needs redrawing.
	reconcile := func() bool {
		added, changed := a.syncStreamSessions(ctx, user.ID, subs)
		for _, sessionID := range added {
			startPump(sessionID, subs[sessionID])
		}
		return changed
	}

	// Subscribe before the first frame so an event emitted while the snapshot
	// is being built waits in the broker queue instead of being lost. Pumps
	// start after stream.ready, which must be the first frame on the wire.
	_, _ = a.syncStreamSessions(ctx, user.ID, subs)

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

	ready, _ := json.Marshal(map[string]any{
		"type":       "stream.ready",
		"serverTime": timestampNow(),
	})
	if err := writeFrame(ready, 10*time.Second); err != nil {
		return
	}
	needsRefresh := false
	if snapshot, err := a.dashboardSnapshot(ctx, user.ID); err == nil {
		if err := writeFrame(snapshot, 15*time.Second); err != nil {
			return
		}
	} else {
		// The snapshot promised right after stream.ready could not be built.
		// Retry on the next refresh tick rather than leaving the client without
		// inventory until some unrelated change happens.
		needsRefresh = true
	}

	// The initial subscriptions were recorded before the snapshot; now that
	// stream.ready and the snapshot are on the wire, start forwarding them.
	for sessionID, sub := range subs {
		startPump(sessionID, sub)
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
			if reconcile() {
				needsRefresh = true
			}
		case <-refresh.C:
			if !needsRefresh {
				continue
			}
			needsRefresh = false
			// Reconcile before redrawing so the snapshot lists the sessions this
			// socket is actually subscribed to, including ones created since the
			// last pass.
			reconcile()
			snapshot, err := a.dashboardSnapshot(ctx, user.ID)
			if err != nil {
				// Keep the redraw pending: a transient database error must not
				// strand the client on a stale snapshot.
				needsRefresh = true
				continue
			}
			if err := writeFrame(snapshot, 15*time.Second); err != nil {
				return
			}
		case <-keepalive.C:
			if !keepaliveDue(time.Unix(0, lastWrite.Load()), time.Now(), streamKeepaliveEvery) {
				continue
			}
			body, _ := json.Marshal(map[string]any{
				"type":       EventKeepalive,
				"serverTime": timestampNow(),
			})
			if err := writeFrame(body, 10*time.Second); err != nil {
				return
			}
		}
	}
}

// syncStreamSessions reconciles subs with the sessions the person owns, active
// and archived. It returns the ids it newly subscribed and whether the set
// changed at all (a removal counts); the caller starts pumps for the added ids
// and redraws the snapshot when anything moved. A listing failure leaves the
// existing set alone rather than tearing down subscriptions that may still be
// correct.
func (a *App) syncStreamSessions(ctx context.Context, userID string, subs map[string]*Subscriber) (added []string, changed bool) {
	sessions, err := a.db.ListAllSessions(ctx, userID, false)
	if err != nil {
		return nil, false
	}
	archived, err := a.db.ListAllSessions(ctx, userID, true)
	if err != nil {
		return nil, false
	}

	wanted := make(map[string]struct{}, len(sessions)+len(archived))
	for _, session := range sessions {
		wanted[session.ID] = struct{}{}
	}
	for _, session := range archived {
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

// pumpStreamSession copies one session's frames to the socket. A failed write
// means the connection is gone, so it cancels the handler instead of leaving
// the other pumps to discover the same failure on their own.
func pumpStreamSession(ctx context.Context, sub *Subscriber, write func([]byte, time.Duration) error, cancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			return
		case frame := <-sub.Frames():
			if err := write(frame, 15*time.Second); err != nil {
				cancel()
				return
			}
		}
	}
}
