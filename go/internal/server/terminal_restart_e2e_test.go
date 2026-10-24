//go:build e2e

// Temporary probe: what does a viewer see after the server restarts while the
// connector (and its PTY) keeps running?
package server_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/auth"
	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/connector"
	"github.com/agents-anywhere/agents-anywhere/internal/server"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

type probeServer struct {
	cfg      config.Server
	db       *store.Store
	http     *http.Server
	listener net.Listener
	done     chan struct{}
	connMu   sync.Mutex
	hijacked []net.Conn
}

func startProbeServer(t *testing.T, ctx context.Context, db *store.Store, logger *slog.Logger) *probeServer {
	t.Helper()
	cfg := config.LoadServer()
	app := server.New(cfg, db, logger)
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	p := &probeServer{cfg: cfg, db: db, listener: listener, done: make(chan struct{})}
	httpServer := &http.Server{Handler: app.Handler()}
	httpServer.ConnState = func(c net.Conn, state http.ConnState) {
		if state == http.StateHijacked {
			p.connMu.Lock()
			p.hijacked = append(p.hijacked, c)
			p.connMu.Unlock()
		}
	}
	p.http = httpServer
	go func() {
		defer close(p.done)
		_ = httpServer.Serve(listener)
	}()
	waitForHealthy(t, ctx, "http://"+cfg.Listen)
	return p
}

func (p *probeServer) stop(t *testing.T) {
	t.Helper()
	// A real process death resets every socket, including hijacked websockets
	// that http.Server.Shutdown ignores.
	p.connMu.Lock()
	for _, conn := range p.hijacked {
		_ = conn.Close()
	}
	p.hijacked = nil
	p.connMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.http.Shutdown(ctx)
	<-p.done
}

func TestTerminalReattachAfterServerRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	dataDir := t.TempDir()
	t.Setenv("AA_DATA_DIR", dataDir)
	t.Setenv("AA_LISTEN", "127.0.0.1:8921")
	t.Setenv("AA_LOG_LEVEL", "error")

	cfg := config.LoadServer()
	db1, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv1 := startProbeServer(t, ctx, db1, logger)

	user, err := db1.CreateUser(ctx, store.User{Email: "restart-probe@example.com", PasswordHash: "x", DisplayName: "probe"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	connectorToken := server.NewToken("con")
	connectorRecord, err := db1.CreateConnector(ctx, store.Connector{
		UserID: user.ID, Name: "probe", Kind: "desktop",
		TokenHash: server.HashToken(connectorToken), TokenPrefix: connectorToken[:12],
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	userToken, err := auth.CreateUserAccessToken(user.ID, 3600)
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}

	client := connector.NewClient("ws://"+cfg.Listen+"/api/v2/connector/ws", connectorToken, "probe", nil, logger)
	connectorCtx, stopConnector := context.WithCancel(ctx)
	defer stopConnector()
	go client.Run(connectorCtx)
	waitForConnectorOnline(t, ctx, "http://"+cfg.Listen, connectorRecord.ID, userToken)

	workspace := t.TempDir()
	createBody := fmt.Sprintf(`{"cols":100,"rows":30,"label":"probe","cwd":%q}`, workspace)
	createURL := "http://" + cfg.Listen + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2?root=" + url.QueryEscape(workspace)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, createURL, strings.NewReader(createBody))
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Content-Type", "application/json")
	var created struct {
		Result struct {
			TerminalID string `json:"terminalId"`
		} `json:"result"`
	}
	if status := doJSON(t, req, &created); status != http.StatusOK {
		t.Fatalf("create status=%d", status)
	}
	terminalID := created.Result.TerminalID
	t.Logf("terminal %s created", terminalID)

	streamURL := "ws://" + cfg.Listen + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2/" + url.PathEscape(terminalID) + "/stream?token=" + url.QueryEscape(userToken) + "&fromSeq=0"
	ws1, frames1 := dialTerminalStream(t, ctx, streamURL)
	sendTerminalInput(t, ctx, ws1, "echo first\r")
	first := waitForTerminalText(t, ctx, frames1, "first", 30*time.Second)
	t.Logf("before restart output contains first=%v", strings.Contains(first, "first"))
	ws1.Close(websocket.StatusNormalClosure, "detach")

	// Restart the server: new process state (empty hub), same SQLite rows.
	t.Log("stopping server 1")
	srv1.stop(t)
	_ = db1.Close()
	t.Log("server 1 stopped")

	db2, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer db2.Close()
	srv2 := startProbeServer(t, ctx, db2, logger)
	defer srv2.stop(t)
	t.Log("server 2 up")
	waitForConnectorOnline(t, ctx, "http://"+cfg.Listen, connectorRecord.ID, userToken)
	t.Log("connector online on server 2")

	ws2, frames2 := dialTerminalStream(t, ctx, streamURL)
	defer ws2.CloseNow()
	// Read frames for two seconds without sending input; log everything. The
	// connector keeps a scrollback snapshot, so the replay must still contain
	// the output produced before the server restarted.
	var history strings.Builder
	deadline := time.After(2 * time.Second)
collect:
	for {
		select {
		case frame := <-frames2.frames:
			t.Logf("post-restart frame: type=%s seq=%d data=%q exit=%v code=%v msg=%q",
				frame.Type, frame.Seq, snippet(frame.Data), intValue(frame.ExitCode), frame.Code, frame.Message)
			if frame.Type == "replay" || frame.Type == "output" {
				raw, _ := base64.StdEncoding.DecodeString(frame.Data)
				history.Write(raw)
			}
		case err := <-frames2.err:
			t.Logf("post-restart stream error: %v", err)
			break collect
		case <-deadline:
			break collect
		}
	}
	if !strings.Contains(history.String(), "first") {
		t.Fatalf("post-restart replay lost the earlier output; got %q", history.String())
	}

	// Now type. Does anything come back?
	sendTerminalInput(t, ctx, ws2, "echo probe\r")
	t.Log("sent echo probe after restart")
	var after strings.Builder
	deadline = time.After(8 * time.Second)
	for !strings.Contains(after.String(), "probe") {
		select {
		case frame := <-frames2.frames:
			t.Logf("after-input frame: type=%s seq=%d data=%q exit=%v code=%v msg=%q",
				frame.Type, frame.Seq, snippet(frame.Data), intValue(frame.ExitCode), frame.Code, frame.Message)
			if frame.Type == "replay" || frame.Type == "output" {
				raw, _ := base64.StdEncoding.DecodeString(frame.Data)
				after.Write(raw)
			}
		case err := <-frames2.err:
			t.Logf("after-input stream error: %v", err)
			t.Fatalf("post-restart output=%q", after.String())
		case <-deadline:
			t.Fatalf("BUG REPRODUCED: no output after typing into the reattached terminal; seen %q", after.String())
		}
	}
	t.Logf("post-restart output seen: %q", after.String())

	// Release the shell so the temporary workspace can be removed.
	closeURL := "http://" + cfg.Listen + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2/" + url.PathEscape(terminalID) + "/close"
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, closeURL, nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	if status := doJSON(t, req, nil); status != http.StatusOK {
		t.Fatalf("close terminal status = %d", status)
	}
	deadline = time.After(10 * time.Second)
	for {
		select {
		case frame := <-frames2.frames:
			if frame.Type == "exit" {
				t.Logf("post-restart exit frame: code=%v reason=%s", intValue(frame.ExitCode), frame.Reason)
				return
			}
		case <-frames2.err:
			return
		case <-deadline:
			t.Fatalf("terminal did not exit after close")
		}
	}
}
