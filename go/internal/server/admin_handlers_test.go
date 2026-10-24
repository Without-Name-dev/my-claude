package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/auth"
	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

func newAdminTestApp(t *testing.T) (*App, *store.Store) {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := New(
		config.Server{DBPath: st.Path(), PublicURL: "http://example.test"},
		st,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return app, st
}

func createTestUser(t *testing.T, st *store.Store, email, role string) store.User {
	t.Helper()
	hash, err := auth.HashPassword("secret", "")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	user, err := st.CreateUser(context.Background(), store.User{
		Email:        email,
		DisplayName:  email,
		Role:         role,
		PasswordHash: hash,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return user
}

func testToken(t *testing.T, user store.User) string {
	t.Helper()
	token, err := auth.CreateUserAccessToken(user.ID, auth.DefaultUserExpiresIn)
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return token
}

func adminRequest(t *testing.T, handler http.Handler, method, path, token string, body any) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var decoded map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &decoded)
	}
	return rec, decoded
}

func TestAdminRoutesRequireAdmin(t *testing.T) {
	app, st := newAdminTestApp(t)
	member := createTestUser(t, st, "member@example.test", "member")
	token := testToken(t, member)
	handler := app.Handler()

	rec, body := adminRequest(t, handler, http.MethodGet, "/api/v2/admin/usage/live", token, nil)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if body["detail"] != "admin required" {
		t.Fatalf("detail = %v, want admin required", body["detail"])
	}

	rec, _ = adminRequest(t, handler, http.MethodGet, "/api/v2/admin/usage/live", "", nil)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}
}

func TestAdminUsageLiveUsesRealCounts(t *testing.T) {
	app, st := newAdminTestApp(t)
	admin := createTestUser(t, st, "admin@example.test", "admin")
	other := createTestUser(t, st, "other@example.test", "member")

	adminConnector, err := st.CreateConnector(context.Background(), store.Connector{
		UserID: admin.ID, Name: "admin-dev", TokenHash: "hash-admin", TokenPrefix: "aa",
	})
	if err != nil {
		t.Fatalf("create admin connector: %v", err)
	}
	otherConnector, err := st.CreateConnector(context.Background(), store.Connector{
		UserID: other.ID, Name: "other-dev", TokenHash: "hash-other", TokenPrefix: "bb",
	})
	if err != nil {
		t.Fatalf("create other connector: %v", err)
	}
	// Two live sockets, but only one belongs to the signed-in administrator.
	app.connectors.register(&connectorConn{id: adminConnector.ID, userID: admin.ID, outbound: make(chan []byte, 1)})
	app.connectors.register(&connectorConn{id: otherConnector.ID, userID: other.ID, outbound: make(chan []byte, 1)})
	t.Cleanup(func() {
		app.connectors.drop(adminConnector.ID)
		app.connectors.drop(otherConnector.ID)
	})

	session, err := st.CreateSession(context.Background(), store.Session{
		ConnectorID: adminConnector.ID,
		Runtime:     "claude",
		RuntimeID:   "claude",
		Status:      "running",
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	now := store.NowMS()
	if err := st.Write(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		if err := store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID: session.ID, ID: "msg_user", Type: "message", Status: "done",
			Role: "user", OrderSeq: now, UpdatedSeq: 1,
			Payload: json.RawMessage(`{"text":"hello"}`),
		}, now); err != nil {
			return err
		}
		return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID: session.ID, ID: "msg_assistant", Type: "message", Status: "done",
			Role: "assistant", OrderSeq: now, UpdatedSeq: 1,
			Payload: json.RawMessage(`{"text":"hi"}`),
		}, now)
	}); err != nil {
		t.Fatalf("write timeline: %v", err)
	}

	token := testToken(t, admin)
	handler := app.Handler()

	rec, body := adminRequest(t, handler, http.MethodGet, "/api/v2/admin/usage/live", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("live status = %d, body %s", rec.Code, rec.Body.String())
	}
	assertNumber(t, body, "onlineConnectors", 1) // only the admin's socket
	assertNumber(t, body, "totalConnectors", 2)
	assertNumber(t, body, "runningTurns", 1)
	assertNumber(t, body, "totalUsers", 2)
	assertNumber(t, body, "totalSessions", 1)
	assertNumber(t, body, "activeSessions15m", 1)

	today := time.Now().UTC().Format("2006-01-02")
	path := "/api/v2/admin/usage/report?from=" + today + "&to=" + today + "&tz=UTC"
	rec, report := adminRequest(t, handler, http.MethodGet, path, token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("report status = %d, body %s", rec.Code, rec.Body.String())
	}
	summary := report["summary"].(map[string]any)
	assertNumber(t, summary, "messages", 1)
	assertNumber(t, summary, "turns", 1)
	assertNumber(t, summary, "sessions", 1)
	assertNumber(t, summary, "totalUsers", 2)
	assertNumber(t, summary, "totalConnectors", 2)
	assertNumber(t, summary, "activeUsers", 1)

	daily := report["daily"].([]any)
	if len(daily) != 1 {
		t.Fatalf("daily points = %d, want 1", len(daily))
	}
	point := daily[0].(map[string]any)
	assertNumber(t, point, "sessions", 1)
	assertNumber(t, point, "devices", 2)
	assertNumber(t, point, "messages", 1)
	assertNumber(t, point, "turns", 1)
	assertNumber(t, point, "activeUsers", 1)
	assertNumber(t, point, "connectorsOnline", 0) // no heartbeat was recorded

	for _, key := range []string{"range", "users", "runtimes", "devices", "quality"} {
		if _, ok := report[key]; !ok {
			t.Fatalf("report is missing %q", key)
		}
	}
	if got := len(report["hourOfDay"].([]any)); got != 24 {
		t.Fatalf("hourOfDay points = %d, want 24", got)
	}
	quality := report["quality"].(map[string]any)
	outcomes := quality["turnOutcomes"].([]any)
	if len(outcomes) != 4 {
		t.Fatalf("turnOutcomes = %d, want the four documented outcomes", len(outcomes))
	}
}

func TestAdminUsersCreateAndList(t *testing.T) {
	app, st := newAdminTestApp(t)
	admin := createTestUser(t, st, "admin@example.test", "admin")
	token := testToken(t, admin)
	handler := app.Handler()

	rec, created := adminRequest(t, handler, http.MethodPost, "/api/v2/admin/users", token, map[string]any{
		"email":       "new@example.test",
		"displayName": "New Person",
		"role":        "member",
		"password":    "s3cret",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", rec.Code, rec.Body.String())
	}
	if created["userId"] == "" || created["email"] != "new@example.test" {
		t.Fatalf("unexpected create response: %v", created)
	}
	if created["role"] != "member" || created["disabled"] != false {
		t.Fatalf("unexpected role/disabled: %v", created)
	}

	rec, list := adminRequest(t, handler, http.MethodGet, "/api/v2/admin/users?limit=10&offset=0", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, body %s", rec.Code, rec.Body.String())
	}
	if got := len(list["users"].([]any)); got != 2 {
		t.Fatalf("users = %d, want 2", got)
	}
	assertNumber(t, list, "total", 2)

	rec, _ = adminRequest(t, handler, http.MethodPost, "/api/v2/admin/users", token, map[string]any{
		"email": "new@example.test", "displayName": "Duplicate", "password": "x",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409", rec.Code)
	}

	rec, _ = adminRequest(t, handler, http.MethodGet, "/api/v2/admin/users?limit=5000", token, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad limit status = %d, want 422", rec.Code)
	}
}

func TestAdminAnnouncementRoundTrip(t *testing.T) {
	app, st := newAdminTestApp(t)
	admin := createTestUser(t, st, "admin@example.test", "admin")
	token := testToken(t, admin)
	handler := app.Handler()

	rec, current := adminRequest(t, handler, http.MethodGet, "/api/v2/admin/announcement", token, nil)
	if rec.Code != http.StatusOK || current["enabled"] != false || current["publishedAt"] != nil {
		t.Fatalf("unexpected default announcement: %d %v", rec.Code, current)
	}

	rec, updated := adminRequest(t, handler, http.MethodPut, "/api/v2/admin/announcement", token, map[string]any{
		"enabled": true, "markdown": "# Maintenance",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update status = %d, body %s", rec.Code, rec.Body.String())
	}
	if updated["enabled"] != true || updated["markdown"] != "# Maintenance" || updated["publishedAt"] == nil {
		t.Fatalf("unexpected update response: %v", updated)
	}
	firstPublished := updated["publishedAt"]

	rec, public := adminRequest(t, handler, http.MethodGet, "/api/v2/announcement", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("public announcement status = %d", rec.Code)
	}
	publicAnnouncement := public["announcement"].(map[string]any)
	if publicAnnouncement["markdown"] != "# Maintenance" {
		t.Fatalf("public markdown = %v", publicAnnouncement["markdown"])
	}

	// Same content is a no-op and must not bump publishedAt.
	_, again := adminRequest(t, handler, http.MethodPut, "/api/v2/admin/announcement", token, map[string]any{
		"enabled": true, "markdown": "# Maintenance",
	})
	if again["publishedAt"] != firstPublished {
		t.Fatalf("publishedAt changed on no-op: %v -> %v", firstPublished, again["publishedAt"])
	}

	rec, _ = adminRequest(t, handler, http.MethodPut, "/api/v2/admin/announcement", token, map[string]any{
		"enabled": true, "markdown": "   ",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("blank markdown status = %d, want 422", rec.Code)
	}
}

func TestAdminSettingsAndServiceInfo(t *testing.T) {
	app, st := newAdminTestApp(t)
	admin := createTestUser(t, st, "admin@example.test", "admin")
	token := testToken(t, admin)
	handler := app.Handler()

	rec, settings := adminRequest(t, handler, http.MethodGet, "/api/v2/admin/settings", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings status = %d, body %s", rec.Code, rec.Body.String())
	}
	if settings["registrationOpen"] != true {
		t.Fatalf("registrationOpen = %v, want true", settings["registrationOpen"])
	}
	if settings["version"] == "" || settings["databasePath"] == nil {
		t.Fatalf("missing instance facts: %v", settings)
	}
	assertNumber(t, settings, "userCount", 1)

	rec, patched := adminRequest(t, handler, http.MethodPatch, "/api/v2/admin/settings", token, map[string]any{
		"registrationOpen": false,
		"email":            map[string]any{"fromAddress": "noreply@example.test"},
	})
	if rec.Code != http.StatusOK || patched["registrationOpen"] != false {
		t.Fatalf("patch response = %d %v", rec.Code, patched)
	}
	email := patched["email"].(map[string]any)
	if email["fromAddress"] != "noreply@example.test" {
		t.Fatalf("email.fromAddress = %v", email["fromAddress"])
	}

	// The audit names PUT; both verbs must update.
	rec, put := adminRequest(t, handler, http.MethodPut, "/api/v2/admin/settings", token, map[string]any{
		"registrationOpen": true,
	})
	if rec.Code != http.StatusOK || put["registrationOpen"] != true {
		t.Fatalf("put response = %d %v", rec.Code, put)
	}

	rec, service := adminRequest(t, handler, http.MethodGet, "/api/v2/admin/service", token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("service status = %d, body %s", rec.Code, rec.Body.String())
	}
	if service["database"] != "SQLite" || service["coordination"] != "memory" {
		t.Fatalf("unexpected service identity: %v", service)
	}
	if service["endpoint"] != "http://example.com" {
		t.Fatalf("endpoint = %v", service["endpoint"])
	}
	if service["databaseSchemaVersion"] == "" || service["startedAt"] == "" {
		t.Fatalf("missing schema/start time: %v", service)
	}
	assertNumber(t, service, "userCount", 1)

	rec, _ = adminRequest(t, handler, http.MethodGet,
		"/api/v2/admin/usage/report?from=2026-01-01&to=2026-01-02&tz=Not/AZone", token, nil)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad timezone status = %d, want 422", rec.Code)
	}
}

func assertNumber(t *testing.T, body map[string]any, key string, want int) {
	t.Helper()
	value, ok := body[key]
	if !ok {
		t.Fatalf("%q is missing from %v", key, body)
	}
	number, ok := value.(float64)
	if !ok {
		t.Fatalf("%q = %v (%T), want a number", key, value, value)
	}
	if int(number) != want {
		t.Fatalf("%q = %v, want %d", key, number, want)
	}
}
