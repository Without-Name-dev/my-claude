package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// TestGatewayEndToEnd drives a streaming Messages request through the real
// router: credential auth, session lookup, model rewrite, upstream streaming,
// and response passthrough.
func TestGatewayEndToEnd(t *testing.T) {
	var upstreamModel string
	var upstreamKey string
	stream := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		_ = json.Unmarshal(body, &doc)
		if model, ok := doc["model"].(string); ok {
			upstreamModel = model
		}
		upstreamKey = r.Header.Get("x-api-key")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, stream)
	}))
	defer upstream.Close()

	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	user, err := s.CreateUser(ctx, store.User{Email: "e2e@example.com", PasswordHash: "x"})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	connector, err := s.CreateConnector(ctx, store.Connector{UserID: user.ID, Name: "PC"})
	if err != nil {
		t.Fatalf("create connector: %v", err)
	}
	provider, err := s.CreateModelProvider(ctx, store.ModelProvider{
		UserID: user.ID, Name: "P", Protocol: "anthropic_messages",
		BaseURL: upstream.URL, AuthType: "api_key", Credential: "sk-upstream",
		ModelID: "upstream-model", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, provider.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}
	session, err := s.CreateSession(ctx, store.Session{ConnectorID: connector.ID, ExternalID: "ext-1"})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	_, token, err := s.IssueGatewayCredential(ctx, connector.ID, user.ID, func() (string, string, string, error) {
		return "gwc_e2e", HashToken("gwc_e2e"), "gwc_e2e", nil
	})
	if err != nil {
		t.Fatalf("issue credential: %v", err)
	}

	app := New(config.Server{
		PublicURL:                    "http://127.0.0.1",
		GatewayMaxBodyBytes:          1 << 20,
		GatewayUpstreamHeaderTimeout: 10 * time.Second,
	}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	handler := app.Handler()

	requestBody := []byte(`{"model":"claude-sonnet","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v2/gateway/v1/messages", bytes.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", token)
	req.Header.Set("x-claude-code-session-id", "ext-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("gateway status %d: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != stream {
		t.Fatalf("stream not passed through\n got: %q\nwant: %q", got, stream)
	}
	if upstreamModel != "upstream-model" {
		t.Fatalf("model not rewritten upstream: %q", upstreamModel)
	}
	if upstreamKey != "sk-upstream" {
		t.Fatalf("provider credential not injected: %q", upstreamKey)
	}

	// The request must be attributed to the session it named, and the route
	// must record what was actually applied.
	route, err := s.SessionModelRouteFor(ctx, session.ID)
	if err != nil {
		t.Fatalf("route row missing: %v", err)
	}
	if route.AppliedProviderID == nil || *route.AppliedProviderID != provider.ID || route.Status != "applied" {
		t.Fatalf("route not marked applied: %+v", route)
	}
}

// TestGatewayRejectsUnknownCredential proves the gateway authenticates with
// its own credential, not a user bearer.
func TestGatewayRejectsUnknownCredential(t *testing.T) {
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	app := New(config.Server{
		PublicURL:                    "http://127.0.0.1",
		GatewayMaxBodyBytes:          1 << 20,
		GatewayUpstreamHeaderTimeout: 10 * time.Second,
	}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/api/v2/gateway/v1/messages",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("x-api-key", "gwc_not_real")
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Fatalf("error shape is not Anthropic-compatible: %s", rec.Body.String())
	}
}

// TestGatewayNoDefaultIsHonest checks the error a user sees before they
// configure anything.
func TestGatewayNoDefaultIsHonest(t *testing.T) {
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	user, _ := s.CreateUser(ctx, store.User{Email: "u@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, store.Connector{UserID: user.ID, Name: "PC"})
	_, token, err := s.IssueGatewayCredential(ctx, connector.ID, user.ID, func() (string, string, string, error) {
		return "gwc_t", HashToken("gwc_t"), "gwc_t", nil
	})
	if err != nil {
		t.Fatalf("issue credential: %v", err)
	}
	app := New(config.Server{
		PublicURL:                    "http://127.0.0.1",
		GatewayMaxBodyBytes:          1 << 20,
		GatewayUpstreamHeaderTimeout: 10 * time.Second,
	}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/api/v2/gateway/v1/messages",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("x-api-key", token)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "no model provider") {
		t.Fatalf("unhelpful error: %s", rec.Body.String())
	}
}

// TestGatewayOpenAIStreamConversion proves the OpenAI adapter turns chat
// completion chunks into a well-formed Anthropic event stream, including a
// streamed tool call.
func TestGatewayOpenAIStreamConversion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The adapter must send a converted Chat Completions body upstream.
		body, _ := io.ReadAll(r.Body)
		var doc map[string]any
		_ = json.Unmarshal(body, &doc)
		if doc["model"] != "gpt-upstream" {
			t.Errorf("upstream model = %v", doc["model"])
		}
		if _, ok := doc["messages"].([]any); !ok {
			t.Errorf("messages not converted: %v", doc["messages"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range []string{
			`data: {"choices":[{"delta":{"role":"assistant","content":"Hel"}}]}`,
			`data: {"choices":[{"delta":{"content":"lo"}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":5}}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, chunk+"\n\n")
		}
	}))
	defer upstream.Close()

	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	user, _ := s.CreateUser(ctx, store.User{Email: "oa@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, store.Connector{UserID: user.ID, Name: "PC"})
	provider, err := s.CreateModelProvider(ctx, store.ModelProvider{
		UserID: user.ID, Name: "OA", Protocol: "openai_chat_completions",
		BaseURL: upstream.URL, AuthType: "bearer", Credential: "oa-key",
		ModelID: "gpt-upstream", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create provider: %v", err)
	}
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, provider.ID); err != nil {
		t.Fatalf("set default: %v", err)
	}
	_, token, err := s.IssueGatewayCredential(ctx, connector.ID, user.ID, func() (string, string, string, error) {
		return "gwc_oa", HashToken("gwc_oa"), "gwc_oa", nil
	})
	if err != nil {
		t.Fatalf("issue credential: %v", err)
	}
	app := New(config.Server{
		PublicURL:                    "http://127.0.0.1",
		GatewayMaxBodyBytes:          1 << 20,
		GatewayUpstreamHeaderTimeout: 10 * time.Second,
	}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))

	body := `{"model":"claude-x","max_tokens":16,"stream":true,"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v2/gateway/v1/messages", strings.NewReader(body))
	req.Header.Set("x-api-key", token)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		`"text":"Hel"`,
		`"text":"lo"`,
		`text_delta`,
		"event: content_block_start",
		`"id":"call_1"`,
		`"name":"lookup"`,
		`input_json_delta`,
		`"partial_json":`,
		`"stop_reason":"tool_use"`,
		"event: message_stop",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in converted stream:\n%s", want, out)
		}
	}
}

// TestGatewayDoesNotFollowRedirects proves a provider answering 3xx neither
// gets followed by the server nor forwarded to the client.
func TestGatewayDoesNotFollowRedirects(t *testing.T) {
	var targetHits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/v1/messages", http.StatusFound)
	}))
	defer redirector.Close()

	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	user, _ := s.CreateUser(ctx, store.User{Email: "r@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, store.Connector{UserID: user.ID, Name: "PC"})
	provider, err := s.CreateModelProvider(ctx, store.ModelProvider{
		UserID: user.ID, Name: "R", Protocol: "anthropic_messages",
		BaseURL: redirector.URL, Credential: "sk", ModelID: "m", Enabled: true,
	})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, provider.ID); err != nil {
		t.Fatalf("default: %v", err)
	}
	_, token, err := s.IssueGatewayCredential(ctx, connector.ID, user.ID, func() (string, string, string, error) {
		return "gwc_r", HashToken("gwc_r"), "gwc_r", nil
	})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	app := New(config.Server{
		PublicURL:                    "http://127.0.0.1",
		GatewayMaxBodyBytes:          1 << 20,
		GatewayUpstreamHeaderTimeout: 10 * time.Second,
	}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/api/v2/gateway/v1/messages",
		strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("x-api-key", token)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for a redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("redirect Location leaked to the client")
	}
	if targetHits != 0 {
		t.Fatalf("gateway followed the redirect (%d hits on the target)", targetHits)
	}
}

// TestGatewayOpenAIJSONAnswerBecomesSSE covers a provider that answers a
// streaming request with a complete JSON body: the client still gets a valid
// SSE sequence.
func TestGatewayOpenAIJSONAnswerBecomesSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Deliberately no text/event-stream content type.
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	user, _ := s.CreateUser(ctx, store.User{Email: "j@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, store.Connector{UserID: user.ID, Name: "PC"})
	provider, err := s.CreateModelProvider(ctx, store.ModelProvider{
		UserID: user.ID, Name: "J", Protocol: "openai_chat_completions",
		BaseURL: upstream.URL, AuthType: "bearer", Credential: "k", ModelID: "m", Enabled: true,
	})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, provider.ID); err != nil {
		t.Fatalf("default: %v", err)
	}
	_, token, err := s.IssueGatewayCredential(ctx, connector.ID, user.ID, func() (string, string, string, error) {
		return "gwc_j", HashToken("gwc_j"), "gwc_j", nil
	})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	app := New(config.Server{
		PublicURL:                    "http://127.0.0.1",
		GatewayMaxBodyBytes:          1 << 20,
		GatewayUpstreamHeaderTimeout: 10 * time.Second,
	}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/api/v2/gateway/v1/messages",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", token)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type %q is not an event stream", ct)
	}
	out := rec.Body.String()
	for _, want := range []string{"message_start", `"text":"hello"`, `"stop_reason":"end_turn"`, "message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}

// TestAnthropicJSONAnswerBecomesSSE covers an Anthropic-protocol provider that
// answers a streaming request with a complete JSON message, including a tool
// use whose input must survive the re-encoding.
func TestAnthropicJSONAnswerBecomesSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m",
			"content":[{"type":"text","text":"ok"},{"type":"tool_use","id":"toolu_1","name":"f","input":{"a":1}}],
			"stop_reason":"tool_use","usage":{"input_tokens":3,"output_tokens":2}}`)
	}))
	defer upstream.Close()

	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	user, _ := s.CreateUser(ctx, store.User{Email: "a@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, store.Connector{UserID: user.ID, Name: "PC"})
	provider, err := s.CreateModelProvider(ctx, store.ModelProvider{
		UserID: user.ID, Name: "A", Protocol: "anthropic_messages",
		BaseURL: upstream.URL, Credential: "sk", ModelID: "m", Enabled: true,
	})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, provider.ID); err != nil {
		t.Fatalf("default: %v", err)
	}
	_, token, err := s.IssueGatewayCredential(ctx, connector.ID, user.ID, func() (string, string, string, error) {
		return "gwc_a", HashToken("gwc_a"), "gwc_a", nil
	})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	app := New(config.Server{
		PublicURL:                    "http://127.0.0.1",
		GatewayMaxBodyBytes:          1 << 20,
		GatewayUpstreamHeaderTimeout: 10 * time.Second,
	}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/api/v2/gateway/v1/messages",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", token)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	out := rec.Body.String()
	for _, want := range []string{"message_start", `"text":"ok"`, `"partial_json":"{\"a\":1}"`, `"stop_reason":"tool_use"`, "message_stop"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
}

// TestGatewayOpenAINonStreamingReturnsJSON pins the non-streaming contract:
// a client that did not ask for a stream gets the Anthropic message JSON, not
// an event stream.
func TestGatewayOpenAINonStreamingReturnsJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"plain"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "server.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	user, _ := s.CreateUser(ctx, store.User{Email: "ns@example.com", PasswordHash: "x"})
	connector, _ := s.CreateConnector(ctx, store.Connector{UserID: user.ID, Name: "PC"})
	provider, err := s.CreateModelProvider(ctx, store.ModelProvider{
		UserID: user.ID, Name: "NS", Protocol: "openai_chat_completions",
		BaseURL: upstream.URL, AuthType: "bearer", Credential: "k", ModelID: "m", Enabled: true,
	})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if _, err := s.SetConnectorModelDefault(ctx, connector.ID, user.ID, provider.ID); err != nil {
		t.Fatalf("default: %v", err)
	}
	_, token, err := s.IssueGatewayCredential(ctx, connector.ID, user.ID, func() (string, string, string, error) {
		return "gwc_ns", HashToken("gwc_ns"), "gwc_ns", nil
	})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	app := New(config.Server{
		PublicURL:                    "http://127.0.0.1",
		GatewayMaxBodyBytes:          1 << 20,
		GatewayUpstreamHeaderTimeout: 10 * time.Second,
	}, s, slog.New(slog.NewTextHandler(io.Discard, nil)))
	req := httptest.NewRequest(http.MethodPost, "/api/v2/gateway/v1/messages",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", token)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("non-streaming answer has content type %q", ct)
	}
	var message anthropicResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &message); err != nil {
		t.Fatalf("body is not a message: %v (%s)", err, rec.Body.String())
	}
	if message.Type != "message" || len(message.Content) == 0 {
		t.Fatalf("unexpected message: %+v", message)
	}
}
