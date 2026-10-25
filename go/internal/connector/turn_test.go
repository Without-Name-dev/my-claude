package connector

import (
	"context"
	"os"
	"testing"
	"time"

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

// turnSink records the frames a watcher emits, so a test can wait for them.
type turnSink struct {
	deltas chan protocol.TurnDelta
	ends   chan protocol.TurnEnd
}

func newTurnSink() *turnSink {
	return &turnSink{
		deltas: make(chan protocol.TurnDelta, 16),
		ends:   make(chan protocol.TurnEnd, 4),
	}
}

func (s *turnSink) TurnBegin(protocol.TurnBegin)         {}
func (s *turnSink) AgentActivity(protocol.AgentActivity) {}
func (s *turnSink) ToolBegin(protocol.ToolBegin)         {}
func (s *turnSink) ToolEnd(protocol.ToolEnd)             {}
func (s *turnSink) TurnDelta(msg protocol.TurnDelta)     { s.deltas <- msg }
func (s *turnSink) TurnEnd(msg protocol.TurnEnd)         { s.ends <- msg }

// A turn longer than the plugin's tail buffer must still end with the whole
// answer, not with the fragment the final snapshot happened to hold.
func TestRunEndsWithTheFullTextAfterTheHeadIsDropped(t *testing.T) {
	b := bridge.New(t.TempDir())
	writeOutbox := func(body string) {
		if err := os.WriteFile(b.OutboxPath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeOutbox(`{"sessionId":"runtime-sess","turnId":"turn-1","status":"streaming","text":"abcdef","ts":1000}`)

	sink := newTurnSink()
	w := NewTurnWatcher(b, sink, "sess", "runtime-sess", "msg-1", &bridge.Submission{QueuedAt: 500})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	select {
	case <-sink.deltas:
	case <-time.After(3 * time.Second):
		t.Fatal("watcher never reported the first text")
	}

	writeOutbox(`{"sessionId":"runtime-sess","turnId":"turn-1","status":"done","text":"defghij","ts":2000}`)
	select {
	case msg := <-sink.ends:
		if msg.Status != "done" || msg.Text != "abcdefghij" {
			t.Fatalf("turn.end = %+v, want the full answer", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watcher never closed the turn")
	}
	<-done
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

// agentSink records the subagent snapshots the watcher emits.
type agentSink struct {
	activities []protocol.AgentActivity
}

func (s *agentSink) TurnBegin(protocol.TurnBegin) {}
func (s *agentSink) TurnDelta(protocol.TurnDelta) {}
func (s *agentSink) TurnEnd(protocol.TurnEnd)     {}
func (s *agentSink) ToolBegin(protocol.ToolBegin) {}
func (s *agentSink) ToolEnd(protocol.ToolEnd)     {}
func (s *agentSink) AgentActivity(msg protocol.AgentActivity) {
	s.activities = append(s.activities, msg)
}

func TestEmitAgentsCarriesLiveActivityAndForwardsChanges(t *testing.T) {
	b := bridge.New(t.TempDir())
	writeAgents := func(body string) {
		if err := os.WriteFile(b.AgentsPath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sink := &agentSink{}
	w := NewTurnWatcher(b, sink, "sess", "runtime-sess", "msg-1", nil)

	writeAgents(`{"sessionId":"runtime-sess","entries":[
		{"agentId":"a1","label":"Explore","status":"running","startedAt":1,"updatedAt":2,
		 "tool":"Bash","detail":"go test ./...","text":"checking the packages"}
	]}`)
	w.emitAgents("turn-1")
	if len(sink.activities) != 1 || len(sink.activities[0].Agents) != 1 {
		t.Fatalf("activities = %+v, want one agent", sink.activities)
	}
	got := sink.activities[0].Agents[0]
	if got.Tool != "Bash" || got.Detail != "go test ./..." || got.Text != "checking the packages" {
		t.Fatalf("agent = %+v, want tool, detail and text passed through", got)
	}

	// Only the streamed text changed: the phone must still see the update.
	writeAgents(`{"sessionId":"runtime-sess","entries":[
		{"agentId":"a1","label":"Explore","status":"running","startedAt":1,"updatedAt":3,
		 "tool":"Bash","detail":"go test ./...","text":"checking the packages; all pass"}
	]}`)
	w.emitAgents("turn-1")
	if len(sink.activities) != 2 {
		t.Fatalf("activities = %d, want a frame for changed text", len(sink.activities))
	}
	if got := sink.activities[1].Agents[0]; got.Text != "checking the packages; all pass" {
		t.Fatalf("changed agent text = %q", got.Text)
	}

	// An unchanged snapshot is not noise.
	w.emitAgents("turn-1")
	if len(sink.activities) != 2 {
		t.Fatalf("activities = %d, want no frame for an unchanged snapshot", len(sink.activities))
	}
}

func TestEmitToolsClosesAToolFirstSeenWhileRunning(t *testing.T) {
	b := bridge.New(t.TempDir())
	writeTools := func(body string) {
		if err := os.WriteFile(b.ToolsPath(), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sink := &toolSink{}
	w := NewTurnWatcher(b, sink, "sess", "runtime-sess", "msg-1", nil)

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
