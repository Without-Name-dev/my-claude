package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// readStreamFrame reads one text frame and decodes it as a JSON object.
func readStreamFrame(t *testing.T, ws *websocket.Conn, ctx context.Context, timeout time.Duration) map[string]any {
	t.Helper()
	readCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	_, data, err := ws.Read(readCtx)
	if err != nil {
		t.Fatalf("read stream frame: %v", err)
	}
	var frame map[string]any
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatalf("decode stream frame %q: %v", data, err)
	}
	return frame
}

// TestStreamWSReadySnapshotEventsAndKeepalive covers the whole frame contract:
// stream.ready first, the dashboard snapshot second, events for two different
// sessions on the one socket, and a keepalive while otherwise idle. Resync
// ticker behaviour and dashboard refreshes are not exercised here because they
// need either a long wait or inventory changes; the subscription set itself is
// checked by the two events arriving.
func TestStreamWSReadySnapshotEventsAndKeepalive(t *testing.T) {
	// Shorten only the keepalive interval so the idle frame is observable. The
	// restore runs after the server has shut the handler down (defers are LIFO),
	// so no goroutine reads the variable while it is replaced.
	previousKeepalive := streamKeepaliveEvery
	streamKeepaliveEvery = 100 * time.Millisecond
	defer func() { streamKeepaliveEvery = previousKeepalive }()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := New(
		config.Server{DBPath: st.Path(), PublicURL: "http://example.test"},
		st,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	user := createTestUser(t, st, "stream-"+store.NewID("u")+"@example.test", "member")
	userToken := testToken(t, user)
	connectorToken := NewToken("con")
	connector, err := st.CreateConnector(ctx, store.Connector{
		UserID:      user.ID,
		Name:        "stream-test",
		Kind:        "desktop",
		TokenHash:   HashToken(connectorToken),
		TokenPrefix: connectorToken[:12],
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	sessionA, err := st.CreateSession(ctx, store.Session{ConnectorID: connector.ID, Title: "A"})
	if err != nil {
		t.Fatalf("create session A: %v", err)
	}
	sessionB, err := st.CreateSession(ctx, store.Session{ConnectorID: connector.ID, Title: "B"})
	if err != nil {
		t.Fatalf("create session B: %v", err)
	}

	// Use the same ticket request the web client makes for the dashboard socket.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v2/ws-ticket",
		strings.NewReader(`{"clientId":"stream-test","scope":{"dashboard":true}}`))
	if err != nil {
		t.Fatalf("build ticket request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request ticket: %v", err)
	}
	var issued struct {
		Ticket string `json:"ticket"`
	}
	err = json.NewDecoder(resp.Body).Decode(&issued)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || issued.Ticket == "" {
		t.Fatalf("ticket status = %d, err = %v, ticket = %q", resp.StatusCode, err, issued.Ticket)
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v2/stream/ws?ticket=" +
		url.QueryEscape(issued.Ticket)
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	ws, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial stream: %v", err)
	}
	defer ws.CloseNow()

	first := readStreamFrame(t, ws, ctx, 5*time.Second)
	if first["type"] != "stream.ready" {
		t.Fatalf("first frame type = %v, want stream.ready", first["type"])
	}
	if _, ok := first["serverTime"].(string); !ok {
		t.Fatalf("stream.ready has no serverTime: %#v", first)
	}

	second := readStreamFrame(t, ws, ctx, 5*time.Second)
	if second["type"] != "dashboard.snapshot" {
		t.Fatalf("second frame type = %v, want dashboard.snapshot", second["type"])
	}
	for _, key := range []string{"connectors", "projects", "sessions", "runtimes", "sessionPages", "serverTime"} {
		if _, ok := second[key]; !ok {
			t.Fatalf("dashboard.snapshot is missing %q: %#v", key, second)
		}
	}
	if sessions, ok := second["sessions"].([]any); !ok || len(sessions) != 2 {
		t.Fatalf("snapshot sessions = %#v, want the two active sessions", second["sessions"])
	}

	eventA, err := newEnvelope(sessionA.ID, sessionA.Seq+1, EventItemCreated, map[string]any{"which": "a"})
	if err != nil {
		t.Fatalf("build event A: %v", err)
	}
	eventB, err := newEnvelope(sessionB.ID, sessionB.Seq+1, EventItemCreated, map[string]any{"which": "b"})
	if err != nil {
		t.Fatalf("build event B: %v", err)
	}
	app.broker.Publish(sessionA.ID, eventA)
	app.broker.Publish(sessionB.ID, eventB)

	seen := map[string]bool{}
	deadline := time.Now().Add(5 * time.Second)
	for !seen[sessionA.ID] || !seen[sessionB.ID] {
		if time.Now().After(deadline) {
			t.Fatalf("events missing before deadline: %#v", seen)
		}
		frame := readStreamFrame(t, ws, ctx, time.Until(deadline))
		if frame["type"] != EventItemCreated {
			continue
		}
		if frame["protocolVersion"] != "1.0" {
			t.Fatalf("event protocolVersion = %v", frame["protocolVersion"])
		}
		if eventID, _ := frame["eventId"].(string); eventID == "" {
			t.Fatalf("event has no eventId: %#v", frame)
		}
		sessionID, _ := frame["sessionId"].(string)
		if sessionID != sessionA.ID && sessionID != sessionB.ID {
			t.Fatalf("event for unexpected session %q", sessionID)
		}
		seen[sessionID] = true
	}

	// The socket has been silent since the events, so the shortened keepalive
	// must arrive without any further traffic.
	deadline = time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no keepalive arrived")
		}
		frame := readStreamFrame(t, ws, ctx, time.Until(deadline))
		if frame["type"] != EventKeepalive {
			continue
		}
		if _, ok := frame["serverTime"].(string); !ok {
			t.Fatalf("keepalive has no serverTime: %#v", frame)
		}
		break
	}
}

// TestKeepaliveDueOnSchedule pins the cadence rule: a keepalive written just
// after a tick must still produce another one at the next tick. Comparing the
// gap against the whole interval fails this, because the write lands a moment
// after the tick, turning the 15s cadence into 30s.
func TestKeepaliveDueOnSchedule(t *testing.T) {
	const interval = 15 * time.Second
	last := time.Unix(1_700_000_000, 0).Add(time.Millisecond) // written just after a tick
	nextTick := last.Add(interval - time.Millisecond)         // the tick after that one
	if !keepaliveDue(last, nextTick, interval) {
		t.Fatal("keepalive skipped at the tick following a keepalive")
	}
	if keepaliveDue(last, last.Add(interval/4), interval) {
		t.Fatal("keepalive sent while frames were still flowing")
	}
	if !keepaliveDue(last, last.Add(3*interval), interval) {
		t.Fatal("keepalive skipped after a long silence")
	}
}

// TestStreamWSResyncSubscribesNewSessions covers what the initial listing
// cannot: a session created after the socket connected, with no dashboard
// notification, must still get a subscription and must appear in a later
// snapshot. POST /api/v2/sessions does exactly this: it writes the row without
// notifying the dashboard hub.
func TestStreamWSResyncSubscribesNewSessions(t *testing.T) {
	// Shorten the resync ticker so the test does not wait the production 15s.
	previousResync := streamResyncEvery
	streamResyncEvery = 100 * time.Millisecond
	defer func() { streamResyncEvery = previousResync }()

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := New(
		config.Server{DBPath: st.Path(), PublicURL: "http://example.test"},
		st,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	srv := httptest.NewServer(app.Handler())
	defer srv.Close()

	user := createTestUser(t, st, "stream-resync-"+store.NewID("u")+"@example.test", "member")
	userToken := testToken(t, user)
	connectorToken := NewToken("con")
	connector, err := st.CreateConnector(ctx, store.Connector{
		UserID:      user.ID,
		Name:        "stream-resync-test",
		Kind:        "desktop",
		TokenHash:   HashToken(connectorToken),
		TokenPrefix: connectorToken[:12],
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	if _, err := st.CreateSession(ctx, store.Session{ConnectorID: connector.ID, Title: "first"}); err != nil {
		t.Fatalf("create first session: %v", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/api/v2/ws-ticket",
		strings.NewReader(`{"clientId":"stream-resync-test","scope":{"dashboard":true}}`))
	if err != nil {
		t.Fatalf("build ticket request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request ticket: %v", err)
	}
	var issued struct {
		Ticket string `json:"ticket"`
	}
	err = json.NewDecoder(resp.Body).Decode(&issued)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusOK || issued.Ticket == "" {
		t.Fatalf("ticket status = %d, err = %v, ticket = %q", resp.StatusCode, err, issued.Ticket)
	}

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v2/stream/ws?ticket=" +
		url.QueryEscape(issued.Ticket)
	dialCtx, dialCancel := context.WithTimeout(ctx, 5*time.Second)
	defer dialCancel()
	ws, _, err := websocket.Dial(dialCtx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial stream: %v", err)
	}
	defer ws.CloseNow()

	if first := readStreamFrame(t, ws, ctx, 5*time.Second); first["type"] != "stream.ready" {
		t.Fatalf("first frame type = %v, want stream.ready", first["type"])
	}
	initial := readStreamFrame(t, ws, ctx, 5*time.Second)
	if initial["type"] != "dashboard.snapshot" {
		t.Fatalf("second frame type = %v, want dashboard.snapshot", initial["type"])
	}
	if sessions, _ := initial["sessions"].([]any); len(sessions) != 1 {
		t.Fatalf("initial snapshot sessions = %#v, want the one session", initial["sessions"])
	}

	late, err := st.CreateSession(ctx, store.Session{ConnectorID: connector.ID, Title: "late"})
	if err != nil {
		t.Fatalf("create late session: %v", err)
	}

	// The resync ticker notices the new row and marks the dashboard dirty; the
	// throttled refresh tick then sends a snapshot containing it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no refreshed snapshot listing the session created after connect")
		}
		frame := readStreamFrame(t, ws, ctx, time.Until(deadline))
		if frame["type"] != "dashboard.snapshot" {
			continue
		}
		if sessions, _ := frame["sessions"].([]any); len(sessions) == 2 {
			break
		}
	}

	// The refreshed snapshot is only possible after the resync subscribed the
	// new session, so an event published now must reach this socket.
	event, err := newEnvelope(late.ID, late.Seq+1, EventItemCreated, map[string]any{"which": "late"})
	if err != nil {
		t.Fatalf("build late event: %v", err)
	}
	app.broker.Publish(late.ID, event)

	deadline = time.Now().Add(5 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("no event from the session created after connect")
		}
		frame := readStreamFrame(t, ws, ctx, time.Until(deadline))
		if frame["type"] == EventItemCreated && frame["sessionId"] == late.ID {
			return
		}
	}
}
