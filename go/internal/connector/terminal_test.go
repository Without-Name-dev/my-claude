package connector

import (
	"bytes"
	"encoding/base64"
	"path/filepath"
	"runtime"
	"testing"
)

func TestTerminalLinkURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{
			"ws://127.0.0.1:8911/api/v2/connector/ws",
			"ws://127.0.0.1:8911/api/v2/connector/terminal-link",
		},
		{
			"wss://example.com/api/v2/connector/ws",
			"wss://example.com/api/v2/connector/terminal-link",
		},
		{
			"wss://example.com/prefix/api/v2/connector/ws/",
			"wss://example.com/prefix/api/v2/connector/terminal-link",
		},
		{
			"ws://127.0.0.1:8911/api/v2/connector/ws?token=ignored",
			"ws://127.0.0.1:8911/api/v2/connector/terminal-link",
		},
	}
	for _, tc := range cases {
		got, err := terminalLinkURL(tc.in)
		if err != nil {
			t.Fatalf("terminalLinkURL(%q): %v", tc.in, err)
		}
		if got != tc.want {
			t.Fatalf("terminalLinkURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLocalTerminalStateCarriesScrollback(t *testing.T) {
	lt := &localTerminal{id: "trm_1"}
	lt.mu.Lock()
	lt.seq = 1
	lt.appendScrollbackLocked([]byte("one"))
	lt.seq = 2
	lt.appendScrollbackLocked([]byte("two"))
	lt.mu.Unlock()

	state := lt.state()
	if !state.Running || state.TerminalID != "trm_1" || state.Seq != 2 {
		t.Fatalf("state = %+v", state)
	}
	raw, err := base64.StdEncoding.DecodeString(state.Data)
	if err != nil {
		t.Fatalf("snapshot is not base64: %v", err)
	}
	if string(raw) != "onetwo" {
		t.Fatalf("snapshot = %q, want %q", raw, "onetwo")
	}
}

func TestLocalTerminalSnapshotTrimsOldestBytes(t *testing.T) {
	lt := &localTerminal{id: "trm_1"}
	big := bytes.Repeat([]byte("a"), terminalScrollbackBytes/2+1)
	lt.mu.Lock()
	lt.appendScrollbackLocked(big)
	lt.appendScrollbackLocked(big)
	lt.seq = 2
	lt.mu.Unlock()

	state := lt.state()
	raw, err := base64.StdEncoding.DecodeString(state.Data)
	if err != nil {
		t.Fatalf("snapshot is not base64: %v", err)
	}
	if len(raw) != len(big) {
		t.Fatalf("snapshot kept %d bytes, want the newest %d", len(raw), len(big))
	}
	if state.Seq != 2 {
		t.Fatalf("snapshot seq = %d, want 2", state.Seq)
	}
}

func TestResolveTerminalWorkingDir(t *testing.T) {
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
		if got := resolveTerminalWorkingDir(tc.root, tc.cwd); got != tc.want {
			t.Fatalf("resolveTerminalWorkingDir(%q, %q) = %q, want %q",
				tc.root, tc.cwd, got, tc.want)
		}
	}
}

func TestClampTerminalSize(t *testing.T) {
	cases := []struct{ cols, rows, wantCols, wantRows int }{
		{0, 0, 80, 24},
		{500, 200, 500, 200},
		{900, 900, 500, 200},
		{1, 1, 1, 1},
	}
	for _, tc := range cases {
		cols, rows := clampTerminalSize(tc.cols, tc.rows)
		if cols != tc.wantCols || rows != tc.wantRows {
			t.Fatalf("clampTerminalSize(%d,%d) = %d,%d", tc.cols, tc.rows, cols, rows)
		}
	}
}
