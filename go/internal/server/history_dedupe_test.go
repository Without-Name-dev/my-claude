package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// writeUserMessage stores a message the way the phone's send path does.
func writeUserMessage(t *testing.T, app *App, st *store.Store, sessionID, id, text string) {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"text": text})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	seq, err := app.writeTimeline(context.Background(), sessionID, func(tx *sql.Tx, seq int64) error {
		return store.UpsertTimelineItem(context.Background(), tx, store.TimelineItem{
			SessionID:  sessionID,
			ID:         id,
			Type:       "message",
			Status:     "done",
			Role:       "user",
			OrderSeq:   store.NowMS(),
			UpdatedSeq: seq,
			Payload:    payload,
		}, store.NowMS())
	})
	if err != nil {
		t.Fatalf("write timeline: %v", err)
	}
	_ = seq
}

func timelineCount(t *testing.T, st *store.Store, sessionID string) int {
	t.Helper()
	items, err := st.TimelineItemsLatest(context.Background(), st.Read(), sessionID, 100)
	if err != nil {
		t.Fatalf("read timeline: %v", err)
	}
	return len(items)
}

// A transcript copy of a message the phone already sent must not be imported a
// second time. Their ids differ, so the importer identifies them by role and
// text — which is exactly what the periodic transcript refresh relies on.
func TestHistorySkipsMessagesTheServerAlreadyShows(t *testing.T) {
	app, st, user, connector, session := newProjectorTestApp(t)
	conn := registerFakeConnector(app, user, connector)
	writeUserMessage(t, app, st, session.ID, "msg_client_user", "你好")

	app.applyHistory(context.Background(), conn, protocol.History{
		SessionID: session.ID,
		Items: []protocol.HistoryItem{
			{ID: "hist_dup", Type: "message", Role: "user", Status: "done", Text: "  你好 "},
		},
	})
	if got := timelineCount(t, st, session.ID); got != 1 {
		t.Fatalf("timeline has %d items, want the phone's message only", got)
	}

	// A message that only exists in the transcript is imported.
	app.applyHistory(context.Background(), conn, protocol.History{
		SessionID: session.ID,
		Items: []protocol.HistoryItem{
			{ID: "hist_new", Type: "message", Role: "user", Status: "done", Text: "在终端里输入的话"},
			{ID: "hist_reply", Type: "message", Role: "assistant", Status: "done", Text: "回复"},
		},
	})
	if got := timelineCount(t, st, session.ID); got != 3 {
		t.Fatalf("timeline has %d items, want the terminal message and its reply imported", got)
	}

	// Re-importing the same window changes nothing.
	app.applyHistory(context.Background(), conn, protocol.History{
		SessionID: session.ID,
		Items: []protocol.HistoryItem{
			{ID: "hist_new", Type: "message", Role: "user", Status: "done", Text: "在终端里输入的话"},
			{ID: "hist_reply", Type: "message", Role: "assistant", Status: "done", Text: "回复"},
		},
	})
	if got := timelineCount(t, st, session.ID); got != 3 {
		t.Fatalf("timeline has %d items after a repeat import, want 3", got)
	}
}
