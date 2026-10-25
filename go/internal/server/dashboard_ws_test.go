package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// expectDirty waits for one dashboard notification and fails when none arrives.
func expectDirty(t *testing.T, dirty <-chan struct{}) {
	t.Helper()
	select {
	case <-dirty:
	case <-time.After(2 * time.Second):
		t.Fatal("a dashboard notification was expected but none arrived")
	}
}

// expectNoDirty fails when a dashboard notification arrives within the window.
func expectNoDirty(t *testing.T, dirty <-chan struct{}, window time.Duration) {
	t.Helper()
	select {
	case <-dirty:
		t.Fatal("the dashboard was notified for a change it does not render")
	case <-time.After(window):
	}
}

// TestDashboardDirtyForEveryListVisibleChange pins the rule documented on
// dashboardHub. Every timeline write allocates a revision, and AllocateSeq also
// moves the session's seq, sort_at and last_activity_at; SessionView renders
// exactly those as the unread flag, ordering and relative time. So item
// boundaries and transcript imports are inventory changes. A streamed delta is
// not: it does not allocate a revision and only changes the open conversation.
func TestDashboardDirtyForEveryListVisibleChange(t *testing.T) {
	app, _, user, connector, session := newProjectorTestApp(t)
	ctx := context.Background()

	dirty, unsubscribe := app.dashboards.subscribe(user.ID)
	defer unsubscribe()

	// An item creation/completion allocates a revision and advances the session's
	// list fields, so it must refresh the dashboard.
	itemID := "msg_dashboard_test_assistant"
	seq, err := app.writeTimeline(ctx, session.ID, func(tx *sql.Tx, seq int64) error {
		return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
			SessionID: session.ID, ID: itemID, Type: "message", Status: "running",
			Role: "assistant", OrderSeq: store.NowMS(), UpdatedSeq: seq,
			Payload: messageContent("streaming text"),
		}, store.NowMS())
	})
	if err != nil {
		t.Fatalf("write timeline item: %v", err)
	}
	app.publishItem(ctx, session.ID, itemID, seq, true)
	expectDirty(t, dirty)

	// Importing a transcript allocates a revision too: it moves the session to
	// the top of the list and flips it unread. The dashboard has to say so.
	conn := &connectorConn{id: connector.ID, userID: user.ID, done: make(chan struct{})}
	app.applyHistory(ctx, conn, protocol.History{
		SessionID: session.ID,
		Items: []protocol.HistoryItem{{
			ID: "msg_history_test", Type: "message", Role: "assistant",
			Status: "done", Text: "imported", Timestamp: store.NowMS(),
		}},
	})
	expectDirty(t, dirty)

	// A streamed delta is the hot path this throttle exists for: it carries text
	// to the open conversation without allocating a revision, so the list cannot
	// change and no inventory snapshot is due.
	app.publishStreamingView(ctx, session.ID, itemID, seq, session.SortAt, session.CreatedAt, "streaming text, one more token")
	expectNoDirty(t, dirty, 150*time.Millisecond)

	// A session meta change is inventory: the list's status dot, unread flag and
	// sort key all live in the snapshot.
	app.setSessionStatus(ctx, session.ID, "running")
	expectDirty(t, dirty)

	// An inventory refresh changes the discovered session set.
	app.applyInventory(ctx, conn, protocol.Inventory{Sessions: []protocol.InventorySession{{
		ExternalID: "ext-dashboard-test",
		CWD:        "/tmp/dashboard-test",
		Title:      "Discovered conversation",
		UpdatedAt:  store.NowMS(),
	}}})
	expectDirty(t, dirty)
}

// TestDashboardSnapshotSizeForLargeInventory measures what one snapshot costs
// for a realistic session count. The number is the reason the snapshot is only
// sent on inventory changes: on a phone, this payload used to be rebuilt and
// resent every dashboardRefresh tick while any timeline item was flowing.
func TestDashboardSnapshotSizeForLargeInventory(t *testing.T) {
	app, st, user, connector, _ := newProjectorTestApp(t)
	ctx := context.Background()

	project, err := st.EnsureProject(ctx, user.ID, connector.ID, "/projects/dashboard-size", "dashboard-size", false)
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	const sessions = 76
	for index := 1; index < sessions; index++ {
		if _, err := st.CreateSession(ctx, store.Session{
			ConnectorID: connector.ID,
			ProjectID:   project.ID,
			Title:       fmt.Sprintf("Conversation %d with a representative title", index),
			CWD:         "/projects/dashboard-size",
			Status:      "idle",
		}); err != nil {
			t.Fatalf("create session %d: %v", index, err)
		}
	}

	start := time.Now()
	body, err := app.dashboardSnapshot(ctx, user.ID)
	if err != nil {
		t.Fatalf("build dashboard snapshot: %v", err)
	}
	elapsed := time.Since(start)

	var decoded struct {
		Sessions []json.RawMessage `json:"sessions"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode dashboard snapshot: %v", err)
	}
	if len(decoded.Sessions) != sessions {
		t.Fatalf("snapshot sessions = %d, want %d", len(decoded.Sessions), sessions)
	}
	t.Logf("dashboard snapshot: sessions=%d bytes=%d build=%s", len(decoded.Sessions), len(body), elapsed)
}
