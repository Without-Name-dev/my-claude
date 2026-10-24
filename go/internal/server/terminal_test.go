package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

func terminalTestOutput(seq uint64, text string) protocol.TerminalOutput {
	return protocol.TerminalOutput{
		TerminalID: "trm_1",
		Seq:        seq,
		Data:       base64.StdEncoding.EncodeToString([]byte(text)),
	}
}

func TestTerminalHubOutputIsMonotonic(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 32, bytes: 1 << 20, subBuffer: 8})
	hub.bind("trm_1", "con_1")

	sub, _, _ := hub.subscribe("trm_1", 0)

	hub.publishOutput("con_1", terminalTestOutput(1, "one"))
	hub.publishOutput("con_1", terminalTestOutput(2, "two"))
	// A duplicate and an out-of-order chunk must not reach viewers.
	if hub.publishOutput("con_1", terminalTestOutput(2, "two-again")) != true {
		t.Fatalf("a duplicate was rejected before the sequence check")
	}
	hub.publishOutput("con_1", terminalTestOutput(1, "late"))

	var seqs []uint64
	for len(sub.ch) > 0 {
		var frame struct {
			Type string `json:"type"`
			Seq  uint64 `json:"seq"`
		}
		if err := json.Unmarshal(<-sub.ch, &frame); err != nil {
			t.Fatalf("frame: %v", err)
		}
		if frame.Type != "output" {
			t.Fatalf("type = %q", frame.Type)
		}
		seqs = append(seqs, frame.Seq)
	}
	if len(seqs) != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("seqs = %v, want [1 2]", seqs)
	}
}

func TestTerminalHubReplayWindowTrimsOldest(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 3, bytes: 1 << 20, subBuffer: 8})
	hub.bind("trm_1", "con_1")
	for i := 1; i <= 5; i++ {
		hub.publishOutput("con_1", terminalTestOutput(uint64(i), fmt.Sprintf("chunk%d", i)))
	}

	_, replay, exit := hub.subscribe("trm_1", 0)
	if string(replay.Data) != "chunk3chunk4chunk5" {
		t.Fatalf("replay data = %q", replay.Data)
	}
	if replay.Seq != 5 {
		t.Fatalf("replay seq = %d, want 5", replay.Seq)
	}
	if exit.Exited {
		t.Fatalf("terminal should not be exited")
	}

	_, replay, _ = hub.subscribe("trm_1", 4)
	if string(replay.Data) != "chunk5" || replay.Seq != 5 {
		t.Fatalf("afterSeq replay = %q at %d", replay.Data, replay.Seq)
	}

	_, replay, _ = hub.subscribe("trm_1", 5)
	if len(replay.Data) != 0 || replay.Seq != 5 {
		t.Fatalf("caught-up replay = %q at %d", replay.Data, replay.Seq)
	}

	// A viewer that claims a cursor beyond what the server saw must not have
	// its cursor moved backwards.
	_, replay, _ = hub.subscribe("trm_1", 9)
	if replay.Seq != 9 {
		t.Fatalf("future cursor replay seq = %d, want 9", replay.Seq)
	}
}

func TestTerminalHubReplayByteLimit(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 100, bytes: 12, subBuffer: 8})
	hub.bind("trm_1", "con_1")
	// Each base64 string is 8 characters ("aaaaaaa"), so the window keeps only
	// the newest chunk once total stays above 12 bytes.
	for i := 1; i <= 4; i++ {
		hub.publishOutput("con_1", protocol.TerminalOutput{
			TerminalID: "trm_1",
			Seq:        uint64(i),
			Data:       base64.StdEncoding.EncodeToString([]byte("1234567")),
		})
	}
	_, replay, _ := hub.subscribe("trm_1", 0)
	if string(replay.Data) != "1234567" {
		t.Fatalf("byte-trimmed replay = %q", replay.Data)
	}
	if replay.Seq != 4 {
		t.Fatalf("byte-trimmed seq = %d", replay.Seq)
	}
}

func TestTerminalHubSlowConsumerDropsOldest(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 32, bytes: 1 << 20, subBuffer: 2})
	hub.bind("trm_1", "con_1")
	sub, _, _ := hub.subscribe("trm_1", 0)

	for i := 1; i <= 5; i++ {
		hub.publishOutput("con_1", terminalTestOutput(uint64(i), fmt.Sprintf("chunk%d", i)))
	}

	if dropped := sub.dropped.Load(); dropped != 3 {
		t.Fatalf("dropped = %d, want 3", dropped)
	}
	var seqs []uint64
	for len(sub.ch) > 0 {
		var frame struct {
			Seq uint64 `json:"seq"`
		}
		_ = json.Unmarshal(<-sub.ch, &frame)
		seqs = append(seqs, frame.Seq)
	}
	if len(seqs) != 2 || seqs[0] != 4 || seqs[1] != 5 {
		t.Fatalf("surviving seqs = %v, want [4 5]", seqs)
	}
}

func TestTerminalHubRejectsForeignConnector(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 32, bytes: 1 << 20, subBuffer: 8})
	hub.bind("trm_1", "con_1")

	if hub.publishOutput("con_2", terminalTestOutput(1, "stolen")) {
		t.Fatalf("output from a foreign connector was accepted")
	}
	_, replay, _ := hub.subscribe("trm_1", 0)
	if len(replay.Data) != 0 || replay.Seq != 0 {
		t.Fatalf("foreign output reached the replay buffer: %q", replay.Data)
	}
}

func TestTerminalHubExitIsVisibleToLateViewers(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 32, bytes: 1 << 20, subBuffer: 8})
	hub.bind("trm_1", "con_1")
	hub.publishOutput("con_1", terminalTestOutput(1, "bye"))

	code := 7
	hub.publishExit("trm_1", &code, "exit")

	sub, replay, exit := hub.subscribe("trm_1", 1)
	if string(replay.Data) != "" {
		t.Fatalf("replay = %q", replay.Data)
	}
	if !exit.Exited || exit.ExitCode == nil || *exit.ExitCode != 7 || exit.Reason != "exit" {
		t.Fatalf("exit state = %+v", exit)
	}
	if len(sub.ch) != 0 {
		t.Fatalf("late viewer should not have a queued live exit, got %d frames", len(sub.ch))
	}

	// A viewer already attached receives the live exit frame.
	sub2, _, _ := hub.subscribe("trm_1", 1)
	hub.publishExit("trm_1", &code, "exit")
	select {
	case frame := <-sub2.ch:
		var decoded struct {
			Type     string `json:"type"`
			ExitCode *int   `json:"exitCode"`
		}
		if err := json.Unmarshal(frame, &decoded); err != nil {
			t.Fatalf("exit frame: %v", err)
		}
		if decoded.Type != "exit" || decoded.ExitCode == nil || *decoded.ExitCode != 7 {
			t.Fatalf("exit frame = %s", frame)
		}
	default:
		t.Fatalf("attached viewer did not receive exit")
	}
}

func terminalTestState(seq uint64, text string) protocol.TerminalState {
	return protocol.TerminalState{
		TerminalID: "trm_1",
		Running:    true,
		Seq:        seq,
		Data:       base64.StdEncoding.EncodeToString([]byte(text)),
	}
}

func TestTerminalHubSnapshotRebuildsReplay(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 32, bytes: 1 << 20, subBuffer: 8})
	hub.bind("trm_1", "con_1")

	// A fresh server (for example after a restart) has no replay. The
	// connector's snapshot must become the replay window.
	if !hub.applyState("con_1", terminalTestState(4, "history")) {
		t.Fatalf("snapshot was rejected")
	}
	_, replay, _ := hub.subscribe("trm_1", 0)
	if string(replay.Data) != "history" || replay.Seq != 4 {
		t.Fatalf("replay = %q at %d", replay.Data, replay.Seq)
	}

	// Output newer than the snapshot keeps appending to it.
	hub.publishOutput("con_1", terminalTestOutput(5, "!"))
	_, replay, _ = hub.subscribe("trm_1", 0)
	if string(replay.Data) != "history!" || replay.Seq != 5 {
		t.Fatalf("replay after output = %q at %d", replay.Data, replay.Seq)
	}
}

func TestTerminalHubSnapshotFansOutReplay(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 32, bytes: 1 << 20, subBuffer: 8})
	hub.bind("trm_1", "con_1")
	sub, _, _ := hub.subscribe("trm_1", 0)

	hub.applyState("con_1", terminalTestState(3, "again"))
	select {
	case frame := <-sub.ch:
		var decoded struct {
			Type string `json:"type"`
			Data string `json:"data"`
			Seq  uint64 `json:"seq"`
		}
		if err := json.Unmarshal(frame, &decoded); err != nil {
			t.Fatalf("frame: %v", err)
		}
		if decoded.Type != "replay" || decoded.Seq != 3 || decoded.Data != terminalTestState(3, "again").Data {
			t.Fatalf("frame = %s", frame)
		}
	default:
		t.Fatalf("attached viewer did not receive the snapshot")
	}
}

func TestTerminalHubStaleSnapshotDoesNotRewind(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 32, bytes: 1 << 20, subBuffer: 8})
	hub.bind("trm_1", "con_1")
	hub.publishOutput("con_1", terminalTestOutput(5, "chunk5"))

	if hub.applyState("con_1", terminalTestState(3, "old")) {
		t.Fatalf("a snapshot older than the cursor was applied")
	}
	_, replay, _ := hub.subscribe("trm_1", 0)
	if string(replay.Data) != "chunk5" || replay.Seq != 5 {
		t.Fatalf("replay = %q at %d", replay.Data, replay.Seq)
	}

	// A newer snapshot replaces the window, healing output lost while the
	// connector link was down.
	if !hub.applyState("con_1", terminalTestState(7, "healed")) {
		t.Fatalf("a newer snapshot was rejected")
	}
	_, replay, _ = hub.subscribe("trm_1", 0)
	if string(replay.Data) != "healed" || replay.Seq != 7 {
		t.Fatalf("replay after snapshot = %q at %d", replay.Data, replay.Seq)
	}
}

func TestTerminalHubProbeWaitersAllResolve(t *testing.T) {
	hub := newTerminalHubWithLimits(terminalReplayLimits{chunks: 4, bytes: 1024, subBuffer: 8})
	first := hub.expectState("trm_1")
	second := hub.expectState("trm_1")
	hub.resolveState(protocol.TerminalState{TerminalID: "trm_1", Running: true, Seq: 9})

	for name, waiter := range map[string]chan protocol.TerminalState{"first": first, "second": second} {
		select {
		case state := <-waiter:
			if !state.Running || state.Seq != 9 {
				t.Fatalf("%s waiter state = %+v", name, state)
			}
		default:
			t.Fatalf("%s waiter was not resolved", name)
		}
	}
	// The registry must not retain the waiters after resolving.
	hub.mu.Lock()
	left := len(hub.stateWaiters)
	hub.mu.Unlock()
	if left != 0 {
		t.Fatalf("stateWaiters has %d entries after resolve", left)
	}
}

func TestTicketScopeTerminalBinding(t *testing.T) {
	manager := NewTicketManager()
	scope := TicketScope{ConnectorID: "con_1", TerminalID: "trm_1"}

	token, _ := manager.Issue("usr_1", "client", scope)
	if _, ok := manager.Consume(token, TicketScope{ConnectorID: "con_1", TerminalID: "trm_2"}); ok {
		t.Fatalf("a ticket for one terminal unlocked another")
	}

	token, _ = manager.Issue("usr_1", "client", scope)
	ticket, ok := manager.Consume(token, scope)
	if !ok || ticket.UserID != "usr_1" {
		t.Fatalf("terminal ticket did not redeem: %+v, %v", ticket, ok)
	}

	// Session tickets must keep working after the scope grew.
	sessionToken, _ := manager.Issue("usr_2", "client", TicketScope{SessionID: "ses_1"})
	if _, ok := manager.Consume(sessionToken, TicketScope{SessionID: "ses_1"}); !ok {
		t.Fatalf("session ticket stopped working")
	}
}

func TestResolveTerminalCWD(t *testing.T) {
	root := "/work"
	absolute := "/absolute"
	if runtime.GOOS == "windows" {
		root = `C:\work`
		absolute = `C:\absolute`
	}
	cases := []struct{ root, cwd, want string }{
		{root, "", root},
		{root, "sub", filepath.Join(root, "sub")},
		{root, absolute, absolute},
		{"", "relative", "relative"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := resolveTerminalCWD(tc.root, tc.cwd); got != tc.want {
			t.Fatalf("resolveTerminalCWD(%q, %q) = %q, want %q", tc.root, tc.cwd, got, tc.want)
		}
	}
}

func TestClampTerminalDimensions(t *testing.T) {
	cases := []struct{ cols, rows, wantCols, wantRows int }{
		{0, 0, 80, 24},
		{-1, -5, 80, 24},
		{1000, 1000, 500, 200},
		{120, 40, 120, 40},
	}
	for _, tc := range cases {
		cols, rows := clampTerminalDimensions(tc.cols, tc.rows)
		if cols != tc.wantCols || rows != tc.wantRows {
			t.Fatalf("clamp(%d,%d) = %d,%d", tc.cols, tc.rows, cols, rows)
		}
	}
}
