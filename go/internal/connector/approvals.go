package connector

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// maxApprovalBody bounds what the plugin may send; a permission question is a
// tool name and its arguments, never megabytes.
const maxApprovalBody = 256 << 10

// ApprovalServer is the loopback endpoint the phone-pane plugin calls before it
// runs a tool.
//
// The hook's own budget is ten seconds, but the engine's clock stops while a
// plugin `$` call is in flight — so the plugin holds one HTTP request open
// here for as long as it takes a person to answer on a phone. A request that
// waits longer than the plugin is willing to hold it returns "pending" and the
// plugin asks again, which is why no logical timeout exists on this side.
type ApprovalServer struct {
	// Ask forwards the question and blocks until there is a decision, or until
	// ctx is done. Returning nil sends "pending".
	Ask func(ctx context.Context, request map[string]any) (map[string]any, error)

	token      string
	endpoint   string
	httpServer *http.Server
	listener   net.Listener
	log        *slog.Logger
}

// NewApprovalServer prepares the endpoint. endpointPath is where the plugin
// reads the port and token from (approval.json in the bridge directory).
func NewApprovalServer(endpointPath, token string, ask func(context.Context, map[string]any) (map[string]any, error), log *slog.Logger) *ApprovalServer {
	return &ApprovalServer{Ask: ask, token: token, endpoint: endpointPath, log: log}
}

// Start binds a loopback port and publishes it for the plugin.
func (s *ApprovalServer) Start() error {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.listener = listener

	mux := http.NewServeMux()
	mux.HandleFunc("POST /approval", s.handle)
	s.httpServer = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		if err := s.httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Warn("approval endpoint stopped", "error", err)
		}
	}()

	port := listener.Addr().(*net.TCPAddr).Port
	payload, _ := json.Marshal(map[string]any{"port": port, "token": s.token})
	if err := writeFileAtomic(s.endpoint, payload); err != nil {
		// The bridge still works for prompts; only approvals are unavailable.
		s.log.Warn("could not publish the approval endpoint", "error", err)
	}
	s.log.Info("approval endpoint listening", "port", port)
	return nil
}

// Stop closes the endpoint and removes the published file.
func (s *ApprovalServer) Stop() {
	if s.httpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.httpServer.Shutdown(ctx)
	}
	_ = os.Remove(s.endpoint)
}

func (s *ApprovalServer) handle(w http.ResponseWriter, r *http.Request) {
	writeJSON := func(status int, value any) {
		body, _ := json.Marshal(value)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxApprovalBody+1))
	if err != nil || len(body) == 0 || len(body) > maxApprovalBody {
		writeJSON(http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		writeJSON(http.StatusBadRequest, map[string]any{"error": "bad request"})
		return
	}
	received, _ := request["token"].(string)
	if subtle.ConstantTimeCompare([]byte(received), []byte(s.token)) != 1 {
		writeJSON(http.StatusForbidden, map[string]any{"error": "forbidden"})
		return
	}

	answer, err := s.Ask(r.Context(), request)
	if err != nil {
		// Never leave the hook hanging: a failed ask falls through to the
		// engine's own verdict rather than denying the tool outright.
		s.log.Warn("approval ask failed", "error", err)
		answer = nil
	}
	if answer == nil {
		answer = map[string]any{"decision": "pending"}
	}
	writeJSON(http.StatusOK, normaliseAnswer(answer))
}

// normaliseAnswer accepts either spelling of the replacement input, because the
// hook silently ignores a key it does not recognise.
func normaliseAnswer(answer map[string]any) map[string]any {
	out := make(map[string]any, len(answer))
	for k, v := range answer {
		out[k] = v
	}
	updated, ok := out["updatedInput"]
	if !ok {
		if alt, has := out["updated_input"]; has {
			updated = alt
			delete(out, "updated_input")
			out["updatedInput"] = updated
			return out
		}
		return out
	}
	if _, isMap := updated.(map[string]any); !isMap {
		delete(out, "updatedInput")
	}
	return out
}

// writeFileAtomic writes through a temporary file in the same directory so a
// reader never sees half a JSON document.
func writeFileAtomic(path string, body []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if _, err := temp.Write(body); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
