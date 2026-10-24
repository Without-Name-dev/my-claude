//go:build e2e

// Reattach end-to-end test: create a terminal, close the viewer socket,
// reconnect to the same terminal and verify replay + input still work.
//
//	go test -tags=e2e ./internal/server -run TestTerminalReattachEndToEnd -v
package server_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/auth"
	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/connector"
	"github.com/agents-anywhere/agents-anywhere/internal/server"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

func TestTerminalReattachEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dataDir := t.TempDir()
	t.Setenv("AA_DATA_DIR", dataDir)
	t.Setenv("AA_LISTEN", "127.0.0.1:8921")
	t.Setenv("AA_LOG_LEVEL", "error")

	cfg := config.LoadServer()
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	app := server.New(cfg, db, logger)

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatalf("listen on %s: %v", cfg.Listen, err)
	}
	httpServer := &http.Server{Handler: app.Handler()}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = httpServer.Serve(listener)
	}()
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = httpServer.Shutdown(shutdownCtx)
		<-serveDone
	}()

	baseURL := "http://" + cfg.Listen
	waitForHealthy(t, ctx, baseURL)

	user, err := db.CreateUser(ctx, store.User{
		Email:        "terminal-reattach-e2e@example.com",
		PasswordHash: "not-used",
		DisplayName:  "Terminal Reattach E2E",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	connectorToken := server.NewToken("con")
	connectorRecord, err := db.CreateConnector(ctx, store.Connector{
		UserID:      user.ID,
		Name:        "e2e-reattach-desktop",
		Kind:        "desktop",
		TokenHash:   server.HashToken(connectorToken),
		TokenPrefix: connectorToken[:12],
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	userToken, err := auth.CreateUserAccessToken(user.ID, 3600)
	if err != nil {
		t.Fatalf("mint user token: %v", err)
	}

	client := connector.NewClient(
		"ws://"+cfg.Listen+"/api/v2/connector/ws",
		connectorToken,
		"e2e-reattach-desktop",
		nil,
		logger,
	)
	connectorCtx, stopConnector := context.WithCancel(ctx)
	defer stopConnector()
	go client.Run(connectorCtx)

	waitForConnectorOnline(t, ctx, baseURL, connectorRecord.ID, userToken)

	workspace := t.TempDir()
	createBody := fmt.Sprintf(`{"cols":100,"rows":30,"label":"reattach","cwd":%q}`, workspace)
	createURL := baseURL + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2?root=" + url.QueryEscape(workspace)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, createURL, strings.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Content-Type", "application/json")
	var created struct {
		OK     bool `json:"ok"`
		Result struct {
			TerminalID string `json:"terminalId"`
			Status     string `json:"status"`
			PID        *int   `json:"pid"`
		} `json:"result"`
	}
	if status := doJSON(t, req, &created); status != http.StatusOK {
		t.Fatalf("create terminal status = %d", status)
	}
	terminalID := created.Result.TerminalID
	t.Logf("created terminal %s status=%s pid=%v", terminalID, created.Result.Status, intValue(created.Result.PID))

	streamURL := "ws://" + cfg.Listen + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2/" + url.PathEscape(terminalID) + "/stream?token=" + url.QueryEscape(userToken) + "&fromSeq=0"

	// First attach: run a command and wait for its output.
	ws1, frames1 := dialTerminalStream(t, ctx, streamURL)
	sendTerminalInput(t, ctx, ws1, "echo first\r")
	firstOutput := waitForTerminalText(t, ctx, frames1, "first", 30*time.Second)
	t.Logf("first attach output:\n%s", firstOutput)
	ws1.Close(websocket.StatusNormalClosure, "detach")
	time.Sleep(300 * time.Millisecond)

	// Second attach: the server must replay what the terminal printed before.
	ws2, frames2 := dialTerminalStream(t, ctx, streamURL)
	defer ws2.CloseNow()
	var replayText strings.Builder
	var replaySeen bool
	deadline := time.After(5 * time.Second)
	for !replaySeen {
		select {
		case frame := <-frames2.frames:
			t.Logf("reattach frame: type=%s seq=%d data=%q", frame.Type, frame.Seq, snippet(frame.Data))
			if frame.Type == "replay" {
				replaySeen = true
				raw, _ := base64.StdEncoding.DecodeString(frame.Data)
				replayText.Write(raw)
			}
		case err := <-frames2.err:
			t.Fatalf("reattach stream closed before replay: %v", err)
		case <-deadline:
			t.Fatalf("no replay frame after reattach")
		}
	}
	if !strings.Contains(replayText.String(), "first") {
		t.Fatalf("replay does not contain earlier output %q; got %q", "first", replayText.String())
	}

	// Input must still reach the live PTY after reattach.
	sendTerminalInput(t, ctx, ws2, "echo second\r")
	secondOutput := waitForTerminalText(t, ctx, frames2, "second", 30*time.Second)
	t.Logf("second attach output:\n%s", secondOutput)

	// Close the terminal, then attach again: the viewer must get an explicit
	// exit frame instead of silence. POST /close keeps the row (the deleted
	// terminal id is answered with 404 at the websocket handshake, which is
	// also explicit).
	closeURL := baseURL + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2/" + url.PathEscape(terminalID) + "/close"
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, closeURL, nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	if status := doJSON(t, req, nil); status != http.StatusOK {
		t.Fatalf("close terminal status = %d", status)
	}
	time.Sleep(500 * time.Millisecond)
	ws2.CloseNow()

	ws3, frames3 := dialTerminalStream(t, ctx, streamURL)
	defer ws3.CloseNow()
	sawExit := false
	deadline = time.After(10 * time.Second)
	for !sawExit {
		select {
		case frame := <-frames3.frames:
			t.Logf("post-close frame: type=%s exitCode=%v reason=%s code=%v message=%q data=%q",
				frame.Type, intValue(frame.ExitCode), frame.Reason, frame.Code, frame.Message, snippet(frame.Data))
			if frame.Type == "exit" {
				sawExit = true
			}
		case err := <-frames3.err:
			t.Fatalf("post-close stream closed without exit frame: %v", err)
		case <-deadline:
			t.Fatalf("no exit frame after attaching to a closed terminal")
		}
	}
}

// TestTerminalAttachStaleRow attaches to a database row that claims to be
// running but has no PTY behind it (the state a connector restart leaves).
// The client must be told explicitly that the terminal is gone.
func TestTerminalAttachStaleRow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	dataDir := t.TempDir()
	t.Setenv("AA_DATA_DIR", dataDir)
	t.Setenv("AA_LISTEN", "127.0.0.1:8922")
	t.Setenv("AA_LOG_LEVEL", "error")

	cfg := config.LoadServer()
	db, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer db.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	app := server.New(cfg, db, logger)

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatalf("listen on %s: %v", cfg.Listen, err)
	}
	httpServer := &http.Server{Handler: app.Handler()}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = httpServer.Serve(listener)
	}()
	defer func() {
		shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = httpServer.Shutdown(shutdownCtx)
		<-serveDone
	}()

	baseURL := "http://" + cfg.Listen
	waitForHealthy(t, ctx, baseURL)

	user, err := db.CreateUser(ctx, store.User{Email: "terminal-stale-e2e@example.com", PasswordHash: "x", DisplayName: "stale"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	connectorToken := server.NewToken("con")
	connectorRecord, err := db.CreateConnector(ctx, store.Connector{
		UserID: user.ID, Name: "stale", Kind: "desktop",
		TokenHash: server.HashToken(connectorToken), TokenPrefix: connectorToken[:12],
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	userToken, err := auth.CreateUserAccessToken(user.ID, 3600)
	if err != nil {
		t.Fatalf("mint user token: %v", err)
	}

	client := connector.NewClient("ws://"+cfg.Listen+"/api/v2/connector/ws", connectorToken, "stale", nil, logger)
	connectorCtx, stopConnector := context.WithCancel(ctx)
	defer stopConnector()
	go client.Run(connectorCtx)
	waitForConnectorOnline(t, ctx, baseURL, connectorRecord.ID, userToken)

	// A row from a previous connector process: running with a pid, but this
	// connector has never opened a PTY for it.
	stale, err := db.CreateTerminal(ctx, store.Terminal{
		ConnectorID: connectorRecord.ID,
		UserID:      user.ID,
		SessionID:   "browse_" + connectorRecord.ID,
		Label:       "stale",
		Status:      "running",
		PID:         4242,
		Cols:        100,
		Rows:        30,
	})
	if err != nil {
		t.Fatalf("create stale terminal: %v", err)
	}

	streamURL := "ws://" + cfg.Listen + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2/" + url.PathEscape(stale.ID) + "/stream?token=" + url.QueryEscape(userToken) + "&fromSeq=0"
	ws, reader := dialTerminalStream(t, ctx, streamURL)
	defer ws.CloseNow()

	sawUnavailable := false
	sawExit := false
	deadline := time.After(10 * time.Second)
	for !sawUnavailable || !sawExit {
		select {
		case frame := <-reader.frames:
			t.Logf("stale attach frame: type=%s code=%v message=%q exitCode=%v reason=%s",
				frame.Type, frame.Code, frame.Message, intValue(frame.ExitCode), frame.Reason)
			switch frame.Type {
			case "error":
				if frame.Code == "terminal_unavailable" && frame.Message == "终端已失效，请新建" {
					sawUnavailable = true
				}
			case "exit":
				sawExit = true
			}
		case err := <-reader.err:
			t.Fatalf("stale stream closed early: %v", err)
		case <-deadline:
			t.Fatalf("did not receive terminal_unavailable + exit (unavailable=%v exit=%v)", sawUnavailable, sawExit)
		}
	}

	reloaded, err := db.TerminalByID(ctx, stale.ID)
	if err != nil {
		t.Fatalf("reload terminal: %v", err)
	}
	if reloaded.Status != "exited" {
		t.Fatalf("stale terminal status = %q, want exited", reloaded.Status)
	}
}

// terminalFrameReader wraps a terminal socket with a decoded frame channel.
type terminalFrameReader struct {
	frames chan terminalStreamFrame
	err    chan error
}

func dialTerminalStream(t *testing.T, ctx context.Context, streamURL string) (*websocket.Conn, *terminalFrameReader) {
	t.Helper()
	ws, _, err := websocket.Dial(ctx, streamURL, nil)
	if err != nil {
		t.Fatalf("dial terminal stream: %v", err)
	}
	ws.SetReadLimit(8 << 20)
	reader := &terminalFrameReader{
		frames: make(chan terminalStreamFrame, 2048),
		err:    make(chan error, 1),
	}
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				reader.err <- err
				return
			}
			var frame terminalStreamFrame
			if err := json.Unmarshal(data, &frame); err != nil {
				continue
			}
			reader.frames <- frame
		}
	}()
	return ws, reader
}

// waitForTerminalText drains frames until needle appears in replay/output.
func waitForTerminalText(t *testing.T, ctx context.Context, reader *terminalFrameReader, needle string, timeout time.Duration) string {
	t.Helper()
	var seen strings.Builder
	deadline := time.After(timeout)
	for !strings.Contains(seen.String(), needle) {
		select {
		case frame := <-reader.frames:
			switch frame.Type {
			case "replay", "output":
				raw, err := base64.StdEncoding.DecodeString(frame.Data)
				if err != nil {
					t.Fatalf("bad base64 in %s frame: %v", frame.Type, err)
				}
				seen.Write(raw)
				t.Logf("%s frame seq=%d data=%q", frame.Type, frame.Seq, snippet(frame.Data))
			case "error":
				t.Logf("error frame: code=%v message=%s", frame.Code, frame.Message)
			case "exit":
				t.Logf("exit frame while waiting for %q: code=%v reason=%s", needle, intValue(frame.ExitCode), frame.Reason)
			}
		case err := <-reader.err:
			t.Fatalf("terminal stream closed while waiting for %q: %v\nseen: %q", needle, err, seen.String())
		case <-deadline:
			t.Fatalf("timed out waiting for %q\nseen: %q", needle, seen.String())
		case <-ctx.Done():
			t.Fatalf("test context ended while waiting for %q", needle)
		}
	}
	return seen.String()
}

func waitForHealthy(t *testing.T, ctx context.Context, baseURL string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become ready: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func snippet(base64Data string) string {
	raw, err := base64.StdEncoding.DecodeString(base64Data)
	if err != nil {
		return base64Data
	}
	text := string(raw)
	if len(text) > 160 {
		text = text[:160] + "..."
	}
	return text
}

var _ = os.Getenv
