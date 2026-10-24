package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// usageTestDayStart normalises a clock reading to UTC midnight so report
// windows can be addressed by date.
func usageTestDayStart(base time.Time) time.Time {
	year, month, day := base.UTC().Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
}

// usageTestDayAt returns a millisecond timestamp on the day daysAgo before
// today, at the given whole hour.
func usageTestDayAt(today time.Time, daysAgo, hour int) int64 {
	return today.AddDate(0, 0, -daysAgo).Add(time.Duration(hour) * time.Hour).UnixMilli()
}

func usageTestInt64(value int64) *int64 { return &value }

// usageTestSetConnector writes the presence clock and revocation state that
// CreateConnector cannot set directly.
func usageTestSetConnector(t *testing.T, st *store.Store, id string, lastSeen *int64, revoked int) {
	t.Helper()
	var seen any
	if lastSeen != nil {
		seen = *lastSeen
	}
	err := st.Write(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE connectors SET last_seen_at = ?, revoked = ? WHERE id = ?`,
			seen, revoked, id)
		return err
	})
	if err != nil {
		t.Fatalf("update connector %s: %v", id, err)
	}
}

// usageTestSetSessionClock writes the timestamps CreateSession fills with
// "now", plus the optional last_activity_at the report prefers.
func usageTestSetSessionClock(t *testing.T, st *store.Store, id string, createdAt, updatedAt int64, lastActivity *int64) {
	t.Helper()
	var activity any
	if lastActivity != nil {
		activity = *lastActivity
	}
	err := st.Write(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET created_at = ?, updated_at = ?, last_activity_at = ?
			WHERE id = ?`, createdAt, updatedAt, activity, id)
		return err
	})
	if err != nil {
		t.Fatalf("update session %s: %v", id, err)
	}
}

// usageTestAddMessage inserts one timeline message at an explicit time so the
// report buckets it on a chosen day.
func usageTestAddMessage(t *testing.T, st *store.Store, sessionID, itemID, role string, atMS int64) {
	t.Helper()
	itemTime := atMS
	err := st.Write(context.Background(), func(ctx context.Context, tx *sql.Tx) error {
		return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID:  sessionID,
			ID:         itemID,
			Type:       "message",
			Status:     "done",
			Role:       role,
			OrderSeq:   atMS,
			UpdatedSeq: 1,
			ItemTime:   &itemTime,
			Payload:    json.RawMessage(`{"text":"hello"}`),
		}, atMS)
	})
	if err != nil {
		t.Fatalf("add message %s: %v", itemID, err)
	}
}

func usageTestDailyPoint(t *testing.T, report map[string]any, date string) map[string]any {
	t.Helper()
	for _, raw := range report["daily"].([]any) {
		point := raw.(map[string]any)
		if point["date"] == date {
			return point
		}
	}
	t.Fatalf("daily point %s is missing", date)
	return nil
}

func usageTestRuntimeRow(t *testing.T, report map[string]any, runtimeType string) map[string]any {
	t.Helper()
	for _, raw := range report["runtimes"].([]any) {
		row := raw.(map[string]any)
		if row["runtimeType"] == runtimeType {
			return row
		}
	}
	t.Fatalf("runtime %s is missing from %v", runtimeType, report["runtimes"])
	return nil
}

// TestAdminUsageReportSessionsFollowActivityWindow builds a dataset whose
// sessions were created and last active on different days, then checks that
// sessions mean "active in the range" at both the summary and daily level.
func TestAdminUsageReportSessionsFollowActivityWindow(t *testing.T) {
	app, st := newAdminTestApp(t)
	admin := createTestUser(t, st, "admin@example.test", "admin")
	ctx := context.Background()
	today := usageTestDayStart(time.Now())
	date := func(daysAgo int) string { return today.AddDate(0, 0, -daysAgo).Format("2006-01-02") }

	createConnector := func(name, tokenHash string) store.Connector {
		t.Helper()
		connector, err := st.CreateConnector(ctx, store.Connector{
			UserID: admin.ID, Name: name, TokenHash: tokenHash, TokenPrefix: name[:2],
		})
		if err != nil {
			t.Fatalf("create connector %s: %v", name, err)
		}
		return connector
	}
	createSession := func(connectorID string) store.Session {
		t.Helper()
		session, err := st.CreateSession(ctx, store.Session{
			ConnectorID: connectorID,
			Runtime:     primaryRuntimeType,
			RuntimeID:   primaryRuntimeType,
			Status:      "idle",
		})
		if err != nil {
			t.Fatalf("create session: %v", err)
		}
		return session
	}

	connA := createConnector("conn-a", "hash-a")
	connB := createConnector("conn-b", "hash-b")
	connRevoked := createConnector("conn-revoked", "hash-c")
	connSilent := createConnector("conn-silent", "hash-d")
	// A was seen today (inside 1d and 30d), B only ten days ago (30d only),
	// C is revoked and must never count, D never heartbeated.
	usageTestSetConnector(t, st, connA.ID, usageTestInt64(usageTestDayAt(today, 0, 1)), 0)
	usageTestSetConnector(t, st, connB.ID, usageTestInt64(usageTestDayAt(today, 10, 1)), 0)
	usageTestSetConnector(t, st, connRevoked.ID, usageTestInt64(usageTestDayAt(today, 0, 1)), 1)
	usageTestSetConnector(t, st, connSilent.ID, nil, 0)

	// Active today; also has a message today.
	sToday := createSession(connA.ID)
	usageTestSetSessionClock(t, st, sToday.ID,
		usageTestDayAt(today, 0, 2), usageTestDayAt(today, 0, 3),
		usageTestInt64(usageTestDayAt(today, 0, 3)))
	usageTestAddMessage(t, st, sToday.ID, "msg-today", "user", usageTestDayAt(today, 0, 2))

	// Last active ten days ago.
	sOld := createSession(connA.ID)
	usageTestSetSessionClock(t, st, sOld.ID,
		usageTestDayAt(today, 10, 2), usageTestDayAt(today, 10, 3),
		usageTestInt64(usageTestDayAt(today, 10, 3)))
	usageTestAddMessage(t, st, sOld.ID, "msg-old", "user", usageTestDayAt(today, 10, 2))

	// No last_activity_at: the report must fall back to updated_at.
	sFallback := createSession(connB.ID)
	usageTestSetSessionClock(t, st, sFallback.ID,
		usageTestDayAt(today, 20, 2), usageTestDayAt(today, 20, 3), nil)
	usageTestAddMessage(t, st, sFallback.ID, "msg-fallback", "user", usageTestDayAt(today, 20, 2))

	// Its last activity is today, but it produced a message five days ago.
	// The historical day must still report a session.
	sHistorical := createSession(connA.ID)
	usageTestSetSessionClock(t, st, sHistorical.ID,
		usageTestDayAt(today, 40, 2), usageTestDayAt(today, 0, 4),
		usageTestInt64(usageTestDayAt(today, 0, 4)))
	usageTestAddMessage(t, st, sHistorical.ID, "msg-historical", "user", usageTestDayAt(today, 5, 2))

	// Completely outside both windows; must not leak into either count.
	sOutside := createSession(connA.ID)
	usageTestSetSessionClock(t, st, sOutside.ID,
		usageTestDayAt(today, 40, 3), usageTestDayAt(today, 40, 3),
		usageTestInt64(usageTestDayAt(today, 40, 3)))

	token := testToken(t, admin)
	handler := app.Handler()
	reportPath := func(from, to string) string {
		return "/api/v2/admin/usage/report?from=" + from + "&to=" + to + "&tz=UTC"
	}

	rec, oneDay := adminRequest(t, handler, http.MethodGet, reportPath(date(0), date(0)), token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("1d report status = %d, body %s", rec.Code, rec.Body.String())
	}
	summary := oneDay["summary"].(map[string]any)
	assertNumber(t, summary, "sessions", 2) // sToday + sHistorical (active today)
	assertNumber(t, summary, "messages", 1) // only today's message
	daily := usageTestDailyPoint(t, oneDay, date(0))
	assertNumber(t, daily, "sessions", 2)
	oneDayRuntime := usageTestRuntimeRow(t, oneDay, primaryRuntimeType)
	assertNumber(t, oneDayRuntime, "configuredConnectors", 1) // connA seen today
	assertNumber(t, oneDayRuntime, "discoveredConnectors", 1)
	assertNumber(t, oneDayRuntime, "sessions", 2)

	rec, thirtyDays := adminRequest(t, handler, http.MethodGet,
		reportPath(date(29), date(0)), token, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("30d report status = %d, body %s", rec.Code, rec.Body.String())
	}
	thirtySummary := thirtyDays["summary"].(map[string]any)
	assertNumber(t, thirtySummary, "sessions", 4) // sToday, sOld, sFallback, sHistorical
	assertNumber(t, thirtySummary, "messages", 4)

	// The same session clock produces different summary values because the
	// window changes; sOutside stays absent from both.
	if thirtySummary["sessions"] == summary["sessions"] {
		t.Fatalf("sessions did not change with the window: 1d=%v 30d=%v",
			summary["sessions"], thirtySummary["sessions"])
	}

	// Historical days with messages must not collapse to zero sessions.
	for _, day := range []string{date(5), date(10), date(20)} {
		point := usageTestDailyPoint(t, thirtyDays, day)
		assertNumber(t, point, "sessions", 1)
	}
	assertNumber(t, usageTestDailyPoint(t, thirtyDays, date(5)), "messages", 1)
	assertNumber(t, usageTestDailyPoint(t, thirtyDays, date(0)), "sessions", 2)

	thirtyRuntime := usageTestRuntimeRow(t, thirtyDays, primaryRuntimeType)
	assertNumber(t, thirtyRuntime, "configuredConnectors", 2) // connA + connB
	assertNumber(t, thirtyRuntime, "discoveredConnectors", 2)
	assertNumber(t, thirtyRuntime, "sessions", 4)
}
