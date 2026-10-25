package server

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// runtimeUUID matches the UUID v4 shape Claude Code accepts for --session-id.
var runtimeUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

// registerFakeConnector makes the hub treat a connector as online without a
// websocket; queued frames wait in outbound for the test to read.
func registerFakeConnector(app *App, user store.User, connector store.Connector) *connectorConn {
	conn := &connectorConn{
		id:       connector.ID,
		userID:   user.ID,
		outbound: make(chan []byte, 16),
		done:     make(chan struct{}),
	}
	conn.setCapabilities(currentConnectorCapabilities())
	app.connectors.register(conn)
	return conn
}

// currentConnectorCapabilities is what a connector built with this package
// advertises: it has a bridge directory and it can start a terminal.
func currentConnectorCapabilities() map[string]bool {
	return map[string]bool{
		protocol.CapabilityBridge:           true,
		protocol.CapabilityBridgeConfigured: true,
		protocol.CapabilitySessionOpen:      true,
	}
}

func nextConnectorFrame(t *testing.T, conn *connectorConn) protocol.Envelope {
	t.Helper()
	select {
	case raw := <-conn.outbound:
		var env protocol.Envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode connector frame: %v", err)
		}
		return env
	case <-time.After(time.Second):
		t.Fatal("the connector received no frame")
		return protocol.Envelope{}
	}
}

func decodeSessionOpen(t *testing.T, env protocol.Envelope) protocol.SessionOpen {
	t.Helper()
	if env.Type != protocol.TypeSessionOpen {
		t.Fatalf("frame type = %q, want %q", env.Type, protocol.TypeSessionOpen)
	}
	var msg protocol.SessionOpen
	if err := json.Unmarshal(env.Data, &msg); err != nil {
		t.Fatalf("decode session.open: %v", err)
	}
	return msg
}

func TestOpenRuntimeSendsSessionOpenAndMintsAnID(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	ctx := context.Background()
	session, err := st.CreateSession(ctx, store.Session{
		ConnectorID: connector.ID,
		Title:       "Open this one",
		CWD:         `C:\work\project`,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conn := registerFakeConnector(app, user, connector)
	token := testToken(t, user)

	rec, body := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/open", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	result, _ := body["result"].(map[string]any)
	if result["sent"] != true {
		t.Fatalf("result.sent = %v, want true", result["sent"])
	}
	if result["resume"] != false {
		t.Fatalf("result.resume = %v, want false for a new conversation", result["resume"])
	}

	open := decodeSessionOpen(t, nextConnectorFrame(t, conn))
	if open.SessionID != session.ID || open.Title != "Open this one" || open.CWD != `C:\work\project` {
		t.Fatalf("session.open = %+v, want session id, title and cwd", open)
	}
	if !runtimeUUID.MatchString(open.ExternalSessionID) {
		t.Fatalf("externalSessionId = %q, want a UUID v4", open.ExternalSessionID)
	}
	if open.Resume {
		t.Fatal("a conversation with no transcript was opened with resume")
	}
	if result["externalSessionId"] != open.ExternalSessionID {
		t.Fatalf("result.externalSessionId = %v, frame has %q", result["externalSessionId"], open.ExternalSessionID)
	}

	stored, err := st.SessionByIDAny(ctx, st.Read(), session.ID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if stored.ExternalID != open.ExternalSessionID {
		t.Fatalf("stored external id = %q, want %q", stored.ExternalID, open.ExternalSessionID)
	}
}

func TestOpenRuntimeResumesAnExistingTranscript(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	ctx := context.Background()
	session, err := st.CreateSession(ctx, store.Session{
		ConnectorID: connector.ID,
		ExternalID:  "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6",
		Title:       "Existing",
		CWD:         "/home/me/repo",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conn := registerFakeConnector(app, user, connector)

	rec, body := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/open", testToken(t, user), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	open := decodeSessionOpen(t, nextConnectorFrame(t, conn))
	if open.ExternalSessionID != session.ExternalID {
		t.Fatalf("externalSessionId = %q, want the stored id", open.ExternalSessionID)
	}
	if !open.Resume {
		t.Fatal("a conversation with a transcript was not resumed")
	}
	result, _ := body["result"].(map[string]any)
	if result["resume"] != true {
		t.Fatalf("result.resume = %v, want true", result["resume"])
	}
}

func TestSendToDeadSessionOpensItBeforeSubmitting(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	ctx := context.Background()
	session, err := st.CreateSession(ctx, store.Session{
		ConnectorID: connector.ID,
		Title:       "Cold start",
		CWD:         `C:\work`,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conn := registerFakeConnector(app, user, connector)

	rec, body := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/messages", testToken(t, user),
		map[string]any{"content": "hello", "clientMessageId": "msg-cold"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if body["ok"] != true {
		t.Fatalf("response = %v, want ok", body)
	}

	open := decodeSessionOpen(t, nextConnectorFrame(t, conn))
	if open.ExternalSessionID == "" {
		t.Fatal("session.open carried no runtime id")
	}

	env := nextConnectorFrame(t, conn)
	if env.Type != protocol.TypeSubmit {
		t.Fatalf("second frame = %q, want %q (open must come first)", env.Type, protocol.TypeSubmit)
	}
	var submit protocol.Submit
	if err := json.Unmarshal(env.Data, &submit); err != nil {
		t.Fatalf("decode submit: %v", err)
	}
	if submit.ExternalSessionID != open.ExternalSessionID {
		t.Fatalf("submit external = %q, want the opened id %q", submit.ExternalSessionID, open.ExternalSessionID)
	}
	if submit.Text != "hello" || submit.RequestID != "msg-cold" {
		t.Fatalf("submit = %+v, want the queued message", submit)
	}

	stored, err := st.SessionByIDAny(ctx, st.Read(), session.ID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if stored.ExternalID != open.ExternalSessionID {
		t.Fatalf("stored external id = %q, want %q", stored.ExternalID, open.ExternalSessionID)
	}
}

func TestSendToDeadSessionReportsWhenOpenCannotBeSent(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	ctx := context.Background()
	session, err := st.CreateSession(ctx, store.Session{
		ConnectorID: connector.ID,
		Title:       "No room",
		CWD:         "/tmp",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	// No reader and no buffer: session.open cannot be queued.
	conn := &connectorConn{
		id:       connector.ID,
		userID:   user.ID,
		outbound: make(chan []byte),
		done:     make(chan struct{}),
	}
	conn.setCapabilities(currentConnectorCapabilities())
	app.connectors.register(conn)

	rec, body := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/messages", testToken(t, user),
		map[string]any{"content": "hello"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	detail, _ := body["detail"].(map[string]any)
	if detail["code"] != sessionNotLiveCode {
		t.Fatalf("detail = %v, want code %q", body["detail"], sessionNotLiveCode)
	}
}

func TestOpenRuntimeRequiresAnOnlineConnector(t *testing.T) {
	app, _, user, _, session := newProjectorTestApp(t)
	rec, body := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/open", testToken(t, user), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if body["detail"] != "connector is offline" {
		t.Fatalf("detail = %v, want connector is offline", body["detail"])
	}
}

func TestOpenRuntimeOnlyWorksForTheOwner(t *testing.T) {
	app, st, _, _, session := newProjectorTestApp(t)
	other := createTestUser(t, st, "other-"+store.NewID("u")+"@example.test", "member")
	rec, _ := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/open", testToken(t, other), nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another user's session", rec.Code)
	}
}

func TestOpenRuntimeRefusesAConnectorThatCannotSpawn(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	session, err := st.CreateSession(context.Background(), store.Session{
		ConnectorID: connector.ID,
		Title:       "Old build",
		CWD:         `C:\work`,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conn := &connectorConn{
		id:       connector.ID,
		userID:   user.ID,
		outbound: make(chan []byte, 4),
		done:     make(chan struct{}),
	}
	// An older connector knows the bridge but not session.open.
	conn.setCapabilities(map[string]bool{protocol.CapabilityBridge: true})
	app.connectors.register(conn)

	rec, _ := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/open", testToken(t, user), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a connector that cannot spawn (body %s)", rec.Code, rec.Body.String())
	}
	select {
	case raw := <-conn.outbound:
		t.Fatalf("an old connector received a session.open frame: %s", raw)
	default:
	}
}

func TestSendToAnOldConnectorUsesTheLivePluginOnly(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	session, err := st.CreateSession(context.Background(), store.Session{
		ConnectorID: connector.ID,
		ExternalID:  "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6",
		Title:       "Old but live",
		CWD:         `C:\work`,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conn := &connectorConn{
		id:       connector.ID,
		userID:   user.ID,
		outbound: make(chan []byte, 8),
		done:     make(chan struct{}),
	}
	conn.setCapabilities(map[string]bool{protocol.CapabilityBridge: true})
	app.connectors.register(conn)

	rec, _ := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/messages", testToken(t, user),
		map[string]any{"content": "hello", "clientMessageId": "msg-old"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if env := nextConnectorFrame(t, conn); env.Type != protocol.TypeSubmit {
		t.Fatalf("first frame = %q, want %q: an old connector must not be sent session.open", env.Type, protocol.TypeSubmit)
	}
	select {
	case raw := <-conn.outbound:
		t.Fatalf("an old connector received a second frame: %s", raw)
	default:
	}
}

func TestSendToAConnectorWithoutABridgeIsNotPromised(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	session, err := st.CreateSession(context.Background(), store.Session{
		ConnectorID: connector.ID,
		Title:       "No bridge",
		CWD:         `C:\work`,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conn := &connectorConn{
		id:       connector.ID,
		userID:   user.ID,
		outbound: make(chan []byte, 8),
		done:     make(chan struct{}),
	}
	// It can spawn a terminal but has no bridge directory for the message to
	// land in, so a send must not be accepted and then silently dropped.
	conn.setCapabilities(map[string]bool{
		protocol.CapabilityBridge:      true,
		protocol.CapabilitySessionOpen: true,
	})
	app.connectors.register(conn)

	rec, body := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/messages", testToken(t, user),
		map[string]any{"content": "hello"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	detail, _ := body["detail"].(map[string]any)
	if detail["code"] != sessionNotLiveCode {
		t.Fatalf("detail = %v, want code %q", body["detail"], sessionNotLiveCode)
	}
	select {
	case raw := <-conn.outbound:
		t.Fatalf("a message with nowhere to run was still queued: %s", raw)
	default:
	}
}

func TestARetriedMessageDoesNotStartAnotherTerminal(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	session, err := st.CreateSession(context.Background(), store.Session{
		ConnectorID: connector.ID,
		Title:       "Cold start",
		CWD:         `C:\work`,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	conn := registerFakeConnector(app, user, connector)

	for _, id := range []string{"msg-first", "msg-retry"} {
		rec, _ := adminRequest(t, app.Handler(), http.MethodPost,
			"/api/v2/sessions/"+session.ID+"/runtime/messages", testToken(t, user),
			map[string]any{"content": "hello", "clientMessageId": id})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200 (body %s)", id, rec.Code, rec.Body.String())
		}
	}

	open := decodeSessionOpen(t, nextConnectorFrame(t, conn))
	for _, want := range []string{"msg-first", "msg-retry"} {
		env := nextConnectorFrame(t, conn)
		if env.Type != protocol.TypeSubmit {
			t.Fatalf("frame after the first open = %q, want a submit", env.Type)
		}
		var submit protocol.Submit
		if err := json.Unmarshal(env.Data, &submit); err != nil {
			t.Fatalf("decode submit: %v", err)
		}
		if submit.RequestID != want {
			t.Fatalf("submit request = %q, want %q", submit.RequestID, want)
		}
		if submit.ExternalSessionID != open.ExternalSessionID {
			t.Fatalf("submit external = %q, want %q", submit.ExternalSessionID, open.ExternalSessionID)
		}
	}
	select {
	case raw := <-conn.outbound:
		t.Fatalf("a retry asked for a second terminal: %s", raw)
	default:
	}
}
