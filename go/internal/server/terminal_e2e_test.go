//go:build e2e

// End-to-end terminal test.
//
// It starts the real HTTP server on 127.0.0.1:8911 with an isolated
// AA_DATA_DIR, runs the real connector (including ConPTY on Windows), creates
// a terminal over HTTP, drives it over the terminal websocket and closes it.
//
//	go test -tags=e2e ./internal/server -run TestTerminalEndToEnd -v
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
	"path/filepath"
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

type terminalStreamFrame struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	Seq      uint64 `json:"seq"`
	ExitCode *int   `json:"exitCode"`
	Reason   string `json:"reason"`
	Code     any    `json:"code"`
	Message  string `json:"message"`
}

func TestTerminalEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	dataDir := t.TempDir()
	t.Setenv("AA_DATA_DIR", dataDir)
	t.Setenv("AA_LISTEN", "127.0.0.1:8911")
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
		t.Fatalf("listen on %s: %v (is another server already running?)", cfg.Listen, err)
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server did not become ready: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	user, err := db.CreateUser(ctx, store.User{
		Email:        "terminal-e2e@example.com",
		PasswordHash: "not-used",
		DisplayName:  "Terminal E2E",
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	connectorToken := server.NewToken("con")
	connectorRecord, err := db.CreateConnector(ctx, store.Connector{
		UserID:      user.ID,
		Name:        "e2e-desktop",
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

	// The real connector process loop, in-process, pointed at the real server.
	client := connector.NewClient(
		"ws://"+cfg.Listen+"/api/v2/connector/ws",
		connectorToken,
		"e2e-desktop",
		nil,
		logger,
	)
	connectorCtx, stopConnector := context.WithCancel(ctx)
	defer stopConnector()
	go client.Run(connectorCtx)

	waitForConnectorOnline(t, ctx, baseURL, connectorRecord.ID, userToken)

	workspace := t.TempDir()
	markerPath := filepath.Join(workspace, "marker.txt")
	if err := os.WriteFile(markerPath, []byte("e2e"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	createBody := fmt.Sprintf(`{"cols":100,"rows":30,"label":"e2e","cwd":%q}`, workspace)
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
			CWD        string `json:"cwd"`
		} `json:"result"`
		Error any `json:"error"`
	}
	resp := doJSON(t, req, &created)
	if resp != http.StatusOK {
		t.Fatalf("create terminal status = %d", resp)
	}
	if !created.OK || created.Result.TerminalID == "" {
		t.Fatalf("create response = %+v", created)
	}
	terminalID := created.Result.TerminalID
	t.Logf("created terminal %s status=%s pid=%v cwd=%s",
		terminalID, created.Result.Status, intValue(created.Result.PID), created.Result.CWD)

	// The create waits for the connector's opened frame; poll the list once
	// more so a slow first PowerShell start cannot make this flaky.
	waitForTerminalRunning(t, ctx, baseURL, connectorRecord.ID, terminalID, userToken)

	ticketURL := baseURL + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2/" + url.PathEscape(terminalID) + "/ticket"
	req, _ = http.NewRequestWithContext(ctx, http.MethodPost, ticketURL, strings.NewReader(`{"clientId":"e2e"}`))
	req.Header.Set("Authorization", "Bearer "+userToken)
	req.Header.Set("Content-Type", "application/json")
	var issued struct {
		Ticket string `json:"ticket"`
	}
	if status := doJSON(t, req, &issued); status != http.StatusOK || issued.Ticket == "" {
		t.Fatalf("ticket status = %d, ticket = %q", status, issued.Ticket)
	}

	streamURL := "ws://" + cfg.Listen + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2/" + url.PathEscape(terminalID) + "/stream?ticket=" + url.QueryEscape(issued.Ticket)
	ws, _, err := websocket.Dial(ctx, streamURL, nil)
	if err != nil {
		t.Fatalf("dial terminal stream: %v", err)
	}
	defer ws.CloseNow()
	ws.SetReadLimit(8 << 20)

	frames := make(chan terminalStreamFrame, 1024)
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			var frame terminalStreamFrame
			if err := json.Unmarshal(data, &frame); err != nil {
				continue
			}
			frames <- frame
		}
	}()

	var output strings.Builder
	var lastSeq uint64
	seqSeen := false
	sawHello := false
	sawMarker := false
	sawExit := false
	var exitCode *int

	consume := func(frame terminalStreamFrame) {
		switch frame.Type {
		case "replay":
			raw, err := base64.StdEncoding.DecodeString(frame.Data)
			if err == nil {
				output.Write(raw)
			}
			lastSeq = frame.Seq
			seqSeen = true
		case "output":
			if seqSeen && frame.Seq <= lastSeq {
				t.Fatalf("terminal seq went backwards: %d after %d", frame.Seq, lastSeq)
			}
			lastSeq = frame.Seq
			seqSeen = true
			raw, err := base64.StdEncoding.DecodeString(frame.Data)
			if err != nil {
				t.Fatalf("output is not base64: %v", err)
			}
			output.Write(raw)
		case "error":
			t.Logf("terminal error frame: code=%v message=%s", frame.Code, frame.Message)
		case "exit":
			sawExit = true
			exitCode = frame.ExitCode
			t.Logf("exit frame: code=%v reason=%s", intValue(frame.ExitCode), frame.Reason)
		}
		if strings.Contains(output.String(), "hello") {
			sawHello = true
		}
		if strings.Contains(output.String(), "marker.txt") {
			sawMarker = true
		}
	}

	go func() {
		time.Sleep(700 * time.Millisecond)
		sendTerminalInput(t, ctx, ws, "echo hello\r")
		time.Sleep(500 * time.Millisecond)
		sendTerminalInput(t, ctx, ws, "dir\r")
	}()

	waitUntil := func(what string, timeout time.Duration, done func() bool) {
		t.Helper()
		limit := time.After(timeout)
		for !done() {
			select {
			case frame := <-frames:
				consume(frame)
			case err := <-readErr:
				t.Fatalf("terminal stream closed while waiting for %s: %v\noutput so far:\n%s",
					what, err, output.String())
			case <-limit:
				t.Fatalf("timed out waiting for %s\noutput so far:\n%s", what, output.String())
			case <-ctx.Done():
				t.Fatalf("test context ended while waiting for %s", what)
			}
		}
	}
	waitUntil("shell output", 30*time.Second, func() bool { return sawHello && sawMarker })
	t.Logf("terminal output:\n%s", output.String())

	// Close over HTTP, exactly as the dashboard does.
	closeURL := baseURL + "/api/v2/connectors/" + url.PathEscape(connectorRecord.ID) +
		"/terminals-v2/" + url.PathEscape(terminalID)
	req, _ = http.NewRequestWithContext(ctx, http.MethodDelete, closeURL, nil)
	req.Header.Set("Authorization", "Bearer "+userToken)
	if status := doJSON(t, req, nil); status != http.StatusOK {
		t.Fatalf("delete terminal status = %d", status)
	}

	waitUntil("terminal.exit", 20*time.Second, func() bool { return sawExit })
	t.Logf("received terminal.exit: exitCode=%v", intValue(exitCode))

	if !sawHello {
		t.Fatalf("never saw 'hello' in terminal output")
	}
	if !sawMarker {
		t.Fatalf("never saw the directory listing in terminal output")
	}
}

// intValue renders an optional int for a log line.
func intValue(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}

func sendTerminalInput(t *testing.T, ctx context.Context, ws *websocket.Conn, text string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"type": "input",
		"data": base64.StdEncoding.EncodeToString([]byte(text)),
	})
	writeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := ws.Write(writeCtx, websocket.MessageText, body); err != nil {
		t.Logf("write terminal input: %v", err)
	}
}

func waitForConnectorOnline(t *testing.T, ctx context.Context, baseURL, connectorID, token string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	url := baseURL + "/api/v2/connectors/" + url.PathEscape(connectorID)
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		var body struct {
			Connector struct {
				Status string `json:"status"`
			} `json:"connector"`
		}
		if status := doJSON(t, req, &body); status == http.StatusOK && body.Connector.Status == "online" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("connector never came online")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func waitForTerminalRunning(t *testing.T, ctx context.Context, baseURL, connectorID, terminalID, token string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	url := baseURL + "/api/v2/connectors/" + url.PathEscape(connectorID) + "/terminals-v2"
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		var body struct {
			Result struct {
				Terminals []struct {
					TerminalID string `json:"terminalId"`
					Status     string `json:"status"`
					PID        *int   `json:"pid"`
				} `json:"terminals"`
			} `json:"result"`
		}
		if status := doJSON(t, req, &body); status == http.StatusOK {
			for _, terminal := range body.Result.Terminals {
				if terminal.TerminalID == terminalID && terminal.Status == "running" {
					t.Logf("terminal is running with pid %v", intValue(terminal.PID))
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal never reached running state")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func doJSON(t *testing.T, req *http.Request, out any) int {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", req.Method, req.URL, body, err)
		}
	}
	return resp.StatusCode
}
