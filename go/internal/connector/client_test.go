package connector

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/bridge"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

func testClient(b *bridge.Bridge) *Client {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return NewClient("ws://example.invalid/api/v2/connector/ws", "token", "test", b, log)
}

// A stop that lands while the message still waits behind another turn is
// remembered for that message instead of being sent to whatever turn the
// plugin happens to be running.
func TestAbortForAQueuedMessageCancelsThatMessage(t *testing.T) {
	c := testClient(nil)
	c.noteQueued("sess-a")

	if _, queued, ok := c.abortTarget("sess-a", ""); !ok || !queued {
		t.Fatalf("queued abort: queued=%v ok=%v, want true true", queued, ok)
	}
	if !c.consumeAbort("sess-a") {
		t.Fatal("the queued cancel was not recorded")
	}
	if c.consumeAbort("sess-a") {
		t.Fatal("one cancel was delivered to two messages")
	}
	c.finishQueued("sess-a")
	if _, _, ok := c.abortTarget("sess-a", ""); ok {
		t.Fatal("an abort is still routed after the message left the queue")
	}
}

// While a turn runs, an abort goes to the plugin with the runtime id, and a
// server that sends the id itself wins over the connector's mapping.
func TestAbortForTheRunningTurnCarriesTheRuntimeID(t *testing.T) {
	c := testClient(nil)
	c.noteQueued("sess-b")
	c.setActive("sess-b", "runtime-b")

	target, queued, ok := c.abortTarget("sess-b", "")
	if !ok || queued || target != "runtime-b" {
		t.Fatalf("abort target = %q queued=%v ok=%v, want runtime-b", target, queued, ok)
	}
	if target, _, _ := c.abortTarget("sess-b", "runtime-from-frame"); target != "runtime-from-frame" {
		t.Fatalf("framed runtime id ignored: %q", target)
	}
	if _, _, ok := c.abortTarget("other-session", ""); ok {
		t.Fatal("an abort for a session this connector never accepted was routed")
	}
}

// A message the bridge cannot hand over must be reported to the server: the
// phone already shows the person's bubble and would otherwise wait forever.
func TestRunTurnReportsASubmitThatNeverReachedThePlugin(t *testing.T) {
	dir := t.TempDir()
	b := bridge.New(dir)
	if err := os.WriteFile(b.OutboxPath(), []byte(`{"turnId":`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := testClient(b)
	c.runTurn(context.Background(), protocol.Submit{
		RequestID: "msg-9",
		SessionID: "sess-c",
		Text:      "hello",
	})

	c.endMu.Lock()
	defer c.endMu.Unlock()
	if len(c.pendingEnds) != 1 {
		t.Fatalf("queued %d final frames, want 1", len(c.pendingEnds))
	}
	end := c.pendingEnds[0]
	if end.Status != "error" || end.SessionID != "sess-c" || end.TurnID != "msg-9" {
		t.Fatalf("unexpected failure frame %+v", end)
	}
}

// Every turn that finishes while the link is down is replayed, in order; one
// slot would lose all but the last answer.
func TestPendingTurnEndsAreKeptInOrder(t *testing.T) {
	c := testClient(nil)
	for _, id := range []string{"turn-1", "turn-2", "turn-3"} {
		c.TurnEnd(protocol.TurnEnd{SessionID: "sess", TurnID: id, Status: "done", Text: id})
	}

	c.endMu.Lock()
	defer c.endMu.Unlock()
	if len(c.pendingEnds) != 3 {
		t.Fatalf("kept %d frames, want 3", len(c.pendingEnds))
	}
	for i, want := range []string{"turn-1", "turn-2", "turn-3"} {
		if c.pendingEnds[i].TurnID != want {
			t.Fatalf("frame %d = %q, want %q", i, c.pendingEnds[i].TurnID, want)
		}
	}
}
