package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func testUserAndConnector(t *testing.T, db *Store) (User, Connector) {
	t.Helper()
	ctx := context.Background()
	user, err := db.UserByEmail(ctx, "t@example.com")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	connector, err := db.CreateConnector(ctx, Connector{
		UserID: user.ID, Name: "c", TokenHash: "h", TokenPrefix: "p",
	})
	if err != nil {
		t.Fatalf("connector: %v", err)
	}
	return user, connector
}

// A transcript that reappears must undo the "missing" mark, or the session
// stays invisible in the list even though the file is back on disk.
func TestDiscoveredSessionIsRestoredFromMissing(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	_, connector := testUserAndConnector(t, db)

	session, err := db.UpsertDiscoveredSession(ctx, connector.ID, "", "ext-1",
		"title", "/work", 1000)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := db.MarkSessionMissing(ctx, connector.ID, "ext-1"); err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	missing, err := db.SessionByIDAny(ctx, db.Read(), session.ID)
	if err != nil || missing.SourceState != "missing" {
		t.Fatalf("source state = %q, err %v", missing.SourceState, err)
	}

	restored, err := db.UpsertDiscoveredSession(ctx, connector.ID, "", "ext-1",
		"title", "/work", 2000)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if restored.SourceState != "visible" {
		t.Fatalf("source state = %q, want visible", restored.SourceState)
	}
}

// The share snapshot is capped; it has to keep the newest items, not the
// opening of a long conversation.
func TestTimelineItemsReturnsNewestWindow(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	_, connector := testUserAndConnector(t, db)
	session, err := db.CreateSession(ctx, Session{
		ConnectorID: connector.ID, Runtime: "claude", RuntimeID: "claude",
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		for i := 0; i < 5; i++ {
			item := TimelineItem{
				SessionID: session.ID, ID: "msg-" + string(rune('a'+i)),
				Type: "message", Status: "done",
				OrderSeq: int64(1000 + i), UpdatedSeq: int64(i + 1),
				Payload: []byte(`{"text":"x"}`),
			}
			if err := UpsertTimelineItem(ctx, tx, item, 5000); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write timeline: %v", err)
	}

	items, err := db.TimelineItems(ctx, db.Read(), session.ID, 3)
	if err != nil {
		t.Fatalf("timeline items: %v", err)
	}
	want := []string{"msg-c", "msg-d", "msg-e"}
	if len(items) != len(want) {
		t.Fatalf("got %d items, want %d", len(items), len(want))
	}
	for i, id := range want {
		if items[i].ID != id {
			t.Fatalf("item %d = %q, want %q", i, items[i].ID, id)
		}
	}
}

// An item written without order_seq used to be stamped with time zero and
// sort ahead of everything the person already said.
func TestTimelineItemWithoutOrderSeqSortsByArrival(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	_, connector := testUserAndConnector(t, db)
	session, err := db.CreateSession(ctx, Session{
		ConnectorID: connector.ID, Runtime: "claude", RuntimeID: "claude",
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		earlier := TimelineItem{
			SessionID: session.ID, ID: "early", Type: "message", Status: "done",
			OrderSeq: 500, UpdatedSeq: 1, Payload: []byte(`{"text":"first"}`),
		}
		if err := UpsertTimelineItem(ctx, tx, earlier, 500); err != nil {
			return err
		}
		later := TimelineItem{
			SessionID: session.ID, ID: "late", Type: "message", Status: "done",
			OrderSeq: 0, UpdatedSeq: 2, Payload: []byte(`{"text":"second"}`),
		}
		return UpsertTimelineItem(ctx, tx, later, 1000)
	}); err != nil {
		t.Fatalf("write timeline: %v", err)
	}

	items, err := db.TimelineItemsLatest(ctx, db.Read(), session.ID, 10)
	if err != nil {
		t.Fatalf("timeline items: %v", err)
	}
	if len(items) != 2 || items[0].ID != "early" || items[1].ID != "late" {
		t.Fatalf("order = %v, want early then late", timelineIDs(items))
	}
}

func timelineIDs(items []TimelineItem) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

// A connector whose owner was disabled must not authenticate, even though its
// token row still exists.
func TestConnectorTokenRejectedForDisabledUser(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	user, connector := testUserAndConnector(t, db)

	if _, err := db.ConnectorByTokenHash(ctx, connector.TokenHash); err != nil {
		t.Fatalf("enabled lookup: %v", err)
	}
	if err := db.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE users SET disabled = 1 WHERE id = ?`, user.ID)
		return err
	}); err != nil {
		t.Fatalf("disable user: %v", err)
	}
	if _, err := db.ConnectorByTokenHash(ctx, connector.TokenHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disabled lookup error = %v, want ErrNotFound", err)
	}
}

// Losing the socket that owns the connector proves nothing is still running
// there; a stale close from a replaced socket must leave the live turns alone.
func TestConnectorDisconnectResetsRunningSessions(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	_, connector := testUserAndConnector(t, db)
	session, err := db.CreateSession(ctx, Session{
		ConnectorID: connector.ID, Runtime: "claude", RuntimeID: "claude",
		Status: "running",
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	if err := db.SetConnectorPresence(ctx, connector.ID, "conn-1", true); err != nil {
		t.Fatalf("online: %v", err)
	}

	if err := db.SetConnectorPresence(ctx, connector.ID, "conn-2", false); err != nil {
		t.Fatalf("stale offline: %v", err)
	}
	stored, err := db.SessionByIDAny(ctx, db.Read(), session.ID)
	if err != nil || stored.Status != "running" {
		t.Fatalf("status after stale close = %q, err %v", stored.Status, err)
	}

	if err := db.SetConnectorPresence(ctx, connector.ID, "conn-1", false); err != nil {
		t.Fatalf("offline: %v", err)
	}
	stored, err = db.SessionByIDAny(ctx, db.Read(), session.ID)
	if err != nil || stored.Status != "idle" {
		t.Fatalf("status after real close = %q, err %v", stored.Status, err)
	}
}

// A terminal may only point at a session the caller owns on the same
// connector; its synthetic browse id is the exception.
func TestCreateTerminalValidatesSessionOwnership(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	user, connector := testUserAndConnector(t, db)
	session, err := db.CreateSession(ctx, Session{
		ConnectorID: connector.ID, Runtime: "claude", RuntimeID: "claude",
	})
	if err != nil {
		t.Fatalf("session: %v", err)
	}

	if _, err := db.CreateTerminal(ctx, Terminal{
		ConnectorID: connector.ID, UserID: user.ID, SessionID: session.ID,
	}); err != nil {
		t.Fatalf("own session terminal: %v", err)
	}

	other, err := db.CreateUser(ctx, User{
		Email: "other@example.com", DisplayName: "Other", PasswordHash: "x",
	})
	if err != nil {
		t.Fatalf("other user: %v", err)
	}
	otherConnector, err := db.CreateConnector(ctx, Connector{
		UserID: other.ID, Name: "other", TokenHash: "h2", TokenPrefix: "p2",
	})
	if err != nil {
		t.Fatalf("other connector: %v", err)
	}
	_, err = db.CreateTerminal(ctx, Terminal{
		ConnectorID: otherConnector.ID, UserID: other.ID, SessionID: session.ID,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign session terminal error = %v, want ErrNotFound", err)
	}
}

// Archive-all used to build one IN list for every session, which SQLite
// rejects once the list is long enough. Crossing a batch boundary must still
// archive and return every row.
func TestArchiveAllChunksLargeSessionSets(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()
	user, connector := testUserAndConnector(t, db)

	const count = sessionArchiveBatch + 10
	for i := 0; i < count; i++ {
		if _, err := db.CreateSession(ctx, Session{
			ConnectorID: connector.ID, Runtime: "claude", RuntimeID: "claude",
		}); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
	}

	archived, err := db.SetSessionsArchivedWithScope(ctx, user.ID, "", true, "active")
	if err != nil {
		t.Fatalf("archive all: %v", err)
	}
	if len(archived) != count {
		t.Fatalf("archived %d sessions, want %d", len(archived), count)
	}
	for _, session := range archived {
		if !session.Archived {
			t.Fatalf("session %s was not archived", session.ID)
		}
	}

	restored, err := db.SetSessionsArchivedWithScope(ctx, user.ID, "", false, "archived")
	if err != nil {
		t.Fatalf("restore all: %v", err)
	}
	if len(restored) != count {
		t.Fatalf("restored %d sessions, want %d", len(restored), count)
	}
}
