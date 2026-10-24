package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/connector"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

type fsTestEnv struct {
	app            *App
	db             *store.Store
	srv            *httptest.Server
	user           store.User
	token          string
	connectorID    string
	connectorToken string
	root           string
}

func newFSTestEnv(t *testing.T) *fsTestEnv {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	app := New(config.Server{DBPath: st.Path(), PublicURL: "http://example.test"}, st, logger)
	user := createTestUser(t, st, "fs-"+store.NewID("u")+"@example.test", "member")
	userToken := testToken(t, user)

	connectorToken := NewToken("con")
	record, err := st.CreateConnector(context.Background(), store.Connector{
		UserID:      user.ID,
		Name:        "fs-test",
		Kind:        "desktop",
		TokenHash:   HashToken(connectorToken),
		TokenPrefix: connectorToken[:12],
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	mux := http.NewServeMux()
	app.registerFileRoutes(mux)
	mux.HandleFunc("GET /api/v2/connector/ws", app.ServeConnectorWS)
	srv := httptest.NewServer(app.withCommon(mux))

	ctx, cancel := context.WithCancel(context.Background())
	client := connector.NewClient(
		"ws"+strings.TrimPrefix(srv.URL, "http")+"/api/v2/connector/ws",
		connectorToken,
		"fs-test",
		nil,
		logger,
	)
	go client.Run(ctx)
	t.Cleanup(func() {
		cancel()
		srv.Close()
	})

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if !app.connectors.waitForConnector(waitCtx, record.ID) {
		t.Fatal("connector did not come online")
	}

	return &fsTestEnv{
		app:            app,
		db:             st,
		srv:            srv,
		user:           user,
		token:          userToken,
		connectorID:    record.ID,
		connectorToken: connectorToken,
		root:           t.TempDir(),
	}
}

func fsRootQuery(root string) string {
	return url.Values{"root": {root}}.Encode()
}

func fsWriteTestFile(t *testing.T, dir, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create parent directory: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	return path
}

func fsRequest(
	t *testing.T, env *fsTestEnv, method, target, token string, body any,
) (int, map[string]any, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, env.srv.URL+target, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, target, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	var decoded map[string]any
	if len(data) > 0 {
		_ = json.Unmarshal(data, &decoded)
	}
	return resp.StatusCode, decoded, data
}

func fsResult(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	result, ok := body["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result object: %#v", body)
	}
	return result
}

func TestFSReadSmallUsesTransfer(t *testing.T) {
	env := newFSTestEnv(t)
	content := []byte("hello from the file panel\n")
	fsWriteTestFile(t, env.root, "hello.txt", content)

	status, body, raw := fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/"+env.connectorID+"/fs/read?"+fsRootQuery(env.root),
		env.token, map[string]any{"path": "hello.txt"})
	if status != http.StatusOK {
		t.Fatalf("read status = %d: %s", status, raw)
	}
	result := fsResult(t, body)
	transferID, _ := result["transferId"].(string)
	transferToken, _ := result["token"].(string)
	downloadURL, _ := result["downloadUrl"].(string)
	if transferID == "" || transferToken == "" || downloadURL == "" {
		t.Fatalf("small read did not return a transfer: %#v", result)
	}
	if _, inline := result["contentBase64"]; inline {
		t.Fatalf("a small file must use a transfer, not contentBase64")
	}
	if result["size"].(float64) != float64(len(content)) {
		t.Fatalf("size = %v, want %d", result["size"], len(content))
	}

	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+downloadURL, nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download transfer: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d: %s", resp.StatusCode, got)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("downloaded %q, want %q", got, content)
	}
}

func TestFSReadLargeUsesTransferAndRange(t *testing.T) {
	env := newFSTestEnv(t)
	content := bytes.Repeat([]byte("0123456789abcdef"), (2<<20)/16+64)
	fsWriteTestFile(t, env.root, "big.bin", content)

	status, body, raw := fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/"+env.connectorID+"/fs/read?"+fsRootQuery(env.root),
		env.token, map[string]any{"path": "big.bin"})
	if status != http.StatusOK {
		t.Fatalf("read status = %d: %s", status, raw)
	}
	result := fsResult(t, body)
	transferID, _ := result["transferId"].(string)
	transferToken, _ := result["token"].(string)
	downloadURL, _ := result["downloadUrl"].(string)
	if transferID == "" || transferToken == "" || downloadURL == "" {
		t.Fatalf("large read did not return a transfer: %#v", result)
	}
	if _, inline := result["contentBase64"]; inline {
		t.Fatalf("a large file must not be inlined")
	}

	req, _ := http.NewRequest(http.MethodGet, env.srv.URL+downloadURL, nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download transfer: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download status = %d: %s", resp.StatusCode, got)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("downloaded %d bytes, want %d", len(got), len(content))
	}
	digest := sha256.Sum256(content)
	if resp.Header.Get("X-File-Sha256") != hex.EncodeToString(digest[:]) {
		t.Fatalf("X-File-Sha256 = %q", resp.Header.Get("X-File-Sha256"))
	}

	req, _ = http.NewRequest(http.MethodGet, env.srv.URL+downloadURL, nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	req.Header.Set("Range", "bytes=5-14")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("range download: %v", err)
	}
	got, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range status = %d: %s", resp.StatusCode, got)
	}
	if string(got) != "56789abcde" {
		t.Fatalf("range body = %q", got)
	}
	if got := resp.Header.Get("Content-Range"); got != "bytes 5-14/"+fmt.Sprint(len(content)) {
		t.Fatalf("Content-Range = %q", got)
	}
}

func TestFSReadTextUTF8AndBinary(t *testing.T) {
	env := newFSTestEnv(t)
	text := "line one\nline two 世界\n"
	fsWriteTestFile(t, env.root, "notes.txt", []byte(text))
	fsWriteTestFile(t, env.root, "binary.bin", []byte{0x00, 0x01, 0x02, 0xff})
	fsWriteTestFile(t, env.root, "invalid.bin", []byte{0xff, 0xfe, 0xfd})

	base := "/api/v2/connectors/" + env.connectorID + "/fs/readText?" + fsRootQuery(env.root)
	status, body, raw := fsRequest(t, env, http.MethodPost, base, env.token,
		map[string]any{"path": "notes.txt"})
	if status != http.StatusOK {
		t.Fatalf("readText status = %d: %s", status, raw)
	}
	if body["text"] != text {
		t.Fatalf("text = %q, want %q", body["text"], text)
	}
	if body["content"] != text {
		t.Fatalf("content alias = %q", body["content"])
	}
	if body["encoding"] != "utf-8" {
		t.Fatalf("encoding = %v", body["encoding"])
	}

	for _, name := range []string{"binary.bin", "invalid.bin"} {
		status, body, raw := fsRequest(t, env, http.MethodPost, base, env.token,
			map[string]any{"path": name})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("%s status = %d, want 422: %s", name, status, raw)
		}
		if detail, _ := body["detail"].(string); detail == "" {
			t.Fatalf("%s has no detail: %#v", name, body)
		}
	}

	status, _, raw = fsRequest(t, env, http.MethodPost, base, env.token,
		map[string]any{"path": "notes.txt", "maxBytes": fsMaxTextBytes + 1})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("oversized maxBytes status = %d: %s", status, raw)
	}
}

func TestFSWriteBase64PlainAndIfMatch(t *testing.T) {
	env := newFSTestEnv(t)
	content := []byte("written through fs/write\n")
	target := "/api/v2/connectors/" + env.connectorID + "/fs/write?" + fsRootQuery(env.root)

	status, body, raw := fsRequest(t, env, http.MethodPost, target, env.token, map[string]any{
		"path":          "written.txt",
		"contentBase64": base64.StdEncoding.EncodeToString(content),
	})
	if status != http.StatusOK {
		t.Fatalf("write status = %d: %s", status, raw)
	}
	result := fsResult(t, body)
	if result["bytesWritten"].(float64) != float64(len(content)) {
		t.Fatalf("bytesWritten = %v", result["bytesWritten"])
	}
	onDisk, err := os.ReadFile(filepath.Join(env.root, "written.txt"))
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !bytes.Equal(onDisk, content) {
		t.Fatalf("written content = %q", onDisk)
	}
	oldSHA, _ := result["sha256"].(string)

	// The browser sends plain text plus the sha it last read; a changed file
	// must answer 412 rather than silently overwriting it.
	status, _, raw = fsRequest(t, env, http.MethodPost, target, env.token, map[string]any{
		"path":    "written.txt",
		"content": "new content",
		"ifMatch": oldSHA,
	})
	if status != http.StatusOK {
		t.Fatalf("plain write status = %d: %s", status, raw)
	}
	status, body, raw = fsRequest(t, env, http.MethodPost, target, env.token, map[string]any{
		"path":    "written.txt",
		"content": "conflicting content",
		"ifMatch": oldSHA,
	})
	if status != http.StatusPreconditionFailed {
		t.Fatalf("stale write status = %d, want 412: %s", status, raw)
	}
	if detail, _ := body["detail"].(string); detail == "" {
		t.Fatalf("stale write has no detail: %#v", body)
	}

	status, _, raw = fsRequest(t, env, http.MethodPost, target, env.token,
		map[string]any{"path": "missing-content.txt"})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("missing content status = %d: %s", status, raw)
	}
}

func TestFSPreviewFlow(t *testing.T) {
	env := newFSTestEnv(t)
	text := "preview only\n"
	fsWriteTestFile(t, env.root, "preview.txt", []byte(text))

	status, body, raw := fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/"+env.connectorID+"/fs/preview-token?"+fsRootQuery(env.root),
		env.token, map[string]any{"path": "preview.txt"})
	if status != http.StatusOK {
		t.Fatalf("preview-token status = %d: %s", status, raw)
	}
	previewToken, _ := body["previewToken"].(string)
	if previewToken == "" || body["expiresAt"] == "" {
		t.Fatalf("preview-token response = %#v", body)
	}

	status, session, raw := fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/fs/preview-session", "",
		map[string]any{"previewToken": previewToken})
	if status != http.StatusOK {
		t.Fatalf("preview-session status = %d: %s", status, raw)
	}
	accessToken, _ := session["previewAccessToken"].(string)
	if accessToken == "" || session["connectorId"] != env.connectorID {
		t.Fatalf("preview-session response = %#v", session)
	}

	// A preview token is single use.
	status, _, raw = fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/fs/preview-session", "",
		map[string]any{"previewToken": previewToken})
	if status != http.StatusBadRequest {
		t.Fatalf("reused preview token status = %d: %s", status, raw)
	}

	status, body, raw = fsRequest(t, env, http.MethodPost, "/api/v2/fs/preview/readText", "",
		map[string]any{"previewAccessToken": accessToken, "maxBytes": 4096})
	if status != http.StatusOK {
		t.Fatalf("preview readText status = %d: %s", status, raw)
	}
	if body["text"] != text {
		t.Fatalf("preview text = %q", body["text"])
	}

	status, body, raw = fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/fs/preview/read", "",
		map[string]any{"previewAccessToken": accessToken})
	if status != http.StatusOK {
		t.Fatalf("preview read status = %d: %s", status, raw)
	}
	result := fsResult(t, body)
	downloadURL, _ := result["downloadUrl"].(string)
	if downloadURL == "" {
		t.Fatalf("preview read has no downloadUrl: %#v", result)
	}

	// The iframe cannot send a bearer, so this request carries only the
	// preview access token embedded in the URL.
	resp, err := http.Get(env.srv.URL + downloadURL)
	if err != nil {
		t.Fatalf("preview download: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(got) != text {
		t.Fatalf("preview download status = %d body = %q", resp.StatusCode, got)
	}

	status, _, raw = fsRequest(t, env, http.MethodPost, "/api/v2/fs/preview/readText", "",
		map[string]any{"previewAccessToken": "not-a-token"})
	if status != http.StatusUnauthorized {
		t.Fatalf("invalid preview token status = %d: %s", status, raw)
	}
}

func TestFSOwnershipOfflineAndPathEscape(t *testing.T) {
	env := newFSTestEnv(t)
	other := createTestUser(t, env.db, "fs-other-"+store.NewID("u")+"@example.test", "member")
	otherToken := testToken(t, other)

	target := "/api/v2/connectors/" + env.connectorID + "/fs/read?" + fsRootQuery(env.root)
	status, _, raw := fsRequest(t, env, http.MethodPost, target, otherToken,
		map[string]any{"path": "anything.txt"})
	if status != http.StatusNotFound {
		t.Fatalf("foreign connector status = %d, want 404: %s", status, raw)
	}

	status, _, raw = fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/"+env.connectorID+"/fs/preview-token?"+fsRootQuery(env.root),
		otherToken, map[string]any{"path": "anything.txt"})
	if status != http.StatusNotFound {
		t.Fatalf("foreign preview-token status = %d, want 404: %s", status, raw)
	}

	fsWriteTestFile(t, env.root, "inside.txt", []byte("inside"))
	outside := filepath.Join(t.TempDir(), "outside.txt")
	fsWriteTestFile(t, filepath.Dir(outside), filepath.Base(outside), []byte("outside"))

	for _, pathValue := range []string{
		"../outside.txt",
		"..\\outside.txt",
		"sub/../../outside.txt",
		outside,
	} {
		status, body, raw := fsRequest(t, env, http.MethodPost, target, env.token,
			map[string]any{"path": pathValue})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("path %q status = %d, want 422: %s", pathValue, status, raw)
		}
		if detail, _ := body["detail"].(string); !strings.Contains(strings.ToLower(detail), "escape") &&
			!strings.Contains(strings.ToLower(detail), "root") {
			t.Fatalf("path %q detail = %q", pathValue, detail)
		}
	}

	// A connector that has never connected is offline, not a 404.
	offlineToken := NewToken("con")
	offline, err := env.db.CreateConnector(context.Background(), store.Connector{
		UserID:      env.user.ID,
		Name:        "offline",
		Kind:        "desktop",
		TokenHash:   HashToken(offlineToken),
		TokenPrefix: offlineToken[:12],
	})
	if err != nil {
		t.Fatalf("create offline connector: %v", err)
	}
	status, _, raw = fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/"+offline.ID+"/fs/read?"+fsRootQuery(env.root),
		env.token, map[string]any{"path": "inside.txt"})
	if status != http.StatusConflict {
		t.Fatalf("offline connector status = %d, want 409: %s", status, raw)
	}

	// Without root the request is rejected before any RPC.
	status, _, raw = fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/"+env.connectorID+"/fs/read", env.token,
		map[string]any{"path": "inside.txt"})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("missing root status = %d, want 422: %s", status, raw)
	}
}

func TestConnectorRejectsEscapedRPCPath(t *testing.T) {
	env := newFSTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := env.app.callConnector(ctx, env.connectorID, "fs.size", map[string]any{
		"root": env.root,
		"path": "../escaped.txt",
	}, 5*time.Second)
	if err == nil {
		t.Fatal("connector accepted a path outside the root")
	}
	var failure *rpcFailure
	if !errors.As(err, &failure) || failure.code == "" {
		t.Fatalf("unexpected connector error: %v", err)
	}
}

func TestFSTransferPushFromConnector(t *testing.T) {
	env := newFSTestEnv(t)
	content := bytes.Repeat([]byte("x"), (2<<20)+32)
	fsWriteTestFile(t, env.root, "large.bin", content)

	status, body, raw := fsRequest(t, env, http.MethodPost,
		"/api/v2/connectors/"+env.connectorID+"/fs/read?"+fsRootQuery(env.root),
		env.token, map[string]any{"path": "large.bin"})
	if status != http.StatusOK {
		t.Fatalf("read status = %d: %s", status, raw)
	}
	result := fsResult(t, body)
	transferID, _ := result["transferId"].(string)
	transferToken, _ := result["token"].(string)

	pushed := []byte("bytes pushed by the connector\n")
	req, err := http.NewRequest(http.MethodPut,
		env.srv.URL+"/api/v2/connector/fs/transfers/"+transferID+"?token="+url.QueryEscape(transferToken),
		bytes.NewReader(pushed))
	if err != nil {
		t.Fatalf("build push request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+env.connectorToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("push transfer: %v", err)
	}
	pushBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push status = %d: %s", resp.StatusCode, pushBody)
	}

	req, _ = http.NewRequest(http.MethodGet,
		env.srv.URL+"/api/v2/connectors/"+env.connectorID+"/fs/transfers/"+transferID+
			"?token="+url.QueryEscape(transferToken), nil)
	req.Header.Set("Authorization", "Bearer "+env.token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("download pushed transfer: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !bytes.Equal(got, pushed) {
		t.Fatalf("pushed download status = %d body = %q", resp.StatusCode, got)
	}
}

// TestFSManualServer is the harness used for the real curl walkthrough. It is
// skipped unless AA_FS_MANUAL=1, and writes credentials to the path in
// AA_FS_MANUAL_CREDS so an external shell can drive the endpoints.
func TestFSManualServer(t *testing.T) {
	if os.Getenv("AA_FS_MANUAL") != "1" {
		t.Skip("set AA_FS_MANUAL=1 to run the manual HTTP harness")
	}
	credsPath := os.Getenv("AA_FS_MANUAL_CREDS")
	stopPath := os.Getenv("AA_FS_MANUAL_STOP")
	if credsPath == "" || stopPath == "" {
		t.Fatal("AA_FS_MANUAL_CREDS and AA_FS_MANUAL_STOP are required")
	}
	addr := os.Getenv("AA_FS_MANUAL_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8917"
	}

	cfg := config.LoadServer()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(context.Background(), cfg.DBPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer db.Close()
	app := New(cfg, db, logger)

	user := createTestUser(t, db, "fs-manual@example.test", "member")
	userToken := testToken(t, user)
	connectorToken := NewToken("con")
	record, err := db.CreateConnector(context.Background(), store.Connector{
		UserID:      user.ID,
		Name:        "fs-manual",
		Kind:        "desktop",
		TokenHash:   HashToken(connectorToken),
		TokenPrefix: connectorToken[:12],
	})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}

	root := os.Getenv("AA_FS_MANUAL_ROOT")
	if root == "" {
		root = t.TempDir()
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create root: %v", err)
	}
	fsWriteTestFile(t, root, "hello.txt", []byte("hello from the fs panel\n"))
	fsWriteTestFile(t, root, "binary.bin", []byte{0x00, 0x01, 0x02, 0xff})
	big := bytes.Repeat([]byte("0123456789abcdef"), (2<<20)/16+64)
	fsWriteTestFile(t, root, "big.bin", big)

	mux := http.NewServeMux()
	app.registerFileRoutes(mux)
	mux.HandleFunc("GET /api/v2/connector/ws", app.ServeConnectorWS)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("listen on %s: %v", addr, err)
	}
	httpServer := &http.Server{Handler: app.withCommon(mux)}
	go func() { _ = httpServer.Serve(listener) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(ctx)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client := connector.NewClient(
		"ws://"+addr+"/api/v2/connector/ws",
		connectorToken,
		"fs-manual",
		nil,
		logger,
	)
	go client.Run(ctx)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer waitCancel()
	if !app.connectors.waitForConnector(waitCtx, record.ID) {
		t.Fatal("connector did not come online")
	}

	creds, _ := json.MarshalIndent(map[string]any{
		"baseUrl":        "http://" + addr,
		"userToken":      userToken,
		"connectorToken": connectorToken,
		"connectorId":    record.ID,
		"root":           root,
	}, "", "  ")
	if err := os.WriteFile(credsPath, creds, 0o644); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	t.Logf("manual server ready on %s, credentials at %s", addr, credsPath)

	deadline := time.Now().Add(10 * time.Minute)
	for {
		if _, err := os.Stat(stopPath); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the stop file")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForConnector blocks until the connector has a socket or ctx ends.
func (h *ConnectorHub) waitForConnector(ctx context.Context, connectorID string) bool {
	if h.Online(connectorID) {
		return true
	}
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if h.Online(connectorID) {
				return true
			}
		}
	}
}
