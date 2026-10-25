package server

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
)

// keepaliveEvery is how long the socket may stay quiet before the server sends
// a bare keepalive object. Clients treat 45 seconds of silence as dead.
const keepaliveEvery = 15 * time.Second

// handleSessionWS streams a session's events.
//
// The socket carries no initial snapshot: clients fetch
// GET /sessions/{id}/snapshot concurrently and buffer events until it arrives.
// The first frame here is session.subscribed with the cursor to resume from.
func (a *App) handleSessionWS(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("session_id")
	ticketToken := r.URL.Query().Get("ticket")
	if ticketToken == "" {
		closeWith(w, r, 1008, "missing ticket")
		return
	}
	ticket, ok := a.tickets.Consume(ticketToken, TicketScope{SessionID: sessionID})
	if !ok {
		closeWith(w, r, 1008, "invalid ticket")
		return
	}
	session, err := a.db.SessionByID(r.Context(), a.db.Read(), sessionID, ticket.UserID)
	if err != nil {
		closeWith(w, r, 1008, "session not found")
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer ws.CloseNow()
	ws.SetReadLimit(1 << 20)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sub := a.broker.Subscribe(session.ID)
	defer a.broker.Unsubscribe(session.ID, sub)
	// Read the row again, now that the subscription exists. Anything published
	// between the first read and the subscription would otherwise sit below the
	// cursor handed to the client and never be replayed on this socket.
	if current, err := a.db.SessionByID(ctx, a.db.Read(), session.ID, ticket.UserID); err == nil {
		session = current
	}

	// The client must learn the cursor before any event lands, so this frame is
	// written before the pump starts.
	subscribed, err := newEnvelope(session.ID, session.Seq, EventSubscribed, map[string]any{
		"clientId":    ticket.ClientID,
		"eventCursor": Cursor(session.Seq),
	})
	if err != nil {
		return
	}
	writeCtx, writeCancel := context.WithTimeout(ctx, 10*time.Second)
	err = ws.Write(writeCtx, websocket.MessageText, subscribed)
	writeCancel()
	if err != nil {
		return
	}

	// The client sends nothing, but reading is how a disconnect is noticed.
	go func() {
		defer cancel()
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}()

	ticker := time.NewTicker(keepaliveEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-sub.Done():
			return
		case frame := <-sub.Frames():
			deadline, stop := context.WithTimeout(ctx, 15*time.Second)
			err := ws.Write(deadline, websocket.MessageText, frame)
			stop()
			if err != nil {
				return
			}
		case <-ticker.C:
			body, _ := json.Marshal(map[string]any{
				"type":       EventKeepalive,
				"serverTime": timestampNow(),
			})
			deadline, stop := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Write(deadline, websocket.MessageText, body)
			stop()
			if err != nil {
				return
			}
		}
	}
}

// closeWith refuses an upgrade with the code the clients expect.
//
// A refusal happens before the socket exists, so the status is written as a
// plain HTTP response and the client treats it as a failed connection.
func closeWith(w http.ResponseWriter, _ *http.Request, _ int, reason string) {
	httpx.JSON(w, http.StatusForbidden, map[string]string{"detail": reason})
}
