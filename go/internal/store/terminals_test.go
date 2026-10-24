package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestTerminalStoreLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "terminals.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()

	user, err := db.CreateUser(ctx, User{Email: "terminal@example.com", PasswordHash: "hash"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	connector, err := db.CreateConnector(ctx, Connector{
		UserID:      user.ID,
		Name:        "desktop",
		Kind:        "desktop",
		TokenHash:   "token-hash",
		TokenPrefix: "con_prefix",
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	terminal, err := db.CreateTerminal(ctx, Terminal{
		ConnectorID: connector.ID,
		UserID:      user.ID,
		SessionID:   "browse_" + connector.ID,
		Label:       "first",
		Root:        "/work",
		CWD:         "/work/project",
		Shell:       "powershell.exe",
		Cols:        100,
		Rows:        30,
	})
	if err != nil {
		t.Fatalf("create terminal: %v", err)
	}
	if terminal.ID == "" || terminal.Status != "starting" {
		t.Fatalf("unexpected created terminal: %+v", terminal)
	}
	if terminal.CreatedAt == 0 {
		t.Fatalf("created_at was not stamped")
	}

	stored, err := db.TerminalByID(ctx, terminal.ID)
	if err != nil {
		t.Fatalf("by id: %v", err)
	}
	if stored.Label != "first" || stored.Cols != 100 || stored.Rows != 30 {
		t.Fatalf("stored terminal lost fields: %+v", stored)
	}

	if err := db.MarkTerminalRunning(ctx, terminal.ID, 4242); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	stored, _ = db.TerminalByID(ctx, terminal.ID)
	if stored.Status != "running" || stored.PID != 4242 {
		t.Fatalf("running state: %+v", stored)
	}

	if err := db.UpdateTerminalSize(ctx, terminal.ID, 120, 40); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if err := db.UpdateTerminalScrollback(ctx, terminal.ID, 99); err != nil {
		t.Fatalf("scrollback: %v", err)
	}
	stored, _ = db.TerminalByID(ctx, terminal.ID)
	if stored.Cols != 120 || stored.Rows != 40 || stored.ScrollbackSeq != 99 {
		t.Fatalf("size/scrollback: %+v", stored)
	}

	renamed, err := db.RenameTerminal(ctx, terminal.ID, user.ID, "renamed")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if renamed.Label != "renamed" {
		t.Fatalf("label = %q", renamed.Label)
	}
	if _, err := db.RenameTerminal(ctx, terminal.ID, "usr_other", "no"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign rename error = %v", err)
	}

	listed, err := db.ListTerminals(ctx, connector.ID, user.ID)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list: %v, %d rows", err, len(listed))
	}
	all, err := db.ListTerminals(ctx, connector.ID, "")
	if err != nil || len(all) != 1 {
		t.Fatalf("list by connector: %v, %d rows", err, len(all))
	}

	code := 0
	if err := db.MarkTerminalExited(ctx, terminal.ID, &code); err != nil {
		t.Fatalf("mark exited: %v", err)
	}
	stored, _ = db.TerminalByID(ctx, terminal.ID)
	if stored.Status != "exited" || stored.ExitCode == nil || *stored.ExitCode != 0 {
		t.Fatalf("exited state: %+v", stored)
	}

	if err := db.DeleteTerminal(ctx, terminal.ID, user.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := db.TerminalByID(ctx, terminal.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete error = %v", err)
	}
}
