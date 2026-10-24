package bridge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
