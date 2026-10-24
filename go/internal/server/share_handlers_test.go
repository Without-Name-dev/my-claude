package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// shareTestHandler mirrors what app.go is expected to do: mount the two new
// registration functions before the catch-all app handler.
func shareTestHandler(app *App) http.Handler {
	mux := http.NewServeMux()
	app.registerShareRoutes(mux)
	app.registerAdminUserEditRoutes(mux)
	mux.Handle("/", app.Handler())
	return mux
}

func createTestConnector(t *testing.T, st *store.Store, userID, name string) store.Connector {
	t.Helper()
	connector, err := st.CreateConnector(context.Background(), store.Connector{
		UserID: userID, Name: name, TokenHash: "hash-" + name, TokenPrefix: "pfx",
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	return connector
}

func createTestSession(t *testing.T, st *store.Store, connectorID, title, cwd string) store.Session {
	t.Helper()
	session, err := st.CreateSession(context.Background(), store.Session{
		ConnectorID: connectorID,
		Runtime:     "claude",
		RuntimeID:   "claude",
		Title:       title,
		CWD:         cwd,
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return session
}

func TestSessionShareCreateListPublicReadAndDelete(t *testing.T) {
	app, st := newAdminTestApp(t)
	owner := createTestUser(t, st, "owner@example.test", "member")
	other := createTestUser(t, st, "other@example.test", "member")
	connector := createTestConnector(t, st, owner.ID, "owner-dev")
	session := createTestSession(t, st, connector.ID, "Shared demo", "/tmp/demo")

	now := store.NowMS()
	if err := st.Write(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		items := []store.TimelineItem{
			{SessionID: session.ID, ID: "msg_user", Type: "message", Status: "done",
				Role: "user", OrderSeq: now, UpdatedSeq: 1,
				Payload: json.RawMessage(`{"text":"question"}`)},
			{SessionID: session.ID, ID: "msg_assistant", Type: "message", Status: "done",
				Role: "assistant", OrderSeq: now + 1, UpdatedSeq: 1,
				Payload: json.RawMessage(`{"text":"answer"}`)},
			{SessionID: session.ID, ID: "tool_one", Type: "tool", Status: "done",
				Role: "tool", OrderSeq: now + 2, UpdatedSeq: 1,
				Payload: json.RawMessage(`{"name":"read"}`)},
		}
		for _, item := range items {
			if err := store.UpsertTimelineItem(ctx, tx, item, now); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write timeline: %v", err)
	}

	handler := shareTestHandler(app)
	ownerToken := testToken(t, owner)
	otherToken := testToken(t, other)

	rec, created := adminRequest(t, handler, http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/shares", ownerToken, map[string]any{"scope": "message"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", rec.Code, rec.Body.String())
	}
	shareID, _ := created["shareId"].(string)
	if shareID == "" {
		t.Fatalf("shareId missing: %v", created)
	}
	if created["scope"] != "message" {
		t.Fatalf("scope = %v, want message", created["scope"])
	}
	if created["sharePath"] != "/share/"+shareID {
		t.Fatalf("sharePath = %v, want /share/%s", created["sharePath"], shareID)
	}
	if created["shareUrl"] != "http://example.test/share/"+shareID {
		t.Fatalf("shareUrl = %v, want absolute AA_PUBLIC_URL link", created["shareUrl"])
	}
	if created["createdAt"] == "" {
		t.Fatalf("createdAt missing: %v", created)
	}

	// The attachment-free public read must work without any bearer.
	rec, public := adminRequest(t, handler, http.MethodGet, "/api/v2/shares/"+shareID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public status = %d, body %s", rec.Code, rec.Body.String())
	}
	if public["shareId"] != shareID || public["scope"] != "message" {
		t.Fatalf("unexpected public response: %v", public)
	}
	sharedSession := public["session"].(map[string]any)
	if sharedSession["id"] != session.ID || sharedSession["title"] != "Shared demo" ||
		sharedSession["runtime"] != "claude" || sharedSession["cwd"] != "/tmp/demo" {
		t.Fatalf("unexpected shared session: %v", sharedSession)
	}
	items := public["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("message share items = %d, want only the assistant message", len(items))
	}
	item := items[0].(map[string]any)
	if item["id"] != "msg_assistant" || item["role"] != "assistant" {
		t.Fatalf("unexpected shared item: %v", item)
	}

	// The specification path must answer as well, for the shipped web client.
	rec, _ = adminRequest(t, handler, http.MethodGet, "/api/v2/public/shares/"+shareID, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("spec public path status = %d, body %s", rec.Code, rec.Body.String())
	}

	rec, list := adminRequest(t, handler, http.MethodGet,
		"/api/v2/sessions/"+session.ID+"/shares", ownerToken, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := len(list["shares"].([]any)); got != 1 {
		t.Fatalf("shares = %d, want 1", got)
	}

	// A different account must not see or revoke the link.
	rec, _ = adminRequest(t, handler, http.MethodGet,
		"/api/v2/sessions/"+session.ID+"/shares", otherToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner list status = %d, want 404", rec.Code)
	}
	rec, _ = adminRequest(t, handler, http.MethodDelete,
		"/api/v2/sessions/"+session.ID+"/shares/"+shareID, otherToken, nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner delete status = %d, want 404", rec.Code)
	}

	rec, _ = adminRequest(t, handler, http.MethodDelete,
		"/api/v2/sessions/"+session.ID+"/shares/"+shareID, ownerToken, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("owner delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	rec, _ = adminRequest(t, handler, http.MethodGet, "/api/v2/shares/"+shareID, "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("deleted share status = %d, want 404", rec.Code)
	}

	// A session share freezes the whole timeline.
	rec, sessionShare := adminRequest(t, handler, http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/shares", ownerToken, map[string]any{"scope": "session"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("session share status = %d, body %s", rec.Code, rec.Body.String())
	}
	rec, public = adminRequest(t, handler, http.MethodGet,
		"/api/v2/shares/"+sessionShare["shareId"].(string), "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("session share read status = %d", rec.Code)
	}
	if got := len(public["items"].([]any)); got != 3 {
		t.Fatalf("session share items = %d, want 3", got)
	}

	// Scope and item validation.
	rec, _ = adminRequest(t, handler, http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/shares", ownerToken, map[string]any{"scope": "nope"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad scope status = %d, want 422", rec.Code)
	}
	rec, _ = adminRequest(t, handler, http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/shares", ownerToken,
		map[string]any{"scope": "message", "itemIds": []string{"msg_user"}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("non-assistant item status = %d, want 422", rec.Code)
	}
	rec, _ = adminRequest(t, handler, http.MethodPost,
		"/api/v2/sessions/"+session.ID+"/shares", ownerToken,
		map[string]any{"scope": "message", "itemIds": []string{"missing"}})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing item status = %d, want 404", rec.Code)
	}

	// A brand-new conversation has no assistant reply yet; the scope-only
	// contract must still produce a link.
	empty := createTestSession(t, st, connector.ID, "Empty", "")
	rec, created = adminRequest(t, handler, http.MethodPost,
		"/api/v2/sessions/"+empty.ID+"/shares", ownerToken, map[string]any{"scope": "message"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("empty session share status = %d, body %s", rec.Code, rec.Body.String())
	}
	rec, public = adminRequest(t, handler, http.MethodGet,
		"/api/v2/shares/"+created["shareId"].(string), "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty session share read status = %d", rec.Code)
	}
	if got := len(public["items"].([]any)); got != 0 {
		t.Fatalf("empty share items = %d, want 0", got)
	}
}

func TestArchiveAllConnectorAndAllDevices(t *testing.T) {
	app, st := newAdminTestApp(t)
	owner := createTestUser(t, st, "owner@example.test", "member")
	other := createTestUser(t, st, "other@example.test", "member")
	connectorA := createTestConnector(t, st, owner.ID, "device-a")
	connectorB := createTestConnector(t, st, owner.ID, "device-b")
	a1 := createTestSession(t, st, connectorA.ID, "a1", "")
	a2 := createTestSession(t, st, connectorA.ID, "a2", "")
	a3 := createTestSession(t, st, connectorA.ID, "a3", "")
	b1 := createTestSession(t, st, connectorB.ID, "b1", "")
	if err := st.SetSessionArchived(context.Background(), a3.ID, owner.ID, true); err != nil {
		t.Fatalf("pre-archive a3: %v", err)
	}

	handler := shareTestHandler(app)
	ownerToken := testToken(t, owner)
	otherToken := testToken(t, other)

	rec, body := adminRequest(t, handler, http.MethodPost,
		"/api/v2/connectors/"+connectorA.ID+"/sessions/archive-all", ownerToken,
		map[string]any{"archived": true, "scope": "active"})
	if rec.Code != http.StatusOK {
		t.Fatalf("connector archive status = %d, body %s", rec.Code, rec.Body.String())
	}
	assertNumber(t, body, "affected", 2)
	if got := len(body["sessions"].([]any)); got != 2 {
		t.Fatalf("connector archive sessions = %d, want 2", got)
	}
	for _, raw := range body["sessions"].([]any) {
		if raw.(map[string]any)["archived"] != true {
			t.Fatalf("session not archived: %v", raw)
		}
	}
	archived, err := st.SessionByIDAny(context.Background(), st.Read(), a1.ID)
	if err != nil || !archived.Archived {
		t.Fatalf("a1 not archived: %v %v", archived.Archived, err)
	}
	stillActive, err := st.SessionByIDAny(context.Background(), st.Read(), b1.ID)
	if err != nil || stillActive.Archived {
		t.Fatalf("b1 should still be active: %v %v", stillActive.Archived, err)
	}

	// Scope is validated and archived is required.
	rec, _ = adminRequest(t, handler, http.MethodPost,
		"/api/v2/connectors/"+connectorA.ID+"/sessions/archive-all", ownerToken,
		map[string]any{"archived": true, "scope": "everything"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad scope status = %d, want 422", rec.Code)
	}
	rec, _ = adminRequest(t, handler, http.MethodPost,
		"/api/v2/connectors/"+connectorA.ID+"/sessions/archive-all", ownerToken, map[string]any{})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("missing archived status = %d, want 422", rec.Code)
	}

	// Cross-device restore of every archived session.
	rec, body = adminRequest(t, handler, http.MethodPost,
		"/api/v2/sessions/archive-all", ownerToken,
		map[string]any{"archived": false, "scope": "archived"})
	if rec.Code != http.StatusOK {
		t.Fatalf("global unarchive status = %d, body %s", rec.Code, rec.Body.String())
	}
	assertNumber(t, body, "affected", 3) // a1, a2 and the pre-archived a3
	for _, id := range []string{a1.ID, a2.ID, a3.ID} {
		session, err := st.SessionByIDAny(context.Background(), st.Read(), id)
		if err != nil || session.Archived {
			t.Fatalf("%s still archived: %v %v", id, session.Archived, err)
		}
	}

	rec, body = adminRequest(t, handler, http.MethodPost,
		"/api/v2/sessions/archive-all", ownerToken,
		map[string]any{"archived": true, "scope": "all"})
	if rec.Code != http.StatusOK {
		t.Fatalf("global archive all status = %d, body %s", rec.Code, rec.Body.String())
	}
	assertNumber(t, body, "affected", 4)

	// Somebody else's connector is indistinguishable from a missing one.
	rec, _ = adminRequest(t, handler, http.MethodPost,
		"/api/v2/connectors/"+connectorA.ID+"/sessions/archive-all", otherToken,
		map[string]any{"archived": true, "scope": "active"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("non-owner archive status = %d, want 404", rec.Code)
	}
}

func TestAdminUserUpdateAndDeleteCascade(t *testing.T) {
	app, st := newAdminTestApp(t)
	admin := createTestUser(t, st, "admin@example.test", "admin")
	second := createTestUser(t, st, "second@example.test", "admin")
	member := createTestUser(t, st, "member@example.test", "member")

	secondConnector := createTestConnector(t, st, second.ID, "second-dev")
	secondSession := createTestSession(t, st, secondConnector.ID, "second session", "")
	secondProject, err := st.CreateProject(context.Background(), store.Project{
		UserID: second.ID, ConnectorID: secondConnector.ID,
		Name: "second-project", WorkspacePath: "/tmp/second",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	handler := shareTestHandler(app)
	adminToken := testToken(t, admin)
	memberToken := testToken(t, member)

	memberPath := "/api/v2/admin/users/" + member.ID
	// Check the member refusal before the account is disabled: a disabled
	// account cannot authenticate at all, which would surface as 401 instead.
	rec, _ := adminRequest(t, handler, http.MethodPatch, memberPath, memberToken,
		map[string]any{"displayName": "Nope"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member patch status = %d, want 403", rec.Code)
	}

	rec, updated := adminRequest(t, handler, http.MethodPatch, memberPath, adminToken,
		map[string]any{"displayName": "Renamed Member", "role": "admin", "disabled": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("patch status = %d, body %s", rec.Code, rec.Body.String())
	}
	if updated["displayName"] != "Renamed Member" || updated["role"] != "admin" ||
		updated["disabled"] != true {
		t.Fatalf("unexpected patch response: %v", updated)
	}

	rec, _ = adminRequest(t, handler, http.MethodPatch, memberPath, adminToken,
		map[string]any{"role": "owner"})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad role status = %d, want 422", rec.Code)
	}
	rec, _ = adminRequest(t, handler, http.MethodPatch,
		"/api/v2/admin/users/usr_missing", adminToken, map[string]any{"disabled": false})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing user status = %d, want 404", rec.Code)
	}

	// Two administrators, so deleting the other one is allowed, and its
	// connectors, sessions and projects go with it.
	rec, _ = adminRequest(t, handler, http.MethodDelete,
		"/api/v2/admin/users/"+second.ID, adminToken, nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete second admin status = %d, body %s", rec.Code, rec.Body.String())
	}
	if _, err := st.UserByID(context.Background(), second.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("user row survived: %v", err)
	}
	if _, err := st.ConnectorByID(context.Background(), secondConnector.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("connector row survived: %v", err)
	}
	if _, err := st.SessionByIDAny(context.Background(), st.Read(), secondSession.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("session row survived: %v", err)
	}
	if _, err := st.ProjectByID(context.Background(), secondProject.ID, second.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("project row survived: %v", err)
	}

	// The caller is now the only enabled administrator. Deleting or demoting
	// that account must be refused instead of locking the instance.
	rec, body := adminRequest(t, handler, http.MethodDelete,
		"/api/v2/admin/users/"+admin.ID, adminToken, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("last admin delete status = %d, body %s", rec.Code, rec.Body.String())
	}
	if body["detail"] != store.ErrLastAdmin.Error() {
		t.Fatalf("last admin detail = %v", body["detail"])
	}
	rec, body = adminRequest(t, handler, http.MethodPatch,
		"/api/v2/admin/users/"+admin.ID, adminToken, map[string]any{"role": "member"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("self demote status = %d, body %s", rec.Code, rec.Body.String())
	}
	if body["detail"] != store.ErrSelfAdminEdit.Error() {
		t.Fatalf("self demote detail = %v", body["detail"])
	}
}
