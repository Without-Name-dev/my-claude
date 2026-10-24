package connector

import (
	"os"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/bridge"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

func TestAdvanceReportsOnlyNewText(t *testing.T) {
	cases := []struct {
		name string
		prev string
		next string
		want string
	}{
		{"first chunk", "", "hello", "hello"},
		{"extension", "hello", "hello world", " world"},
		{"unchanged", "hello", "hello", ""},
		{"head dropped", "abcdefgh", "defghij", "ij"},
		{"no overlap", "abc", "xyz", "xyz"},
		{"shorter with overlap", "abcdef", "efg", "g"},
		{"only truncation", "abcdef", "cdef", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := advance(tc.prev, tc.next); got != tc.want {
				t.Fatalf("advance(%q, %q) = %q, want %q", tc.prev, tc.next, got, tc.want)
			}
		})
	}
}

func TestAdvanceRebuildsTextAfterTruncation(t *testing.T) {
	// A turn longer than the plugin's 20,000 character tail: the head disappears
	// and the remaining text must still arrive without duplication.
	head := "0123456789"
	full := head + head + head
	tail := full[len(head):]

	var got string
	prev := ""
	for _, step := range []string{head, full, tail} {
		got += advance(prev, step)
		prev = step
	}
	if got != full {
		t.Fatalf("rebuilt text = %q, want %q", got, full)
	}
}

// toolSink records tool events for the watcher tests.
type toolSink struct {
	begins []string
	ends   []string
	oks    []bool
}

func (s *toolSink) TurnBegin(protocol.TurnBegin)         {}
func (s *toolSink) TurnDelta(protocol.TurnDelta)         {}
func (s *toolSink) TurnEnd(protocol.TurnEnd)             {}
func (s *toolSink) AgentActivity(protocol.AgentActivity) {}
func (s *toolSink) ToolBegin(msg protocol.ToolBegin)     { s.begins = append(s.begins, msg.ToolUseID) }
func (s *toolSink) ToolEnd(msg protocol.ToolEnd) {
	s.ends = append(s.ends, msg.ToolUseID)
	s.oks = append(s.oks, msg.OK)
}

func TestEmitToolsClosesAToolFirstSeenWhileRunning(t *testing.T) {
	b := bridge.New(t.TempDir())
	writeTools := func(body string) {
		if err := os.WriteFile(b.ToolsPath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sink := &toolSink{}
	w := NewTurnWatcher(b, sink, "sess", "runtime-sess", nil)

	writeTools(`{"sessionId":"runtime-sess","entries":[
		{"id":"tool-1","tool":"Read","input":{"file_path":"x"},"done":false}
	]}`)
	w.emitTools("turn-1")
	if len(sink.begins) != 1 || len(sink.ends) != 0 {
		t.Fatalf("running tool: begins=%v ends=%v", sink.begins, sink.ends)
	}

	writeTools(`{"sessionId":"runtime-sess","entries":[
		{"id":"tool-1","tool":"Read","input":{"file_path":"x"},"done":true,"output":"ok"}
	]}`)
	w.emitTools("turn-1")
	if len(sink.ends) != 1 || sink.ends[0] != "tool-1" || !sink.oks[0] {
		t.Fatalf("finished tool: ends=%v oks=%v", sink.ends, sink.oks)
	}

	writeTools(`{"sessionId":"other-session","entries":[
		{"id":"tool-2","tool":"Bash","done":true}
	]}`)
	w.emitTools("turn-1")
	if len(sink.begins) != 1 || len(sink.ends) != 1 {
		t.Fatalf("another session's tool leaked: begins=%v ends=%v", sink.begins, sink.ends)
	}
}
