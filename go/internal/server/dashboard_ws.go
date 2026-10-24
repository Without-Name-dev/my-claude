package server

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// dashboardRefresh is how often a dirty dashboard redraws. Streaming a long
// answer marks it dirty many times a second; rebuilding the whole snapshot that
// often would burn CPU for no visible benefit.
const dashboardRefresh = 1200 * time.Millisecond

// dashboardHub tracks one socket per browsing client.
type dashboardHub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func newDashboardHub() *dashboardHub {
	return &dashboardHub{subs: map[string]map[chan struct{}]struct{}{}}
}

func (h *dashboardHub) subscribe(userID string) (chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	set, ok := h.subs[userID]
	if !ok {
		set = map[chan struct{}]struct{}{}
		h.subs[userID] = set
	}
	set[ch] = struct{}{}
	h.mu.Unlock()

	return ch, func() {
		h.mu.Lock()
		if set, ok := h.subs[userID]; ok {
			delete(set, ch)
			if len(set) == 0 {
				delete(h.subs, userID)
			}
		}
		h.mu.Unlock()
	}
}

// notify marks every dashboard of one person as needing a refresh.
func (h *dashboardHub) notify(userID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[userID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// handleDashboardWS pushes the dashboard snapshot and keeps it current.
func (a *App) handleDashboardWS(w http.ResponseWriter, r *http.Request) {
	ticketToken := r.URL.Query().Get("ticket")
	if ticketToken == "" {
		closeWith(w, r, 1008, "missing ticket")
		return
	}
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
	defer cancel()

	dirty, unsubscribe := a.dashboards.subscribe(user.ID)
	defer unsubscribe()

	go func() {
		defer cancel()
		for {
			if _, _, err := ws.Read(ctx); err != nil {
				return
			}
		}
	}()

	if !a.writeDashboard(ws, ctx, user.ID) {
		return
	}

	refresh := time.NewTicker(dashboardRefresh)
	defer refresh.Stop()
	keepalive := time.NewTicker(keepaliveEvery)
	defer keepalive.Stop()
	needsRefresh := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-dirty:
			needsRefresh = true
		case <-refresh.C:
			if needsRefresh {
				needsRefresh = false
				if !a.writeDashboard(ws, ctx, user.ID) {
					return
				}
			}
		case <-keepalive.C:
			body, _ := json.Marshal(map[string]any{
				"type":       EventKeepalive,
				"serverTime": timestampNow(),
			})
			writeCtx, stop := context.WithTimeout(ctx, 10*time.Second)
			err := ws.Write(writeCtx, websocket.MessageText, body)
			stop()
			if err != nil {
				return
			}
		}
	}
}

// writeDashboard sends one full snapshot.
func (a *App) writeDashboard(ws *websocket.Conn, ctx context.Context, userID string) bool {
	connectors, err := a.db.ListConnectors(ctx, userID)
	if err != nil {
		return true
	}
	connectorViews := make([]ConnectorView, 0, len(connectors))
	for _, c := range connectors {
		connectorViews = append(connectorViews, a.connectorView(c))
	}

	projects, err := a.projectViews(ctx, userID)
	if err != nil {
		return true
	}

	sessions, err := a.db.ListAllSessions(ctx, userID, false)
	if err == nil {
		archived, archErr := a.db.ListAllSessions(ctx, userID, true)
		if archErr == nil {
			sessions = append(sessions, archived...)
		}
	}
	sessionViews := make([]SessionView, 0, len(sessions))
	for _, s := range sessions {
		sessionViews = append(sessionViews, a.sessionView(ctx, s))
	}

	payload := map[string]any{
		"type":       "dashboard.snapshot",
		"connectors": connectorViews,
		"projects":   projects,
		"sessions":   sessionViews,
		"runtimes":   []any{},
		"sessionPages": map[string]any{
			"active":   map[string]any{"hasMore": false, "nextCursor": nil},
			"archived": map[string]any{"hasMore": false, "nextCursor": nil},
		},
		"serverTime": timestampNow(),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return true
	}
	writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return ws.Write(writeCtx, websocket.MessageText, body) == nil
}

// notifySessionDashboard refreshes the owner's dashboard after a session
// changed. It resolves the owner from the session rather than threading the
// user through every connector event.
func (a *App) notifySessionDashboard(ctx context.Context, sessionID string) {
	var userID string
	err := a.db.Read().QueryRowContext(ctx, `
		SELECT c.user_id FROM sessions s JOIN connectors c ON c.id = s.connector_id
		WHERE s.id = ?`, sessionID).Scan(&userID)
	if err != nil || userID == "" {
		return
	}
	a.dashboards.notify(userID)
}
