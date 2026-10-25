package connector

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/bridge"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

type sessionSpawn struct {
	called bool
	cwd    string
	id     string
	title  string
	resume bool
}

// spyOnSpawns replaces the terminal launcher and records what it was asked to
// start. Spawns never reach the operating system in these tests.
func spyOnSpawns(t *testing.T) (*sessionSpawn, *int) {
	t.Helper()
	calls := 0
	spawn := &sessionSpawn{}
	previous := startSessionTerminalProcess
	startSessionTerminalProcess = func(cwd, externalSessionID, title string, resume bool) (string, error) {
		calls++
		spawn.called, spawn.cwd, spawn.id, spawn.title, spawn.resume = true, cwd, externalSessionID, title, resume
		return "fake-claude", nil
	}
	t.Cleanup(func() { startSessionTerminalProcess = previous })
	return spawn, &calls
}

func TestSessionOpenThrottleAllowsOneSpawnPerMinute(t *testing.T) {
	state := &sessionOpenState{last: map[string]time.Time{}}
	now := time.Unix(1_700_000_000, 0)

	if wait, ok := state.allow("ext-1", now); !ok || wait != 0 {
		t.Fatalf("first spawn: ok=%v wait=%v, want true 0", ok, wait)
	}
	if wait, ok := state.allow("ext-1", now.Add(10*time.Second)); ok || wait != 50*time.Second {
		t.Fatalf("second spawn: ok=%v wait=%v, want false 50s", ok, wait)
	}
	if _, ok := state.allow("ext-2", now.Add(10*time.Second)); !ok {
		t.Fatal("a different session shares another session's throttle")
	}
	if _, ok := state.allow("ext-1", now.Add(61*time.Second)); !ok {
		t.Fatal("the throttle did not expire after a minute")
	}
}

func TestOpenSessionSpawnsResumeOnceInCwd(t *testing.T) {
	t.Setenv("AA_ALLOW_SPAWN", "1")
	c := testClient(nil)
	spawn, calls := spyOnSpawns(t)
	cwd := t.TempDir()

	c.openSession(protocol.SessionOpen{
		SessionID:         "ses-1",
		ExternalSessionID: "ext-1",
		CWD:               cwd,
		Title:             "A conversation",
		Resume:            true,
	})
	if !spawn.called || *calls != 1 {
		t.Fatalf("spawn called %d times, want 1", *calls)
	}
	if spawn.cwd != cwd || spawn.id != "ext-1" || !spawn.resume || spawn.title != "A conversation" {
		t.Fatalf("spawn = %+v, want cwd=%q id=ext-1 resume title", spawn, cwd)
	}

	// A second click inside the window must not start another terminal.
	c.openSession(protocol.SessionOpen{
		SessionID:         "ses-1",
		ExternalSessionID: "ext-1",
		CWD:               cwd,
		Resume:            true,
	})
	if *calls != 1 {
		t.Fatalf("spawn called %d times inside the throttle window, want 1", *calls)
	}
}

func TestOpenSessionSpawnsNewConversation(t *testing.T) {
	t.Setenv("AA_ALLOW_SPAWN", "1")
	c := testClient(nil)
	spawn, calls := spyOnSpawns(t)

	c.openSession(protocol.SessionOpen{
		SessionID:         "ses-new",
		ExternalSessionID: "9b2c5f0e-6b7a-4f0b-9d1e-1234567890ab",
		CWD:               t.TempDir(),
		Resume:            false,
	})
	if *calls != 1 || spawn.resume {
		t.Fatalf("new conversation spawn = %d calls, resume=%v; want one non-resume spawn", *calls, spawn.resume)
	}
}

func TestOpenSessionRefusals(t *testing.T) {
	liveBridge := func(t *testing.T, id string) *bridge.Bridge {
		t.Helper()
		b := bridge.New(t.TempDir())
		if err := os.MkdirAll(b.LiveDir(), 0o755); err != nil {
			t.Fatal(err)
		}
		stamp := strconv.FormatInt(time.Now().UnixMilli(), 10)
		if err := os.WriteFile(filepath.Join(b.LiveDir(), id+".txt"), []byte(stamp), 0o644); err != nil {
			t.Fatal(err)
		}
		return b
	}

	outboxBridge := func(t *testing.T, id string, ts time.Time) *bridge.Bridge {
		t.Helper()
		b := bridge.New(t.TempDir())
		body, err := json.Marshal(map[string]any{"sessionId": id, "status": "idle", "ts": ts.UnixMilli()})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(b.OutboxPath(), body, 0o644); err != nil {
			t.Fatal(err)
		}
		return b
	}

	cases := []struct {
		name   string
		env    string
		bridge *bridge.Bridge
		msg    protocol.SessionOpen
	}{
		{
			name: "missing runtime id",
			env:  "1",
			msg:  protocol.SessionOpen{SessionID: "ses-1", CWD: t.TempDir()},
		},
		{
			name: "unsafe runtime id",
			env:  "1",
			msg: protocol.SessionOpen{
				SessionID: "ses-1", ExternalSessionID: "ext-1 & calc", CWD: t.TempDir(),
			},
		},
		{
			name: "option-like runtime id",
			env:  "1",
			msg: protocol.SessionOpen{
				SessionID: "ses-1", ExternalSessionID: "--dangerously-skip-permissions", CWD: t.TempDir(),
			},
		},
		{
			name: "spawning disabled",
			env:  "0",
			msg: protocol.SessionOpen{
				SessionID: "ses-1", ExternalSessionID: "ext-1", CWD: t.TempDir(),
			},
		},
		{
			name: "empty cwd",
			env:  "1",
			msg:  protocol.SessionOpen{SessionID: "ses-1", ExternalSessionID: "ext-1"},
		},
		{
			name: "missing cwd",
			env:  "1",
			msg: protocol.SessionOpen{
				SessionID: "ses-1", ExternalSessionID: "ext-1",
				CWD: filepath.Join(t.TempDir(), "does-not-exist"),
			},
		},
		{
			name:   "session already live",
			env:    "1",
			bridge: liveBridge(t, "ext-1"),
			msg: protocol.SessionOpen{
				SessionID: "ses-1", ExternalSessionID: "ext-1", CWD: t.TempDir(),
			},
		},
		{
			name:   "session live through an old plugin's outbox",
			env:    "1",
			bridge: outboxBridge(t, "ext-1", time.Now()),
			msg: protocol.SessionOpen{
				SessionID: "ses-1", ExternalSessionID: "ext-1", CWD: t.TempDir(),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AA_ALLOW_SPAWN", tc.env)
			c := testClient(tc.bridge)
			spawn, calls := spyOnSpawns(t)
			c.openSession(tc.msg)
			if *calls != 0 || spawn.called {
				t.Fatalf("refused request started %d terminals, want none", *calls)
			}
		})
	}
}

func TestFormatCommandQuotesOnlyWhenNeeded(t *testing.T) {
	got := formatCommand("cmd.exe", []string{"/c", "start", "", "/D", `C:\My Projects`, "cmd.exe", "/k", "claude"})
	want := `cmd.exe /c start "" /D "C:\\My Projects" cmd.exe /k claude`
	if got != want {
		t.Fatalf("formatCommand = %q, want %q", got, want)
	}
}

func TestSafeRuntimeSessionIDRejectsOptionLikeIDs(t *testing.T) {
	for _, id := range []string{"--dangerously-skip-permissions", "-resume", "-", ".hidden", ""} {
		if safeRuntimeSessionID(id) {
			t.Fatalf("safeRuntimeSessionID(%q) = true, want false", id)
		}
	}
	for _, id := range []string{"6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6", "ext-1", "A_b.c", "0"} {
		if !safeRuntimeSessionID(id) {
			t.Fatalf("safeRuntimeSessionID(%q) = false, want true", id)
		}
	}
}

// A conversation opened into a fresh terminal gets a message before its plugin
// can possibly boot. The connector must pin that conversation's cursor to the
// queue head first, or a later global-cursor advance can make the plugin skip
// the message entirely.
func TestSpawnRequestPrimesTheNewSessionQueueCursor(t *testing.T) {
	t.Setenv("AA_ALLOW_SPAWN", "1")
	dir := t.TempDir()
	b := bridge.New(dir)
	c := testClient(b)
	cwd := t.TempDir()

	spawned := make(chan struct{}, 1)
	previous := startSessionTerminalProcess
	startSessionTerminalProcess = func(_, _, _ string, _ bool) (string, error) {
		spawned <- struct{}{}
		return "fake-claude", nil
	}
	t.Cleanup(func() { startSessionTerminalProcess = previous })

	if err := os.MkdirAll(b.QueueDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.QueueDir(), "4.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	raw, err := json.Marshal(protocol.SessionOpen{
		SessionID:         "ses-new",
		ExternalSessionID: "ext-new",
		CWD:               cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.handleSessionOpen(raw)
	select {
	case <-spawned:
	case <-time.After(time.Second):
		t.Fatal("session.open did not reach the launcher")
	}

	// The turn watcher would poll until its pickup deadline; a short context
	// leaves the queued file in place so the test can inspect it.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c.runTurn(ctx, protocol.Submit{
		RequestID:         "msg-1",
		SessionID:         "ses-new",
		ExternalSessionID: "ext-new",
		Text:              "hello",
	})

	cursor, err := os.ReadFile(filepath.Join(dir, "cursor.ext-new.txt"))
	if err != nil {
		t.Fatalf("the new session's cursor was not seeded: %v", err)
	}
	if strings.TrimSpace(string(cursor)) != "4" {
		t.Fatalf("cursor = %q, want the queue head 4", cursor)
	}
	if _, err := os.Stat(filepath.Join(b.QueueDir(), "5.json")); err != nil {
		t.Fatalf("the message was not queued above the seeded cursor: %v", err)
	}
}

// Starting a terminal can take seconds (the Windows launcher waits for the
// window, macOS waits for osascript). The read loop must not wait: it also
// carries heartbeats and the turn frames that keep the queue moving.
func TestSessionOpenDoesNotBlockTheReadLoop(t *testing.T) {
	t.Setenv("AA_ALLOW_SPAWN", "1")
	c := testClient(nil)
	cwd := t.TempDir()

	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	previous := startSessionTerminalProcess
	startSessionTerminalProcess = func(_, _, _ string, _ bool) (string, error) {
		entered <- struct{}{}
		<-release
		return "fake-claude", nil
	}
	t.Cleanup(func() { startSessionTerminalProcess = previous })

	raw, err := json.Marshal(protocol.SessionOpen{
		SessionID:         "ses-1",
		ExternalSessionID: "ext-1",
		CWD:               cwd,
	})
	if err != nil {
		t.Fatal(err)
	}

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		c.handleSessionOpen(raw)
	}()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("handleSessionOpen waited for the terminal to start")
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("the spawn goroutine never started the launcher")
	}
	close(release)
}

// A snapshot left behind by a plugin that died must not block a terminal
// forever; only a fresh one proves the conversation is still running.
func TestOpenSessionSpawnsOverAStaleOutbox(t *testing.T) {
	t.Setenv("AA_ALLOW_SPAWN", "1")
	b := bridge.New(t.TempDir())
	body, err := json.Marshal(map[string]any{
		"sessionId": "ext-1",
		"status":    "done",
		"ts":        time.Now().Add(-2 * time.Minute).UnixMilli(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.OutboxPath(), body, 0o644); err != nil {
		t.Fatal(err)
	}

	c := testClient(b)
	spawn, calls := spyOnSpawns(t)
	c.openSession(protocol.SessionOpen{
		SessionID:         "ses-1",
		ExternalSessionID: "ext-1",
		CWD:               t.TempDir(),
		Resume:            true,
	})
	if !spawn.called || *calls != 1 {
		t.Fatalf("spawn called %d times over a stale outbox, want 1", *calls)
	}
}

// The server mints an id before it knows the terminal started, so a retry must
// not keep resuming a transcript that was never created.
func TestOpenSessionResumesOnlyAnExistingTranscript(t *testing.T) {
	t.Setenv("AA_ALLOW_SPAWN", "1")
	root := t.TempDir()
	project := filepath.Join(root, "-work")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6"
	if err := os.WriteFile(filepath.Join(project, existing+".jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := testClient(nil)
	c.Discovery = &Discovery{Root: root, cache: map[string]cachedTranscript{}}
	spawn, _ := spyOnSpawns(t)

	c.openSession(protocol.SessionOpen{
		SessionID:         "ses-1",
		ExternalSessionID: existing,
		CWD:               t.TempDir(),
		Resume:            false,
	})
	if !spawn.resume {
		t.Fatal("an existing transcript was created instead of resumed")
	}

	c.openSession(protocol.SessionOpen{
		SessionID:         "ses-2",
		ExternalSessionID: "9b2c5f0e-6b7a-4f0b-9d1e-1234567890ab",
		CWD:               t.TempDir(),
		Resume:            true,
	})
	if spawn.resume {
		t.Fatal("a missing transcript was resumed")
	}
}
