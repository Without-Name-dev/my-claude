package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/auth"
	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// terminalClientFrame is one browser → server message on a terminal stream.
type terminalClientFrame struct {
	Type string `json:"type"`
	Data string `json:"data"`
	Cols int    `json:"cols"`
	Rows int    `json:"rows"`
}

// handleTerminalStream attaches a viewer to a running terminal.
//
// The socket accepts a single-use ticket (the server's preferred mechanism)
// or a plain user access token in ?token=, which is what the existing web
// panel sends. Reading and writing happen on separate goroutines; only the
// main goroutine writes, because coder/websocket permits one writer.
func (a *App) handleTerminalStream(w http.ResponseWriter, r *http.Request) {
	connectorID := r.PathValue("connector_id")
	terminalID := r.PathValue("terminal_id")

	user, ok := a.terminalStreamUser(r, connectorID, terminalID)
	if !ok {
		httpx.Fail(w, httpx.Unauthorized("sign in required"))
		return
	}
	terminal, err := a.db.TerminalByID(r.Context(), terminalID)
	if err != nil || terminal.UserID != user.ID || terminal.ConnectorID != connectorID {
		httpx.Fail(w, httpx.NotFound("terminal not found"))
		return
	}
	connector, err := a.db.ConnectorByID(r.Context(), connectorID)
	if err != nil || connector.UserID != user.ID {
		httpx.Fail(w, httpx.NotFound("connector not found"))
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

	hub := a.terminalHub()
	hub.bind(terminal.ID, connector.ID)

	// A server restart wipes the in-memory replay, and a connector restart
	// wipes the PTY while leaving the row marked running. Before attaching,
	// ask the connector whether the terminal still exists. A live terminal
	// answers with a bounded scrollback snapshot, which refills the replay; a
	// dead one is reported explicitly instead of leaving a blank screen.
	probe := a.probeTerminal(ctx, hub, connector.ID, terminal)

	afterSeq := parseTerminalAfterSeq(r)
	sub, replay, exit := hub.subscribe(terminal.ID, afterSeq)
	defer hub.unsubscribe(terminal.ID, sub)
	// After a server restart the in-memory exit state is gone, but the stored
	// row still knows the terminal is dead; a reconnecting viewer must be
	// told rather than left waiting.
	if !exit.Exited && terminal.Status == "exited" {
		exit = terminalExitState{Exited: true, ExitCode: terminal.ExitCode, Reason: "exit"}
	}

	if !writeTerminalFrame(ctx, ws, terminalReplayFrame(replay)) {
		return
	}
	switch {
	case probe == terminalProbeMissing || probe == terminalProbeTimeout ||
		(exit.Exited && exit.Reason == "lost"):
		if !writeTerminalFrame(ctx, ws, terminalErrorFrame(terminalUnavailableCode, terminalUnavailableMessage)) {
			return
		}
	case probe == terminalProbeOffline:
		if !writeTerminalFrame(ctx, ws, terminalErrorFrame("connector_offline", "连接器离线，无法连接终端")) {
			return
		}
	}
	if exit.Exited {
		if !writeTerminalFrame(ctx, ws, terminalExitFrame(exit.ExitCode, exit.Reason)) {
			return
		}
	}

	pong := make(chan struct{}, 1)
	clientErrors := make(chan []byte, 1)

	go func() {
		defer cancel()
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				return
			}
			var msg terminalClientFrame
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			switch msg.Type {
			case "input":
				// The connector decodes the base64; validating here means an
				// invalid paste never leaves the server.
				if msg.Data == "" || len(msg.Data) > 4<<20 {
					continue
				}
				if _, err := base64.StdEncoding.DecodeString(msg.Data); err != nil {
					continue
				}
				if !a.connectors.Send(connector.ID, protocol.TypeTerminal, protocol.TerminalCommand{
					Op:         protocol.TerminalOpInput,
					TerminalID: terminal.ID,
					Data:       msg.Data,
				}) {
					select {
					case clientErrors <- terminalErrorFrame(409, "connector is offline"):
					default:
					}
				}
			case "resize":
				cols, rows := clampTerminalDimensions(msg.Cols, msg.Rows)
				_ = a.db.UpdateTerminalSize(ctx, terminal.ID, cols, rows)
				if !a.connectors.Send(connector.ID, protocol.TypeTerminal, protocol.TerminalCommand{
					Op:         protocol.TerminalOpResize,
					TerminalID: terminal.ID,
					Cols:       cols,
					Rows:       rows,
				}) {
					select {
					case clientErrors <- terminalErrorFrame(409, "connector is offline"):
					default:
					}
				}
			case "ping":
				select {
				case pong <- struct{}{}:
				default:
				}
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			a.log.Debug("terminal stream closed",
				"terminal", terminal.ID,
				"dropped_frames", sub.dropped.Load())
			return
		case frame, open := <-sub.ch:
			if !open {
				return
			}
			if !writeTerminalFrame(ctx, ws, frame) {
				return
			}
		case <-pong:
			if !writeTerminalFrame(ctx, ws, []byte(`{"type":"pong"}`)) {
				return
			}
		case frame := <-clientErrors:
			if !writeTerminalFrame(ctx, ws, frame) {
				return
			}
		}
	}
}

// terminalProbe is what the attach-time liveness check learned.
type terminalProbe int

const (
	terminalProbeSkipped terminalProbe = iota
	terminalProbeAlive
	terminalProbeMissing
	terminalProbeOffline
	terminalProbeTimeout
)

// probeTerminal asks the connector whether a terminal still exists and takes a
// scrollback snapshot in the same round trip. It runs on every attach: after a
// server restart the in-memory replay is gone, and after a connector restart
// the row still says "running" even though the PTY is dead. Both cases would
// otherwise leave the viewer staring at a blank screen. The snapshot is
// ignored when the server already holds newer or equal output.
func (a *App) probeTerminal(
	ctx context.Context, hub *terminalHub, connectorID string, terminal store.Terminal,
) terminalProbe {
	if terminal.Status == "exited" {
		return terminalProbeSkipped
	}
	if !a.connectors.Online(connectorID) {
		return terminalProbeOffline
	}

	reply := hub.expectState(terminal.ID)
	defer hub.forgetState(terminal.ID, reply)
	if !a.connectors.Send(connectorID, protocol.TypeTerminal, protocol.TerminalCommand{
		Op:         protocol.TerminalOpProbe,
		TerminalID: terminal.ID,
	}) {
		return terminalProbeOffline
	}
	select {
	case state := <-reply:
		if state.Running {
			return terminalProbeAlive
		}
		return terminalProbeMissing
	case <-time.After(terminalProbeWait):
		return terminalProbeTimeout
	case <-ctx.Done():
		return terminalProbeTimeout
	}
}

// terminalStreamUser resolves the viewer behind a terminal socket.
func (a *App) terminalStreamUser(
	r *http.Request, connectorID, terminalID string,
) (store.User, bool) {
	if ticketToken := r.URL.Query().Get("ticket"); ticketToken != "" {
		ticket, ok := a.tickets.Consume(ticketToken, TicketScope{
			ConnectorID: connectorID,
			TerminalID:  terminalID,
		})
		if !ok {
			return store.User{}, false
		}
		user, err := a.db.UserByID(r.Context(), ticket.UserID)
		if err != nil || user.Disabled {
			return store.User{}, false
		}
		return user, true
	}

	token := r.URL.Query().Get("token")
	if token == "" {
		token = bearerToken(r)
	}
	if token == "" {
		return store.User{}, false
	}
	userID, err := auth.VerifyUserAccessToken(token)
	if err != nil {
		return store.User{}, false
	}
	user, err := a.db.UserByID(r.Context(), userID)
	if err != nil || user.Disabled {
		return store.User{}, false
	}
	return user, true
}

// parseTerminalAfterSeq reads the resume cursor. afterSeq is the go-spec
// name; fromSeq is what the web panel sends today.
func parseTerminalAfterSeq(r *http.Request) uint64 {
	raw := r.URL.Query().Get("afterSeq")
	if raw == "" {
		raw = r.URL.Query().Get("fromSeq")
	}
	if raw == "" {
		return 0
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0
	}
	return value
}

// writeTerminalFrame writes one message with a deadline.
func writeTerminalFrame(ctx context.Context, ws *websocket.Conn, frame []byte) bool {
	writeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return ws.Write(writeCtx, websocket.MessageText, frame) == nil
}

func terminalReplayFrame(replay terminalReplay) []byte {
	body, _ := json.Marshal(map[string]any{
		"type": "replay",
		"data": base64.StdEncoding.EncodeToString(replay.Data),
		"seq":  replay.Seq,
	})
	return body
}

func terminalExitFrame(code *int, reason string) []byte {
	body, _ := json.Marshal(map[string]any{
		"type":     "exit",
		"exitCode": code,
		"reason":   reason,
	})
	return body
}

func terminalErrorFrame(code any, message string) []byte {
	body, _ := json.Marshal(map[string]any{
		"type":    "error",
		"code":    code,
		"message": message,
	})
	return body
}
