package connector

import (
	"os"
	"path/filepath"
	"testing"
)

func writeHistoryFile(t *testing.T, lines []string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "s.jsonl")
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Claude Code writes one assistant message as several records that share a
// message id, each holding a single block. Reading them individually loses the
// text entirely, which made every historical conversation open empty.
func TestReadHistoryMergesBlockPerRecordMessages(t *testing.T) {
	path := writeHistoryFile(t, []string{
		`{"type":"user","uuid":"u1","timestamp":"2026-10-24T08:00:00.000Z","message":{"role":"user","content":"怎么修登录"}}`,
		`{"type":"assistant","uuid":"a1","timestamp":"2026-10-24T08:00:01.000Z","message":{"id":"msg-1","role":"assistant","content":[{"type":"thinking","thinking":"hmm"}]}}`,
		`{"type":"assistant","uuid":"a2","timestamp":"2026-10-24T08:00:02.000Z","message":{"id":"msg-1","role":"assistant","content":[{"type":"text","text":"先看登录接口"}]}}`,
		`{"type":"assistant","uuid":"a3","timestamp":"2026-10-24T08:00:03.000Z","message":{"id":"msg-1","role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"Bash","input":{"command":"grep login"}}]}}`,
		`{"type":"user","uuid":"u2","timestamp":"2026-10-24T08:00:04.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","content":"found it"}]}}`,
	})

	items, err := ReadHistory(path, 100)
	if err != nil {
		t.Fatalf("ReadHistory: %v", err)
	}
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3 (user, assistant, tool): %+v", len(items), items)
	}
	if items[0].Role != "user" || items[0].Text != "怎么修登录" {
		t.Fatalf("first item = %+v", items[0])
	}
	if items[1].Role != "assistant" || items[1].Text != "先看登录接口" {
		t.Fatalf("assistant item = %+v, want the merged text", items[1])
	}
	if items[2].Type != "tool" || items[2].Tool != "Bash" || len(items[2].Output) == 0 {
		t.Fatalf("tool item = %+v, want the result attached", items[2])
	}
}

func TestReadHistorySeparatesConsecutiveMessages(t *testing.T) {
	path := writeHistoryFile(t, []string{
		`{"type":"assistant","uuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"第一句"}]}}`,
		`{"type":"assistant","uuid":"a2","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"第二句"}]}}`,
		`{"type":"assistant","uuid":"a3","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"另一个消息"}]}}`,
	})
	items, _ := ReadHistory(path, 100)
	if len(items) != 2 {
		t.Fatalf("got %d items, want 2", len(items))
	}
	if items[0].Text != "第一句\n\n第二句" {
		t.Fatalf("first = %q", items[0].Text)
	}
}
