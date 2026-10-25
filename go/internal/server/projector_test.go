package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// newProjectorTestApp builds the smallest app a projection test needs: one
// person, one connector and one session for that connector.
func newProjectorTestApp(t *testing.T) (*App, *store.Store, store.User, store.Connector, store.Session) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "projector.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := New(
		config.Server{DBPath: st.Path(), PublicURL: "http://example.test"},
		st,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	user := createTestUser(t, st, "projector-"+store.NewID("u")+"@example.test", "member")
	token := NewToken("con")
	connector, err := st.CreateConnector(ctx, store.Connector{
		UserID:      user.ID,
		Name:        "projector-test",
		Kind:        "desktop",
		TokenHash:   HashToken(token),
		TokenPrefix: token[:12],
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	session, err := st.CreateSession(ctx, store.Session{ConnectorID: connector.ID, Title: "projector"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	return app, st, user, connector, session
}

func timelineText(t *testing.T, item store.TimelineItem) string {
	t.Helper()
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(item.Payload, &payload); err != nil {
		t.Fatalf("decode payload %q: %v", item.Payload, err)
	}
	return payload.Text
}

// TestTurnEndKeepsTurnStartOrderSeq keeps the answer where it began instead of
// moving it below anything written while the turn streamed.
func TestTurnEndKeepsTurnStartOrderSeq(t *testing.T) {
	app, st, user, connector, session := newProjectorTestApp(t)
	ctx := context.Background()
	conn := &connectorConn{id: connector.ID, userID: user.ID}

	app.onTurnBegin(ctx, conn, protocol.TurnBegin{SessionID: session.ID, TurnID: "turn-1"})
	opened, err := st.TimelineItemByID(ctx, st.Read(), session.ID, assistantItemID("turn-1"))
	if err != nil {
		t.Fatalf("read opened item: %v", err)
	}
	app.onTurnDelta(ctx, conn, protocol.TurnDelta{
		SessionID: session.ID, TurnID: "turn-1", Rev: 1, Text: "hello",
	})
	app.onTurnEnd(ctx, conn, protocol.TurnEnd{
		SessionID: session.ID, TurnID: "turn-1", Status: "done", Text: "hello",
	})
	closed, err := st.TimelineItemByID(ctx, st.Read(), session.ID, assistantItemID("turn-1"))
	if err != nil {
		t.Fatalf("read closed item: %v", err)
	}
	if closed.OrderSeq != opened.OrderSeq {
		t.Fatalf("turn.end moved orderSeq from %d to %d", opened.OrderSeq, closed.OrderSeq)
	}
	if closed.Status != "done" || timelineText(t, closed) != "hello" {
		t.Fatalf("closed item = status %q text %q", closed.Status, timelineText(t, closed))
	}
}

// TestTurnFramesRequireOwnership drops deltas and endings from another
// connector, and ignores a replayed delta revision.
func TestTurnFramesRequireOwnership(t *testing.T) {
	app, st, user, connector, session := newProjectorTestApp(t)
	ctx := context.Background()
	owner := &connectorConn{id: connector.ID, userID: user.ID}
	token := NewToken("con")
	other, err := st.CreateConnector(ctx, store.Connector{
		UserID:      user.ID,
		Name:        "other",
		Kind:        "desktop",
		TokenHash:   HashToken(token),
		TokenPrefix: token[:12],
	})
	if err != nil {
		t.Fatalf("create other connector: %v", err)
	}
	intruder := &connectorConn{id: other.ID, userID: user.ID}

	app.onTurnBegin(ctx, owner, protocol.TurnBegin{SessionID: session.ID, TurnID: "turn-1"})
	app.onTurnDelta(ctx, intruder, protocol.TurnDelta{
		SessionID: session.ID, TurnID: "turn-1", Rev: 1, Text: "evil",
	})
	app.onTurnEnd(ctx, intruder, protocol.TurnEnd{
		SessionID: session.ID, TurnID: "turn-1", Status: "done", Text: "evil",
	})
	stillRunning, err := st.TimelineItemByID(ctx, st.Read(), session.ID, assistantItemID("turn-1"))
	if err != nil {
		t.Fatalf("read item: %v", err)
	}
	if stillRunning.Status != "running" || timelineText(t, stillRunning) != "" {
		t.Fatalf("intruder frames changed item: status %q text %q",
			stillRunning.Status, timelineText(t, stillRunning))
	}

	app.onTurnDelta(ctx, owner, protocol.TurnDelta{
		SessionID: session.ID, TurnID: "turn-1", Rev: 1, Text: "A",
	})
	// Same revision twice: the second copy is a replay and must not append.
	app.onTurnDelta(ctx, owner, protocol.TurnDelta{
		SessionID: session.ID, TurnID: "turn-1", Rev: 1, Text: "B",
	})
	app.onTurnEnd(ctx, owner, protocol.TurnEnd{SessionID: session.ID, TurnID: "turn-1", Status: "done"})
	closed, err := st.TimelineItemByID(ctx, st.Read(), session.ID, assistantItemID("turn-1"))
	if err != nil {
		t.Fatalf("read closed item: %v", err)
	}
	if got := timelineText(t, closed); got != "A" {
		t.Fatalf("replayed delta was applied, text = %q", got)
	}
}

// TestTurnEndExpiresApprovals removes questions a finished turn can no longer
// answer.
func TestTurnEndExpiresApprovals(t *testing.T) {
	app, _, user, connector, session := newProjectorTestApp(t)
	ctx := context.Background()
	conn := &connectorConn{id: connector.ID, userID: user.ID}

	app.onTurnBegin(ctx, conn, protocol.TurnBegin{SessionID: session.ID, TurnID: "turn-1"})
	app.onApprovalRequest(ctx, conn, protocol.ApprovalRequest{
		SessionID: session.ID,
		TurnID:    "turn-1",
		ToolUseID: "tool-1",
		Tool:      "Bash",
		Kind:      "permission",
	})
	if pending := len(app.approvals.byID); pending != 1 {
		t.Fatalf("pending approvals = %d, want 1", pending)
	}
	app.onTurnEnd(ctx, conn, protocol.TurnEnd{SessionID: session.ID, TurnID: "turn-1", Status: "done"})
	if pending := len(app.approvals.byID); pending != 0 {
		t.Fatalf("pending approvals after turn.end = %d, want 0", pending)
	}
}

// TestHistoryReconcileKeepsStableRevision re-imports the same transcript
// without bumping the session, and updates a changed item in place.
func TestHistoryReconcileKeepsStableRevision(t *testing.T) {
	app, st, user, connector, session := newProjectorTestApp(t)
	ctx := context.Background()
	conn := &connectorConn{id: connector.ID, userID: user.ID}
	history := protocol.History{SessionID: session.ID, Items: []protocol.HistoryItem{
		{ID: "m1", Type: "message", Role: "user", Status: "done", Text: "hi", Timestamp: 1000},
		{ID: "m2", Type: "message", Role: "assistant", Status: "done", Text: "hello", Timestamp: 2000},
	}}

	app.applyHistory(ctx, conn, history)
	first, err := st.SessionByIDAny(ctx, st.Read(), session.ID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	item, err := st.TimelineItemByID(ctx, st.Read(), session.ID, "m2")
	if err != nil {
		t.Fatalf("read item: %v", err)
	}
	// A repeated import is a no-op: no new revision and no reset event.
	app.applyHistory(ctx, conn, history)
	second, err := st.SessionByIDAny(ctx, st.Read(), session.ID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if second.Seq != first.Seq {
		t.Fatalf("unchanged history bumped seq from %d to %d", first.Seq, second.Seq)
	}

	// A changed entry updates the text but keeps the position it already has.
	history.Items[1].Text = "hello!"
	app.applyHistory(ctx, conn, history)
	updated, err := st.TimelineItemByID(ctx, st.Read(), session.ID, "m2")
	if err != nil {
		t.Fatalf("read updated item: %v", err)
	}
	third, err := st.SessionByIDAny(ctx, st.Read(), session.ID)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if third.Seq <= second.Seq {
		t.Fatalf("changed history did not bump seq: %d", third.Seq)
	}
	if timelineText(t, updated) != "hello!" {
		t.Fatalf("updated text = %q", timelineText(t, updated))
	}
	if updated.OrderSeq != item.OrderSeq {
		t.Fatalf("reconcile moved orderSeq from %d to %d", item.OrderSeq, updated.OrderSeq)
	}
}

// TestChangesTimelinePageHasMore pages the changes feed and continues from the
// last revision returned instead of the session head.
func TestChangesTimelinePageHasMore(t *testing.T) {
	app, st, user, _, session := newProjectorTestApp(t)
	ctx := context.Background()
	seqs := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		err := st.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
			seq, err := store.AllocateSeq(ctx, tx, session.ID, store.NowMS())
			if err != nil {
				return err
			}
			seqs = append(seqs, seq)
			return store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
				SessionID:  session.ID,
				ID:         "item-" + strconv.Itoa(i),
				Type:       "message",
				Status:     "done",
				Role:       "user",
				OrderSeq:   store.NowMS(),
				UpdatedSeq: seq,
				Payload:    messageContent("m"),
			}, store.NowMS())
		})
		if err != nil {
			t.Fatalf("write item: %v", err)
		}
	}

	req := httptest.NewRequest("GET",
		"/api/v2/sessions/"+session.ID+"/timeline?mode=changes&afterSeq=0&limit=2", nil)
	req.SetPathValue("session_id", session.ID)
	rec := httptest.NewRecorder()
	app.handleTimeline(rec, req, user)
	if rec.Code != 200 {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"hasMore"`
		NextSeq int64            `json:"nextSeq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Items) != 2 || !body.HasMore {
		t.Fatalf("got %d items, hasMore = %v", len(body.Items), body.HasMore)
	}
	if body.NextSeq != seqs[1] {
		t.Fatalf("nextSeq = %d, want %d", body.NextSeq, seqs[1])
	}
}

// TestChangesTimelinePageKeepsTieGroup keeps every row of one revision in the
// same page, so the single-number cursor cannot skip the rest of it.
func TestChangesTimelinePageKeepsTieGroup(t *testing.T) {
	app, st, user, _, session := newProjectorTestApp(t)
	ctx := context.Background()
	err := st.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		seq, err := store.AllocateSeq(ctx, tx, session.ID, store.NowMS())
		if err != nil {
			return err
		}
		for i := 0; i < 3; i++ {
			if err := store.UpsertTimelineItem(ctx, tx, store.TimelineItem{
				SessionID:  session.ID,
				ID:         "tie-" + strconv.Itoa(i),
				Type:       "message",
				Status:     "done",
				Role:       "user",
				OrderSeq:   1,
				UpdatedSeq: seq,
				Payload:    messageContent("m"),
			}, store.NowMS()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("write items: %v", err)
	}

	req := httptest.NewRequest("GET",
		"/api/v2/sessions/"+session.ID+"/timeline?mode=changes&afterSeq=0&limit=2", nil)
	req.SetPathValue("session_id", session.ID)
	rec := httptest.NewRecorder()
	app.handleTimeline(rec, req, user)
	var body struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"hasMore"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if len(body.Items) != 3 || body.HasMore {
		t.Fatalf("got %d items, hasMore = %v; want the whole 3-row revision", len(body.Items), body.HasMore)
	}

	// The same boundary on the backward page must carry the tie group too.
	older := httptest.NewRequest("GET",
		"/api/v2/sessions/"+session.ID+"/timeline?mode=history&beforeOrderSeq=999999&limit=2", nil)
	older.SetPathValue("session_id", session.ID)
	rec = httptest.NewRecorder()
	app.handleTimeline(rec, older, user)
	var olderBody struct {
		Items   []map[string]any `json:"items"`
		HasMore bool             `json:"hasMore"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &olderBody); err != nil {
		t.Fatalf("decode history body: %v", err)
	}
	if len(olderBody.Items) != 3 || olderBody.HasMore {
		t.Fatalf("history got %d items, hasMore = %v; want the whole 3-row group",
			len(olderBody.Items), olderBody.HasMore)
	}
}
