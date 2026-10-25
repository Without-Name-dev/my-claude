package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// saveTestAttachment puts one file in the session's attachment store and
// returns its metadata.
func saveTestAttachment(t *testing.T, app *App, sessionID, name string, data []byte) store.AttachmentMeta {
	t.Helper()
	meta := store.AttachmentMeta{
		FileID:    store.NewID("file"),
		SessionID: sessionID,
		Name:      name,
		MediaType: "image/png",
	}
	if err := store.SaveAttachment(app.cfg.UploadDir, &meta, data); err != nil {
		t.Fatalf("save attachment: %v", err)
	}
	return meta
}

// nextSubmit reads frames until the submit arrives, skipping the session.open
// that a conversation with no live plugin produces first.
func nextSubmit(t *testing.T, conn *connectorConn) (protocol.SessionOpen, protocol.Submit) {
	t.Helper()
	var open protocol.SessionOpen
	for {
		env := nextConnectorFrame(t, conn)
		switch env.Type {
		case protocol.TypeSessionOpen:
			if err := json.Unmarshal(env.Data, &open); err != nil {
				t.Fatalf("decode session.open: %v", err)
			}
		case protocol.TypeSubmit:
			var submit protocol.Submit
			if err := json.Unmarshal(env.Data, &submit); err != nil {
				t.Fatalf("decode submit: %v", err)
			}
			return open, submit
		}
	}
}

func TestSendMessageForwardsAttachmentsToTheConnector(t *testing.T) {
	app, st, user, connector, session := newProjectorTestApp(t)
	app.cfg.UploadDir = t.TempDir()
	conn := registerFakeConnector(app, user, connector)

	meta := saveTestAttachment(t, app, session.ID, "shot.png", []byte("png bytes"))

	rec, body := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/messages", testToken(t, user),
		map[string]any{
			"content":         "what is in this picture?",
			"clientMessageId": "msg-image",
			"attachments":     []map[string]any{{"fileId": meta.FileID}},
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if body["ok"] != true {
		t.Fatalf("response = %v, want ok", body)
	}

	_, submit := nextSubmit(t, conn)
	if len(submit.Attachments) != 1 {
		t.Fatalf("submit attachments = %+v, want one", submit.Attachments)
	}
	got := submit.Attachments[0]
	if got.FileID != meta.FileID || got.Name != "shot.png" || got.MediaType != "image/png" {
		t.Fatalf("attachment = %+v, want the stored metadata", got)
	}
	wantURL := "/api/v2/connector/sessions/" + session.ID + "/attachments/" + meta.FileID + "/content"
	if got.DownloadURL != wantURL {
		t.Fatalf("downloadUrl = %q, want %q", got.DownloadURL, wantURL)
	}
	if submit.Text != "what is in this picture?" {
		t.Fatalf("text = %q, want the person's words untouched", submit.Text)
	}

	// The bubble has to keep drawing the image after the optimistic copy is
	// replaced by the stored item.
	items, err := st.TimelineItems(context.Background(), st.Read(), session.ID, 50)
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	var payload struct {
		Attachments []struct {
			FileID    string `json:"fileId"`
			Name      string `json:"name"`
			MediaType string `json:"mediaType"`
		} `json:"attachments"`
	}
	found := false
	for _, item := range items {
		if item.ID != "msg_msg-image_user" {
			continue
		}
		found = true
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
	}
	if !found {
		t.Fatal("the user's message was not stored on the timeline")
	}
	if len(payload.Attachments) != 1 || payload.Attachments[0].FileID != meta.FileID {
		t.Fatalf("stored attachments = %+v, want the uploaded file", payload.Attachments)
	}
}

func TestSendMessageRefusesAnAttachmentItNeverStored(t *testing.T) {
	app, st, user, connector, session := newProjectorTestApp(t)
	app.cfg.UploadDir = t.TempDir()
	conn := registerFakeConnector(app, user, connector)

	rec, _ := adminRequest(t, app.Handler(), http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/runtime/messages", testToken(t, user),
		map[string]any{
			"content":     "look",
			"attachments": []map[string]any{{"fileId": "file_missing"}},
		})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	select {
	case raw := <-conn.outbound:
		t.Fatalf("a refused message still reached the connector: %s", raw)
	default:
	}
	items, err := st.TimelineItems(context.Background(), st.Read(), session.ID, 50)
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("stored %d items for a refused message, want none", len(items))
	}
}

func TestSessionCapabilitiesAdvertiseAttachments(t *testing.T) {
	app, _, user, connector, session := newProjectorTestApp(t)
	registerFakeConnector(app, user, connector)

	rec, body := adminRequest(t, app.Handler(), http.MethodGet,
		"/api/v2/sessions/"+session.ID+"/runtime/capabilities", testToken(t, user), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	set, _ := body["capabilitySet"].(map[string]any)
	capabilities, _ := set["capabilities"].([]any)
	for _, entry := range capabilities {
		capability, _ := entry.(map[string]any)
		if capability["capabilityId"] != "runtime.attachment" {
			continue
		}
		if capability["supported"] != true || capability["allowed"] != true {
			t.Fatalf("attachment capability = %v, want supported and allowed", capability)
		}
		return
	}
	t.Fatal("the session does not advertise runtime.attachment, so the composer hides the picker")
}

func TestConnectorAttachmentRouteServesOnlyItsOwnSessions(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	app.cfg.UploadDir = t.TempDir()
	_ = registerFakeConnector(app, user, connector)

	connectorToken := NewToken("con")
	other, err := st.CreateConnector(context.Background(), store.Connector{
		UserID:      user.ID,
		Name:        "owning-connector",
		Kind:        "desktop",
		TokenHash:   HashToken(connectorToken),
		TokenPrefix: connectorToken[:12],
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	session, err := st.CreateSession(context.Background(), store.Session{
		ConnectorID: other.ID,
		Title:       "with a file",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	meta := saveTestAttachment(t, app, session.ID, "shot.png", []byte("png bytes"))

	rec, _ := adminRequest(t, app.Handler(), http.MethodGet,
		"/api/v2/connector/sessions/"+session.ID+"/attachments/"+meta.FileID+"/content",
		connectorToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if rec.Body.String() != "png bytes" {
		t.Fatalf("body = %q, want the stored bytes", rec.Body.String())
	}

	// A connector must not read another connector's session.
	stranger := NewToken("con")
	if _, err := st.CreateConnector(context.Background(), store.Connector{
		UserID:      user.ID,
		Name:        "stranger",
		Kind:        "desktop",
		TokenHash:   HashToken(stranger),
		TokenPrefix: stranger[:12],
	}); err != nil {
		t.Fatalf("create stranger: %v", err)
	}
	rec, _ = adminRequest(t, app.Handler(), http.MethodGet,
		"/api/v2/connector/sessions/"+session.ID+"/attachments/"+meta.FileID+"/content",
		stranger, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for another connector's session", rec.Code)
	}
}
