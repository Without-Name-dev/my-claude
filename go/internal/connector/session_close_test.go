package connector

import (
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/bridge"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// closedWatcher builds a watcher whose bridge shows a running turn.
func closedWatcher(t *testing.T, closed func() bool) (*TurnWatcher, *turnSink) {
	t.Helper()
	b := bridge.New(t.TempDir())
	writeBridgeFiles(t, b, true,
		`{"sessionId":"runtime-sess","turnId":"turn-1","status":"streaming","text":"working","ts":1000}`)
	sink := newTurnSink()
	w := NewTurnWatcher(b, sink, "sess", "runtime-sess", "msg-1", &bridge.Submission{QueuedAt: 500})
	w.Closed = closed
	return w, sink
}

// Closing the terminal must end the phone's turn at once: nothing will ever
// finish it, and waiting for the heartbeat to expire would leave a spinner.
func TestAClosedTerminalEndsTheTurnImmediately(t *testing.T) {
	w, sink := closedWatcher(t, func() bool { return true })
	end := runWatcher(t, w, sink, time.Second)
	if end == nil {
		t.Fatal("the watcher kept running after the terminal was closed")
	}
	if end.Status != "aborted" || end.TurnID != "turn-1" {
		t.Fatalf("turn.end = %+v, want the running turn aborted", end)
	}
}

func TestAnOpenTerminalIsNotEndedByTheCloseHook(t *testing.T) {
	w, sink := closedWatcher(t, func() bool { return false })
	if end := runWatcher(t, w, sink, 400*time.Millisecond); end != nil {
		t.Fatalf("a running turn was ended without a close: %+v", end)
	}
}

func TestCloseSessionMarksTheSessionAndWithdrawsPresence(t *testing.T) {
	previous := closeSessionProcessFunc
	t.Cleanup(func() { closeSessionProcessFunc = previous })
	killedFor := ""
	closeSessionProcessFunc = func(externalID string) (int, error) {
		killedFor = externalID
		return 1, nil
	}

	b := bridge.New(t.TempDir())
	client := NewClient("ws://unused/api/v2/connector/ws", "token", "test", b,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	const external = "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6"
	if err := os.MkdirAll(b.LiveDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	stamp := b.LiveDir() + string(os.PathSeparator) + external + ".txt"
	if err := os.WriteFile(stamp, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(protocol.SessionClose{SessionID: "sess-1", ExternalSessionID: external})
	client.handleSessionClose(raw)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && killedFor != external {
		time.Sleep(10 * time.Millisecond)
	}
	if killedFor != external {
		t.Fatalf("killed = %q, want the runtime session id", killedFor)
	}
	if !client.sessionWasClosed("sess-1") {
		t.Fatal("the close was not recorded for the turn watcher")
	}
	if _, err := os.Stat(stamp); !os.IsNotExist(err) {
		t.Fatalf("presence stamp still exists (err %v)", err)
	}
	if len(b.LiveSessions(time.Minute)) != 0 {
		t.Fatal("the closed session is still reported live")
	}

	// A new message is a request to run again, so the mark must not survive it.
	client.clearSessionClosed("sess-1")
	if client.sessionWasClosed("sess-1") {
		t.Fatal("a close outlived the next message")
	}
}

func TestCloseSessionRefusesAnUnsafeRuntimeID(t *testing.T) {
	previous := closeSessionProcessFunc
	t.Cleanup(func() { closeSessionProcessFunc = previous })
	called := false
	closeSessionProcessFunc = func(string) (int, error) { called = true; return 0, nil }

	client := NewClient("ws://unused/api/v2/connector/ws", "token", "test", bridge.New(t.TempDir()),
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	raw, _ := json.Marshal(protocol.SessionClose{SessionID: "sess-1", ExternalSessionID: "; rm -rf /"})
	client.handleSessionClose(raw)
	time.Sleep(100 * time.Millisecond)
	if called {
		t.Fatal("an unsafe runtime id reached the process killer")
	}
	if client.sessionWasClosed("sess-1") {
		t.Fatal("a refused close was still recorded")
	}
}
