package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSubmitWritesOneCompleteQueueFile(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)

	sub, err := b.Submit("sess-1", "", "msg-1", "hello", "prompt")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if sub.PreviousTurnID != "" {
		t.Fatalf("previous turn = %q, want empty", sub.PreviousTurnID)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "queue"))
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "1.json" {
		t.Fatalf("queue = %v, want [1.json]", entries)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "queue", "1.json"))
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var msg QueueMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("queue file is not valid JSON: %v", err)
	}
	if msg.Text != "hello" || msg.Action != "prompt" || msg.SessionID != "sess-1" {
		t.Fatalf("unexpected message %+v", msg)
	}
	if msg.Token == "" {
		t.Fatal("queued message has no token")
	}
}

func TestSubmitCarriesPreviousTurnSoStaleOutboxIsIgnored(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.OutboxPath(), []byte(`{"turnId":"old","status":"done","text":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	sub, err := b.Submit("s", "", "m", "next", "prompt")
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if sub.PreviousTurnID != "old" {
		t.Fatalf("previous turn = %q, want old", sub.PreviousTurnID)
	}
}

func TestQueueSequenceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	first := New(dir)
	if _, err := first.Submit("s", "", "a", "one", "prompt"); err != nil {
		t.Fatal(err)
	}

	// A connector restart forgets its counter; the next message must not
	// overwrite the one still sitting in the queue.
	second := New(dir)
	if _, err := second.Submit("s", "", "b", "two", "prompt"); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(filepath.Join(dir, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("queue has %d entries, want 2", len(entries))
	}
}

func TestReadOutboxForwardsOnlyReportedState(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"turnId":"t1","status":"streaming","chunks":3,"text":"hi","ts":1,
	          "model":"opus","cwd":"/tmp","git":null,"costUsd":0.02}`
	if err := os.WriteFile(b.OutboxPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	box, err := b.ReadOutbox()
	if err != nil || box == nil {
		t.Fatalf("ReadOutbox: %v %v", box, err)
	}
	if !box.Running() || box.Chunks != 3 || box.Text != "hi" {
		t.Fatalf("unexpected outbox %+v", box)
	}
	if _, ok := box.State["git"]; ok {
		t.Fatal("a null field should not become state")
	}
	if box.State["model"] != "opus" {
		t.Fatalf("model = %v, want opus", box.State["model"])
	}
}

func TestToolsDecodeThePluginShape(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"sessionId":"s1","entries":[
		{"id":"tool-1","tool":"Read","input":{"file_path":"x"},"done":true,"isError":false,"output":"ok"}
	]}`
	if err := os.WriteFile(b.ToolsPath(), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entries := b.Tools("s1")
	if len(entries) != 1 {
		t.Fatalf("Tools returned %d entries, want 1", len(entries))
	}
	entry := entries[0]
	if entry.ID != "tool-1" || !entry.Done || entry.IsError {
		t.Fatalf("unexpected entry %+v", entry)
	}
	if string(entry.Output) != `"ok"` {
		t.Fatalf("output = %s, want a JSON string", entry.Output)
	}
	if entries := b.Tools("another-session"); entries != nil {
		t.Fatalf("a snapshot from another session was returned: %+v", entries)
	}
}

func TestLiveSessionsKeepsOnlyFreshStamps(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	live := b.LiveDir()
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	files := map[string]string{
		"fresh.txt":  strconv.FormatInt(now, 10),
		"stale.txt":  strconv.FormatInt(now-31_000, 10),
		"empty.txt":  "",
		"broken.txt": "not a stamp",
		"nested":     "ignored",
	}
	for name, body := range files {
		path := filepath.Join(live, name)
		if name == "nested" {
			if err := os.MkdirAll(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := b.LiveSessions(DefaultLiveTTL)
	if len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("LiveSessions = %v, want [fresh]", got)
	}
}

func TestLiveSessionsIsEmptyWithoutADirectory(t *testing.T) {
	b := New(t.TempDir())
	if got := b.LiveSessions(DefaultLiveTTL); len(got) != 0 {
		t.Fatalf("LiveSessions = %v, want none", got)
	}
}

// A stamp left behind by a process that died never moves again. The scan has to
// delete it, or every killed plugin leaves a file that the connector rereads on
// every heartbeat forever.
func TestLiveSessionsExpiresDeadStamps(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if err := os.MkdirAll(b.LiveDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	old := now.Add(-31 * time.Second)
	fresh := filepath.Join(b.LiveDir(), "fresh.txt")
	stale := filepath.Join(b.LiveDir(), "stale.txt")
	empty := filepath.Join(b.LiveDir(), "empty.txt")
	for path, body := range map[string]string{
		fresh: strconv.FormatInt(now.UnixMilli(), 10),
		stale: strconv.FormatInt(old.UnixMilli(), 10),
		empty: "",
	} {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		// The dead files were last written when their stamp says they were.
		if path != fresh {
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	got := b.LiveSessions(DefaultLiveTTL)
	if len(got) != 1 || got[0] != "fresh" {
		t.Fatalf("LiveSessions = %v, want [fresh]", got)
	}
	for _, path := range []string{stale, empty} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s was not expired (err = %v)", filepath.Base(path), err)
		}
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh stamp was removed: %v", err)
	}
}

// Deleting is only safe when the file is old too: a plugin that rewrites the
// same path between the read and the expiry check has a fresh mtime, and its
// live stamp must survive. A stale value with a recent mtime is left in place
// (and is simply not live).
func TestLiveSessionsKeepsRecentlyWrittenStamps(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if err := os.MkdirAll(b.LiveDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.LiveDir(), "rewritten.txt")
	stale := time.Now().Add(-31 * time.Second).UnixMilli()
	if err := os.WriteFile(path, []byte(strconv.FormatInt(stale, 10)), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := b.LiveSessions(DefaultLiveTTL); len(got) != 0 {
		t.Fatalf("LiveSessions = %v, want none", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a recently written stamp was removed: %v", err)
	}
}

func TestRequestAbortCarriesTheSessionOnTheSecondLine(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if err := b.RequestAbort("runtime-session"); err != nil {
		t.Fatalf("RequestAbort: %v", err)
	}
	raw, err := os.ReadFile(b.AbortPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	if len(lines) != 2 {
		t.Fatalf("abort file = %q, want a stamp and a session", raw)
	}
	if _, err := strconv.ParseInt(lines[0], 10, 64); err != nil {
		t.Fatalf("first line is not a stamp: %q", lines[0])
	}
	if lines[1] != "runtime-session" {
		t.Fatalf("session = %q, want runtime-session", lines[1])
	}
}

func TestSubmitCommandCarriesTheRuntimeSession(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if _, err := b.SubmitCommand("sess-1", "runtime-1", "cmd-1", "/compact", "keep the summary"); err != nil {
		t.Fatalf("SubmitCommand: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "queue", "1.json"))
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var msg QueueMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatalf("queue file is not valid JSON: %v", err)
	}
	if msg.ExternalSessionID != "runtime-1" {
		t.Fatalf("external session = %q, want runtime-1", msg.ExternalSessionID)
	}
	if msg.Command != "compact" || msg.Args != "keep the summary" || msg.Action != "command" {
		t.Fatalf("unexpected command %+v", msg)
	}
}

func TestCancelQueuedRemovesOnlyTheNamedMessage(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if _, err := b.Submit("s", "", "keep", "one", "prompt"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Submit("s", "", "drop", "two", "prompt"); err != nil {
		t.Fatal(err)
	}

	if err := b.CancelQueued("drop"); err != nil {
		t.Fatalf("CancelQueued: %v", err)
	}
	entries, err := os.ReadDir(b.QueueDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("queue has %d entries, want 1", len(entries))
	}
	raw, err := os.ReadFile(filepath.Join(b.QueueDir(), entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var msg QueueMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		t.Fatal(err)
	}
	if msg.ID != "keep" {
		t.Fatalf("cancelled the wrong message: %q remains", msg.ID)
	}

	// A message the plugin already consumed, or one never queued, is not an
	// error: there is simply nothing left to withdraw.
	if err := b.CancelQueued("missing"); err != nil {
		t.Fatalf("CancelQueued(missing): %v", err)
	}
	if err := b.CancelQueued(""); err != nil {
		t.Fatalf("CancelQueued(empty): %v", err)
	}
}

func TestSubmitFailsOnAnOutboxItCannotRead(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.OutboxPath(), []byte(`{"turnId":`), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Submit("s", "", "m", "next", "prompt"); err == nil {
		t.Fatal("Submit accepted a torn outbox instead of failing")
	}
	if entries, err := os.ReadDir(b.QueueDir()); err == nil && len(entries) != 0 {
		t.Fatalf("a message was queued despite the unreadable outbox: %v", entries)
	}
}

// The plugin only reads files numbered above its cursor. If the queue is
// emptied and numbering restarts at 1, every new message becomes invisible.
func TestQueueNumberingRespectsPluginCursor(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cursor.txt"), []byte("73"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := New(dir)
	if _, err := b.Submit("s", "", "m", "hello", "prompt"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "74.json" {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("queued %v, want [74.json]", names)
	}
}

// A conversation spawned into a fresh terminal must start reading at the queue
// head, not at the global cursor another session may have advanced past its
// first message. Its own cursor is created only when absent.
func TestSeedSessionCursorPinsANewSessionToTheQueueHead(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if err := os.MkdirAll(b.QueueDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.QueueDir(), "7.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := b.SeedSessionCursor("6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6"); err != nil {
		t.Fatalf("SeedSessionCursor: %v", err)
	}
	for _, name := range []string{
		"cursor.6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6.txt",
		"cursor.6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6.txt.bak",
	} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if strings.TrimSpace(string(raw)) != "7" {
			t.Fatalf("%s = %q, want the queue head 7", name, raw)
		}
	}

	// A live plugin's cursor is never lowered by a later spawn request.
	primary := filepath.Join(dir, "cursor.6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6.txt")
	if err := os.WriteFile(primary, []byte("9"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.SeedSessionCursor("6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6"); err != nil {
		t.Fatalf("SeedSessionCursor again: %v", err)
	}
	raw, err := os.ReadFile(primary)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(raw)) != "9" {
		t.Fatalf("cursor = %q, want the plugin's 9", raw)
	}
}

func TestSeedSessionCursorSanitizesLikeThePlugin(t *testing.T) {
	dir := t.TempDir()
	b := New(dir)
	if err := b.SeedSessionCursor("a/b:c d"); err != nil {
		t.Fatalf("SeedSessionCursor: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "cursor.a_b_c_d.txt")); err != nil {
		t.Fatalf("sanitized cursor file: %v", err)
	}
}
