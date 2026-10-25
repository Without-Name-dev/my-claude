package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"reflect"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

func testHub() *ConnectorHub {
	return NewConnectorHub(slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestConnectorHubTracksLiveSessionsPerSocket(t *testing.T) {
	hub := testHub()
	conn := &connectorConn{id: "con-1", done: make(chan struct{})}
	conn.setBridge(true)
	hub.register(conn)

	if got := hub.SetLiveSessions(conn, []string{"ext-1", "ext-2", ""}); !reflect.DeepEqual(got, []string{"ext-1", "ext-2"}) {
		t.Fatalf("first report changed %v, want [ext-1 ext-2]", got)
	}
	if !hub.Live("con-1", "ext-1") || !hub.Live("con-1", "ext-2") {
		t.Fatal("reported sessions are not live")
	}
	if hub.Live("con-1", "") {
		t.Fatal("an empty external id must not match a report")
	}
	if got := hub.SetLiveSessions(conn, []string{"ext-2"}); !reflect.DeepEqual(got, []string{"ext-1"}) {
		t.Fatalf("removal changed %v, want [ext-1]", got)
	}
	if hub.Live("con-1", "ext-1") {
		t.Fatal("a session missing from the new report is still live")
	}

	// A socket the hub replaced must not overwrite the set the current one owns.
	stale := &connectorConn{id: "con-1", done: make(chan struct{})}
	if got := hub.SetLiveSessions(stale, []string{"ext-9"}); got != nil {
		t.Fatalf("a stale socket changed %v", got)
	}
	if hub.Live("con-1", "ext-9") {
		t.Fatal("a stale socket added a live session")
	}

	if !hub.unregister(conn) {
		t.Fatal("current socket did not unregister")
	}
	if hub.Live("con-1", "ext-2") {
		t.Fatal("presence survived its connector going offline")
	}
}

func TestLiveSessionsShowOnTheSessionViewAndPublishMeta(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	ctx := context.Background()
	session, err := st.CreateSession(ctx, store.Session{
		ConnectorID: connector.ID,
		ExternalID:  "ext-live",
		Title:       "live session",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	conn := &connectorConn{id: connector.ID, userID: user.ID, done: make(chan struct{})}
	conn.setBridge(true)
	app.connectors.register(conn)
	sub := app.broker.Subscribe(session.ID)
	t.Cleanup(func() { app.broker.Unsubscribe(session.ID, sub) })

	app.applyLiveSessions(ctx, conn, []string{"ext-live"})
	if !app.sessionView(ctx, session).Live {
		t.Fatal("session view does not report a live session")
	}
	var frame Envelope
	select {
	case raw := <-sub.Frames():
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("event frame: %v", err)
		}
	default:
		t.Fatal("becoming live published no session.meta.updated")
	}
	if frame.Type != EventSessionMeta {
		t.Fatalf("event type = %q, want %q", frame.Type, EventSessionMeta)
	}
	var payload struct {
		Session SessionView `json:"session"`
	}
	if err := json.Unmarshal(frame.Payload, &payload); err != nil {
		t.Fatalf("event payload: %v", err)
	}
	if !payload.Session.Live {
		t.Fatal("published session view is not live")
	}

	// Repeating the same report changes nothing and must not wake watchers.
	app.applyLiveSessions(ctx, conn, []string{"ext-live"})
	select {
	case raw := <-sub.Frames():
		t.Fatalf("an unchanged presence report published %s", raw)
	default:
	}

	app.applyLiveSessions(ctx, conn, nil)
	if app.sessionView(ctx, session).Live {
		t.Fatal("session view stayed live after the report cleared")
	}
}

func TestConnectorHubUnregisterKeepsTheNewerSocket(t *testing.T) {
	hub := testHub()
	first := &connectorConn{id: "con-1", done: make(chan struct{})}
	second := &connectorConn{id: "con-1", done: make(chan struct{})}
	second.setBridge(true)
	hub.register(first)
	hub.register(second)

	if got := hub.SetLiveSessions(second, []string{"ext-2"}); len(got) != 1 || got[0] != "ext-2" {
		t.Fatalf("new socket changed %v, want [ext-2]", got)
	}
	if hub.unregister(first) {
		t.Fatal("a replaced socket unregistered as current")
	}
	if !hub.Live("con-1", "ext-2") {
		t.Fatal("the replaced socket's cleanup cleared the new socket's presence")
	}
}

func TestConnectorHubLiveRequiresAFreshBridge(t *testing.T) {
	hub := testHub()
	conn := &connectorConn{id: "con-1", done: make(chan struct{})}
	hub.register(conn)
	hub.SetLiveSessions(conn, []string{"ext-1"})

	if hub.Live("con-1", "ext-1") {
		t.Fatal("a session is live without a fresh bridge heartbeat")
	}
	conn.setBridge(true)
	if !hub.Live("con-1", "ext-1") {
		t.Fatal("a live session with a fresh bridge is not live")
	}
	conn.setBridge(false)
	if hub.Live("con-1", "ext-1") {
		t.Fatal("a stale bridge left the session live")
	}
}

func TestConnectorHubRegisterDropsTheReplacedSocketPresence(t *testing.T) {
	hub := testHub()
	first := &connectorConn{id: "con-1", done: make(chan struct{})}
	first.setBridge(true)
	hub.register(first)
	hub.SetLiveSessions(first, []string{"ext-old"})

	second := &connectorConn{id: "con-1", done: make(chan struct{})}
	second.setBridge(true)
	hub.register(second)

	if hub.Live("con-1", "ext-old") {
		t.Fatal("a reconnect kept the previous socket's live sessions")
	}
	if hub.isCurrent(first) || !hub.isCurrent(second) {
		t.Fatal("isCurrent did not follow the replacement")
	}
}

// An older connector answers a ping with {"pong":true}: no bridge flag and no
// presence list. Neither absence may erase what its last real beat reported.
func TestAnOldConnectorsPongDoesNotClearPresence(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	ctx := context.Background()
	if _, err := st.CreateSession(ctx, store.Session{
		ConnectorID: connector.ID,
		ExternalID:  "ext-live",
		Title:       "live",
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}
	conn := &connectorConn{id: connector.ID, userID: user.ID, outbound: make(chan []byte, 2), done: make(chan struct{})}
	conn.setCapabilities(map[string]bool{protocol.CapabilityBridge: true})
	app.connectors.register(conn)
	app.applyLiveSessions(ctx, conn, []string{"ext-live"})
	if !app.connectors.Live(connector.ID, "ext-live") || !conn.bridgeReady() {
		t.Fatal("setup: the session did not become live")
	}

	app.handleConnectorFrame(ctx, conn, protocol.Envelope{
		Type: protocol.TypeBeat,
		Data: json.RawMessage(`{"pong":true}`),
	})
	if !app.connectors.Live(connector.ID, "ext-live") {
		t.Fatal("an old connector's pong cleared its live sessions")
	}
	if !conn.bridgeReady() {
		t.Fatal("an old connector's pong cleared its bridge flag")
	}

	// A connector that does understand the field says "nothing is live" with an
	// explicit null or empty list, and that must take effect.
	app.handleConnectorFrame(ctx, conn, protocol.Envelope{
		Type: protocol.TypeBeat,
		Data: json.RawMessage(`{"bridge":true,"liveSessions":null}`),
	})
	if app.connectors.Live(connector.ID, "ext-live") {
		t.Fatal("an explicit empty presence report left the session live")
	}
}
