package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

func TestCloseRuntimeAsksTheConnectorAndStopsTheWorkingState(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	ctx := context.Background()
	session, err := st.CreateSession(ctx, store.Session{
		ConnectorID: connector.ID,
		ExternalID:  "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6",
		Title:       "Running",
		CWD:         `C:\work`,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := st.SetSessionStatus(ctx, session.ID, "running"); err != nil {
		t.Fatalf("set status: %v", err)
	}
	conn := registerFakeConnector(app, user, connector)

	rec, body := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/close", testToken(t, user), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if body["ok"] != true {
		t.Fatalf("response = %v, want ok", body)
	}

	env := nextConnectorFrame(t, conn)
	if env.Type != protocol.TypeSessionClose {
		t.Fatalf("frame type = %q, want %q", env.Type, protocol.TypeSessionClose)
	}
	var msg protocol.SessionClose
	if err := json.Unmarshal(env.Data, &msg); err != nil {
		t.Fatalf("decode session.close: %v", err)
	}
	if msg.SessionID != session.ID || msg.ExternalSessionID != session.ExternalID {
		t.Fatalf("session.close = %+v, want both ids", msg)
	}

	stored, err := st.SessionByIDAny(ctx, st.Read(), session.ID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if stored.Status == "running" {
		t.Fatal("the session still reports working after a deliberate close")
	}
}

func TestCloseRuntimeOnAConversationThatNeverRanAnswersWithoutAFrame(t *testing.T) {
	app, _, user, connector, session := newProjectorTestApp(t)
	conn := registerFakeConnector(app, user, connector)

	rec, body := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/close", testToken(t, user), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	result, _ := body["result"].(map[string]any)
	if result["sent"] != false {
		t.Fatalf("result = %v, want sent=false for a conversation with no runtime id", result)
	}
	select {
	case raw := <-conn.outbound:
		t.Fatalf("a frame was sent for a conversation that never ran: %s", raw)
	default:
	}
}

func TestCloseRuntimeRefusesAnOfflineConnector(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	session, err := st.CreateSession(context.Background(), store.Session{
		ConnectorID: connector.ID,
		ExternalID:  "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	rec, _ := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/close", testToken(t, user), nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for an offline connector", rec.Code)
	}
}
