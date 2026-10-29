package server

import (
	"bytes"
	"encoding/json"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
	"net/http"
	"strings"
	"testing"
)

func TestRewriteModel(t *testing.T) {
	body := []byte(`{"model":"claude-old","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}],"system":"keep me"}`)
	out, streaming, ok := rewriteModel(body, "new-model")
	if !ok || !streaming {
		t.Fatalf("expected ok streaming rewrite, got ok=%v streaming=%v", ok, streaming)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rewritten body is not JSON: %v", err)
	}
	if doc["model"] != "new-model" {
		t.Fatalf("model not rewritten: %v", doc["model"])
	}
	if doc["system"] != "keep me" || doc["max_tokens"] != float64(16) {
		t.Fatalf("unrelated fields changed: %v", doc)
	}
}

func TestRewriteModelNonObject(t *testing.T) {
	if _, _, ok := rewriteModel([]byte(`[1,2]`), "m"); ok {
		t.Fatal("array body must be rejected")
	}
	if _, _, ok := rewriteModel([]byte(`not json`), "m"); ok {
		t.Fatal("invalid JSON must be rejected")
	}
}

func TestAnthropicMessagesURL(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com":             "https://api.example.com/v1/messages",
		"https://api.example.com/":            "https://api.example.com/v1/messages",
		"https://api.example.com/v1":          "https://api.example.com/v1/messages",
		"https://api.example.com/v1/":         "https://api.example.com/v1/messages",
		"https://api.example.com/v1/messages": "https://api.example.com/v1/messages",
	}
	for in, want := range cases {
		if got := anthropicMessagesURL(in); got != want {
			t.Errorf("anthropicMessagesURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenAIChatURL(t *testing.T) {
	cases := map[string]string{
		"https://api.example.com":                     "https://api.example.com/v1/chat/completions",
		"https://api.example.com/v1":                  "https://api.example.com/v1/chat/completions",
		"https://api.example.com/v1/chat/completions": "https://api.example.com/v1/chat/completions",
	}
	for in, want := range cases {
		if got := openaiChatURL(in); got != want {
			t.Errorf("openaiChatURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConvertToOpenAIBasic(t *testing.T) {
	req := &anthropicRequest{
		Model:     "irrelevant",
		System:    json.RawMessage(`"be brief"`),
		MaxTokens: 32,
		Messages: []anthropicMsg{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
		Tools: []anthropicTool{{
			Name: "get_weather", Description: "weather",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`),
		}},
	}
	out, err := convertToOpenAI(req, "target-model")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if out["model"] != "target-model" {
		t.Fatalf("model not replaced: %v", out["model"])
	}
	msgs := out["messages"].([]map[string]any)
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "be brief" {
		t.Fatalf("system message wrong: %v", msgs[0])
	}
	tools := out["tools"].([]map[string]any)
	if tools[0]["type"] != "function" {
		t.Fatalf("tool not function type: %v", tools[0])
	}
	fn := tools[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("tool name lost: %v", fn)
	}
}

func TestConvertToolRoundTrip(t *testing.T) {
	// Assistant tool_use -> OpenAI tool_calls; user tool_result -> tool message.
	req := &anthropicRequest{
		Messages: []anthropicMsg{
			{Role: "assistant", Content: json.RawMessage(`[
				{"type":"text","text":"calling"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}
			]`)},
			{Role: "user", Content: json.RawMessage(`[
				{"type":"tool_result","tool_use_id":"toolu_1","content":"sunny"}
			]`)},
		},
	}
	out, err := convertToOpenAI(req, "m")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	msgs := out["messages"].([]map[string]any)
	// First message: assistant with text + tool_calls.
	assistant := msgs[0]
	if assistant["role"] != "assistant" {
		t.Fatalf("first message role: %v", assistant["role"])
	}
	calls := assistant["tool_calls"].([]map[string]any)
	if calls[0]["id"] != "toolu_1" {
		t.Fatalf("tool call id lost: %v", calls[0])
	}
	// Second: the tool result as its own tool message.
	toolMsg := msgs[1]
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" || toolMsg["content"] != "sunny" {
		t.Fatalf("tool result message wrong: %v", toolMsg)
	}
}

func TestConvertUnsupportedContentFails(t *testing.T) {
	req := &anthropicRequest{
		Messages: []anthropicMsg{
			{Role: "user", Content: json.RawMessage(`[{"type":"document","source":{"type":"base64"}}]`)},
		},
	}
	if _, err := convertToOpenAI(req, "m"); err == nil {
		t.Fatal("document content must fail the request, not be dropped")
	}
}

func TestConvertStopReason(t *testing.T) {
	cases := map[string]string{
		"stop": "end_turn", "length": "max_tokens", "tool_calls": "tool_use",
		"content_filter": "refusal",
	}
	for in, want := range cases {
		if got := convertStopReason(in); got != want {
			t.Errorf("convertStopReason(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRouteCacheEpoch(t *testing.T) {
	c := newGatewayRouteCache()
	c.entries["s:x"] = cacheEntry{decision: decisionForTest("p1"), userID: "u1", size: 10}
	epoch := c.beginRead()
	// An invalidation between read and write must win: the stale put is dropped.
	c.invalidateUser("u1")
	c.putIfEpoch("s:x", epoch, decisionForTest("p2"))
	if _, ok := c.entries["s:x"]; ok {
		t.Fatal("stale write-back after invalidation was accepted")
	}
	// A clean epoch accepts the write.
	epoch = c.beginRead()
	c.putIfEpoch("s:x", epoch, decisionForTest("p2"))
	if d, ok := c.get("s:x"); !ok || d.Provider.ID != "p2" {
		t.Fatalf("clean write not accepted: %v %v", ok, d.Provider.ID)
	}
	// Exact-key invalidation drops the single entry.
	c.invalidateKey("s:x")
	if _, ok := c.entries["s:x"]; ok {
		t.Fatal("exact-key invalidation did not drop the entry")
	}
}

func decisionForTest(id string) store.RouteDecision {
	return store.RouteDecision{Provider: store.ModelProvider{ID: id}}
}

// discardWriter accepts SSE output for converter unit tests.
type discardWriter struct{ buf bytes.Buffer }

func (d *discardWriter) Header() http.Header         { return http.Header{} }
func (d *discardWriter) Write(b []byte) (int, error) { return d.buf.Write(b) }
func (d *discardWriter) WriteHeader(int)             {}

func TestSSEConverterRejectsMalformedToolCall(t *testing.T) {
	cases := map[string][]string{
		"missing name": {
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"arguments":"{}"}}]}}]}`,
		},
		"invalid json arguments": {
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":"}}]}}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		},
	}
	for name, chunks := range cases {
		c := newSSEConverter(&discardWriter{}, "m")
		var ingestErr error
		for _, chunk := range chunks {
			if err := c.ingest([]byte(chunk)); err != nil {
				ingestErr = err
				break
			}
		}
		if ingestErr == nil {
			ingestErr = c.finish()
		}
		if ingestErr == nil {
			t.Fatalf("%s: malformed tool call was accepted", name)
		}
	}
}

func TestSSEConverterErrorFrameFails(t *testing.T) {
	c := newSSEConverter(&discardWriter{}, "m")
	if err := c.ingest([]byte(`{"error":{"message":"overloaded","type":"server_error"}}`)); err == nil {
		t.Fatal("an error frame must fail the stream")
	}
}

func TestSSEConverterValidToolCall(t *testing.T) {
	w := &discardWriter{}
	c := newSSEConverter(w, "m")
	c.emitMessageStart()
	for _, chunk := range []string{
		`{"choices":[{"delta":{"content":"hi"}}]}`,
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"{\"a\":1}"}}]}}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	} {
		if err := c.ingest([]byte(chunk)); err != nil {
			t.Fatalf("ingest: %v", err)
		}
	}
	if err := c.finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	out := w.buf.String()
	for _, want := range []string{"message_start", "message_stop", "tool_use", `"partial_json":"{\"a\":1}"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	if !c.sawFinishReason {
		t.Fatal("finish reason not recorded")
	}
}

func TestConvertToolResultOnlyTurnHasNoEmptyUserMessage(t *testing.T) {
	req := &anthropicRequest{
		Messages: []anthropicMsg{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"t1","name":"f","input":{}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]`)},
		},
	}
	out, err := convertToOpenAI(req, "m")
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	messages := out["messages"].([]map[string]any)
	if len(messages) != 2 {
		t.Fatalf("expected assistant + tool, got %d messages: %v", len(messages), messages)
	}
	if messages[1]["role"] != "tool" {
		t.Fatalf("second message should be the tool result: %v", messages[1])
	}
	for _, message := range messages {
		if message["role"] == "user" && message["content"] == nil {
			t.Fatalf("empty user message emitted: %v", messages)
		}
	}
}

func TestConvertRedactedThinkingIsSkipped(t *testing.T) {
	req := &anthropicRequest{
		Messages: []anthropicMsg{
			{Role: "assistant", Content: json.RawMessage(`[{"type":"redacted_thinking","data":"x"},{"type":"text","text":"answer"}]`)},
		},
	}
	if _, err := convertToOpenAI(req, "m"); err != nil {
		t.Fatalf("redacted_thinking must be skippable: %v", err)
	}
}
