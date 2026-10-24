//go:build windows && e2e

package connector

import (
	"strings"
	"testing"
	"time"
)

// TestWindowsConPTYSmoke drives the raw ConPTY implementation without the
// server or the link. It is the fastest way to tell a platform bug from a
// protocol bug.
func TestWindowsConPTYSmoke(t *testing.T) {
	proc, err := startTerminal("", "", 80, 24)
	if err != nil {
		t.Fatalf("startTerminal: %v", err)
	}
	defer func() {
		_ = proc.Kill()
		_ = proc.Close()
		proc.Release()
	}()
	t.Logf("pid=%d", proc.PID())

	read := make(chan []byte, 32)
	readErr := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := proc.Read(buf)
			if n > 0 {
				chunk := make([]byte, n)
				copy(chunk, buf[:n])
				read <- chunk
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()

	time.Sleep(700 * time.Millisecond)
	const token = "conpty-smoke-ok"
	if _, err := proc.Write([]byte("echo " + token + "\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	var out strings.Builder
	deadline := time.After(15 * time.Second)
	// The token appears once as the echoed command line and once as the
	// command's output; requiring both proves the shell actually ran it.
	for strings.Count(out.String(), token) < 2 {
		select {
		case chunk := <-read:
			out.Write(chunk)
		case err := <-readErr:
			t.Fatalf("read: %v after %q", err, out.String())
		case <-deadline:
			t.Fatalf("timed out; output so far: %q", out.String())
		}
	}
	t.Logf("ConPTY output: %q", out.String())
}
