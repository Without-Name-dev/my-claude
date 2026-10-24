package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.CreateUser(context.Background(), User{
		Email: "t@example.com", DisplayName: "T", PasswordHash: "x",
	}); err != nil {
		t.Fatalf("user: %v", err)
	}
	return db
}

// A session discovered for the first time often has nothing better than an
// auto-generated slug. Once the transcript contains a real prompt the title has
// to follow it, otherwise the list shows a name nobody recognises.
func TestDiscoveredTitleFollowsTheTranscript(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	user, _ := db.UserByEmail(ctx, "t@example.com")
	connector, _ := db.CreateConnector(ctx, Connector{
		UserID: user.ID, Name: "c", TokenHash: "h", TokenPrefix: "p",
	})

	session, err := db.UpsertDiscoveredSession(ctx, connector.ID, "", "ext-1",
		"inherited-whistling-widget", `C:\work`, 1000)
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if session.Title != "inherited-whistling-widget" {
		t.Fatalf("title = %q", session.Title)
	}

	updated, err := db.UpsertDiscoveredSession(ctx, connector.ID, "", "ext-1",
		"帮我修一下登录", `C:\work`, 2000)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if updated.Title != "帮我修一下登录" {
		t.Fatalf("title stayed %q, want the new prompt", updated.Title)
	}
}

// A title the person edited is theirs, no matter what the transcript says later.
func TestUserTitleIsNotOverwritten(t *testing.T) {
	db := openTestStore(t)
	ctx := context.Background()

	user, _ := db.UserByEmail(ctx, "t@example.com")
	connector, _ := db.CreateConnector(ctx, Connector{
		UserID: user.ID, Name: "c", TokenHash: "h", TokenPrefix: "p",
	})

	if _, err := db.UpsertDiscoveredSession(ctx, connector.ID, "", "ext-2",
		"first prompt", `C:\work`, 1000); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	sessions, err := db.SessionsForConnector(ctx, connector.ID)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions: %v %v", sessions, err)
	}
	if err := db.SetSessionTitle(ctx, sessions[0].ID, "我自己的标题"); err != nil {
		t.Fatalf("set title: %v", err)
	}

	updated, err := db.UpsertDiscoveredSession(ctx, connector.ID, "", "ext-2",
		"a later prompt", `C:\work`, 3000)
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if updated.Title != "我自己的标题" {
		t.Fatalf("title = %q, want the user's own", updated.Title)
	}
}
