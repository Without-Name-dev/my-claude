package server

// OpenAI Chat Completions adapter. The gateway speaks Anthropic Messages to
// the client; when the routed provider is OpenAI-protocol, this file converts
// the request forward and the response (streaming and not) back.
//
// Honesty rules from the design document:
//   - Tool calls keep stable IDs, names, and argument JSON across both
//     directions; multiple calls stay correlated by index.
//   - Unsupported content (e.g. PDF documents) fails the request explicitly
//     rather than being silently dropped.
//   - finish_reason is never conflated: length, content filter, tool calls,
//     and normal stops map to distinct Anthropic stop reasons.
//   - Usage figures are the upstream's own numbers, never invented.
//   - A cut stream is reported as ended-with-error; nothing is padded into a
//     fake success.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// ---------------------------------------------------------------------------
// Request: Anthropic Messages -> OpenAI Chat Completions
// ---------------------------------------------------------------------------

// anthropicRequest is the subset of the Messages body the adapter reads.
// Everything it does not translate (metadata, thinking, cache_control) is
// deliberately not forwarded; the provider would not understand it.
type anthropicRequest struct {
	Model         string          `json:"model"`
	System        json.RawMessage `json:"system"`
	Messages      []anthropicMsg  `json:"messages"`
	MaxTokens     int64           `json:"max_tokens"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
	Stream        bool            `json:"stream"`
	Tools         []anthropicTool `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicBlock struct {
	Type string `json:"type"`
	// text
	Text string `json:"text"`
	// tool_use
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
	// tool_result
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	// image
	Source json.RawMessage `json:"source"`
}

// ErrUnsupportedContent marks request content the OpenAI protocol cannot
// represent; the gateway fails the request instead of dropping it silently.
var ErrUnsupportedContent = fmt.Errorf("content type is not supported by this provider protocol")

// maxToolCallArgs bounds a streamed tool-call argument accumulation buffer.
const maxToolCallArgs = 8 << 20

// convertToOpenAI builds the Chat Completions payload. modelID overrides the
// top-level model because one provider configuration carries one model.
func convertToOpenAI(req *anthropicRequest, modelID string) (map[string]any, error) {
	out := map[string]any{"model": modelID}
	if req.MaxTokens > 0 {
		out["max_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		out["stop"] = req.StopSequences
	}
	if req.Stream {
		out["stream"] = true
		// Usage arrives only when asked for; without this most providers send
		// no token counts and message_delta would report zero.
		out["stream_options"] = map[string]any{"include_usage": true}
	}

	var messages []map[string]any
	if system := systemText(req.System); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	for i := range req.Messages {
		converted, err := convertMessage(&req.Messages[i])
		if err != nil {
			return nil, err
		}
		messages = append(messages, converted...)
	}
	out["messages"] = messages

	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			schema := map[string]any{}
			if len(t.InputSchema) > 0 {
				_ = json.Unmarshal(t.InputSchema, &schema)
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  schema,
				},
			})
		}
		out["tools"] = tools
		// Anthropic tool_choice {"type":"auto"|"any"|"tool","name":...} maps
		// onto OpenAI's "auto"/"required"/named form.
		if choice := convertToolChoice(req.ToolChoice); choice != nil {
			out["tool_choice"] = choice
		}
	}
	return out, nil
}

func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

func convertToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var choice struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &choice); err != nil {
		return nil
	}
	switch choice.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "tool":
		if choice.Name != "" {
			return map[string]any{"type": "function", "function": map[string]any{"name": choice.Name}}
		}
	case "none":
		return "none"
	}
	return nil
}

// convertMessage turns one Anthropic message into one or more OpenAI
// messages. A user turn with tool results becomes one "tool" message per
// result followed by the remaining user text; an assistant turn with tool_use
// becomes one assistant message carrying text and tool_calls together.
func convertMessage(msg *anthropicMsg) ([]map[string]any, error) {
	// Content may be a plain string shorthand.
	var asString string
	if err := json.Unmarshal(msg.Content, &asString); err == nil {
		return []map[string]any{{"role": msg.Role, "content": asString}}, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(msg.Content, &blocks); err != nil {
		return nil, fmt.Errorf("message %q has unreadable content", msg.Role)
	}

	var out []map[string]any
	var textParts []string
	var toolCalls []map[string]any
	for i := range blocks {
		b := &blocks[i]
		switch b.Type {
		case "text":
			textParts = append(textParts, b.Text)
		case "thinking", "redacted_thinking":
			// Reasoning blocks (including safety-redacted ones) have no Chat
			// Completions equivalent and must not be replayed; skipping them
			// is the documented degradation for this protocol.
			continue
		case "tool_use":
			input := map[string]any{}
			if len(b.Input) > 0 {
				_ = json.Unmarshal(b.Input, &input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   b.ID,
				"type": "function",
				"function": map[string]any{
					"name":      b.Name,
					"arguments": string(orEmptyJSON(b.Input)),
				},
			})
		case "tool_result":
			// Tool results must precede the assistant turn's continuation in
			// the OpenAI ordering: emit them as separate tool messages.
			content, err := toolResultText(b.Content)
			if err != nil {
				return nil, err
			}
			entry := map[string]any{
				"role":         "tool",
				"tool_call_id": b.ToolUseID,
				"content":      content,
			}
			out = append(out, entry)
		case "image":
			if msg.Role != "user" {
				return nil, fmt.Errorf("%w: image in an assistant message", ErrUnsupportedContent)
			}
			part, err := convertImage(b.Source)
			if err != nil {
				return nil, err
			}
			// OpenAI image parts need the content to be an array.
			textParts = append(textParts, "\x00IMAGE\x00"+mustJSON(part))
		default:
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedContent, b.Type)
		}
	}

	text := strings.Join(textParts, "")
	role := msg.Role
	if role == "assistant" {
		// An assistant turn made only of skipped thinking blocks has nothing
		// to send; emitting it with null content and no tool calls is rejected
		// by every OpenAI-compatible server.
		if text != "" || len(toolCalls) > 0 {
			entry := map[string]any{"role": "assistant", "content": nilIfEmpty(text)}
			if len(toolCalls) > 0 {
				entry["tool_calls"] = toolCalls
			}
			out = append(out, entry)
		}
		return out, nil
	}
	// A user turn that carried only tool results has already been emitted as
	// tool messages. Adding an empty user message after them turns the usual
	// tool loop into a request most providers reject.
	if text == "" {
		return out, nil
	}
	if strings.Contains(text, "\x00IMAGE\x00") {
		out = append(out, map[string]any{"role": role, "content": splitImageContent(text)})
	} else {
		out = append(out, map[string]any{"role": role, "content": text})
	}
	return out, nil
}

// splitImageContent turns the IMAGE markers emitted by convertMessage into a
// mixed text/image_url content array.
func splitImageContent(text string) []map[string]any {
	var parts []map[string]any
	const marker = "\x00IMAGE\x00"
	for {
		idx := strings.Index(text, marker)
		if idx < 0 {
			if text != "" {
				parts = append(parts, map[string]any{"type": "text", "text": text})
			}
			return parts
		}
		if idx > 0 {
			parts = append(parts, map[string]any{"type": "text", "text": text[:idx]})
		}
		text = text[idx+len(marker):]
		end := strings.Index(text, marker)
		var raw string
		if end < 0 {
			raw, text = text, ""
		} else {
			raw, text = text[:end], text[end:]
		}
		var part map[string]any
		_ = json.Unmarshal([]byte(raw), &part)
		parts = append(parts, part)
	}
}

func convertImage(source json.RawMessage) (map[string]any, error) {
	var src struct {
		Type      string `json:"type"`
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
		URL       string `json:"url"`
	}
	if err := json.Unmarshal(source, &src); err != nil {
		return nil, fmt.Errorf("unreadable image source")
	}
	switch src.Type {
	case "base64":
		return map[string]any{
			"type": "image_url",
			"image_url": map[string]any{
				"url": "data:" + src.MediaType + ";base64," + src.Data,
			},
		}, nil
	case "url":
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": src.URL},
		}, nil
	default:
		return nil, fmt.Errorf("%w: image source %q", ErrUnsupportedContent, src.Type)
	}
}

func toolResultText(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString, nil
	}
	var blocks []anthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, b := range blocks {
			switch b.Type {
			case "text":
				parts = append(parts, b.Text)
			case "image":
				// A tool result carrying an image cannot be represented in a
				// Chat Completions tool message. Failing is honest; silently
				// replacing the screenshot with text would answer blind.
				return "", fmt.Errorf("%w: image in a tool result", ErrUnsupportedContent)
			default:
				// A block type the protocol cannot carry must fail the request,
				// not vanish from the tool result.
				return "", fmt.Errorf("%w: tool_result %s", ErrUnsupportedContent, b.Type)
			}
		}
		return strings.Join(parts, "\n"), nil
	}
	return string(raw), nil
}

func orEmptyJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func mustJSON(v any) string {
	out, _ := json.Marshal(v)
	return string(out)
}

// ---------------------------------------------------------------------------
// Response: OpenAI -> Anthropic (non-streaming)
// ---------------------------------------------------------------------------

type openaiResponse struct {
	ID      string         `json:"id"`
	Model   string         `json:"model"`
	Choices []openaiChoice `json:"choices"`
	Usage   *openaiUsage   `json:"usage"`
	Error   *openaiError   `json:"error"`
}

type openaiChoice struct {
	Index        int            `json:"index"`
	FinishReason string         `json:"finish_reason"`
	Message      openaiMesssage `json:"message"`
}

type openaiMesssage struct {
	Role      string          `json:"role"`
	Content   *string         `json:"content"`
	ToolCalls []openaiToolUse `json:"tool_calls"`
}

type openaiToolUse struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Index    *int   `json:"index"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openaiUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

type openaiError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    any    `json:"code"`
}

// anthropicResponse is the non-streaming answer shape.
type anthropicResponse struct {
	ID           string           `json:"id"`
	Type         string           `json:"type"`
	Role         string           `json:"role"`
	Model        string           `json:"model"`
	Content      []map[string]any `json:"content"`
	StopReason   *string          `json:"stop_reason"`
	StopSequence *string          `json:"stop_sequence"`
	Usage        anthropicUsage   `json:"usage"`
}

type anthropicUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func convertStopReason(finish string) string {
	switch finish {
	case "length":
		return "max_tokens"
	case "tool_calls", "function_call":
		return "tool_use"
	case "content_filter":
		return "refusal"
	case "stop":
		return "end_turn"
	default:
		return "end_turn"
	}
}

func convertFromOpenAI(resp *openaiResponse, requestedModel string) *anthropicResponse {
	out := &anthropicResponse{
		ID:    orDefault(resp.ID, "msg_gateway"),
		Type:  "message",
		Role:  "assistant",
		Model: requestedModel,
	}
	if resp.Usage != nil {
		out.Usage = anthropicUsage{InputTokens: resp.Usage.PromptTokens, OutputTokens: resp.Usage.CompletionTokens}
	}
	if len(resp.Choices) == 0 {
		reason := "end_turn"
		out.StopReason = &reason
		out.Content = []map[string]any{}
		return out
	}
	choice := resp.Choices[0]
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		out.Content = append(out.Content, map[string]any{"type": "text", "text": *choice.Message.Content})
	}
	for _, call := range choice.Message.ToolCalls {
		args := json.RawMessage(orDefault(call.Function.Arguments, "{}"))
		if !json.Valid(args) {
			args = json.RawMessage("{}")
		}
		out.Content = append(out.Content, map[string]any{
			"type": "tool_use", "id": call.ID, "name": call.Function.Name, "input": args,
		})
	}
	reason := convertStopReason(choice.FinishReason)
	out.StopReason = &reason
	return out
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// ---------------------------------------------------------------------------
// Forwarding entry point
// ---------------------------------------------------------------------------

// forwardOpenAI converts and forwards to an OpenAI-protocol provider and
// converts the answer back, streaming included.
func (g *gateway) forwardOpenAI(
	w http.ResponseWriter, r *http.Request, provider store.ModelProvider,
	body []byte, streaming bool, metric *requestMetric, finishSummary summaryFinisher,
) {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		g.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"the request body is not a valid Messages JSON object")
		return
	}
	payload, err := convertToOpenAI(&req, provider.ModelID)
	if err != nil {
		failure := err.Error()
		finishSummary("failed", nil, 0, "", &failure)
		g.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		g.writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "could not encode the converted request")
		return
	}
	upstream, err := g.upstreamRequest(r.Context(), provider, http.MethodPost, openaiChatURL(provider.BaseURL), r, encoded, streaming)
	if err != nil {
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error", "could not build the provider request")
		return
	}
	resp, err := g.client.Do(upstream)
	if err != nil {
		metric.recordError()
		failure := "upstream unreachable"
		finishSummary("failed", nil, 0, "", &failure)
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"the provider could not be reached: "+provider.Name)
		return
	}
	defer resp.Body.Close()
	if streaming {
		metric.recordStream()
	}
	upstreamRequestID := resp.Header.Get("x-request-id")
	if upstreamRequestID == "" {
		upstreamRequestID = resp.Header.Get("request-id")
	}

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		metric.recordError()
		status := resp.StatusCode
		failure := "provider returned a redirect"
		finishSummary("failed", &status, 0, upstreamRequestID, &failure)
		g.writeAnthropicError(w, http.StatusBadGateway, "api_error",
			"the provider returned a redirect; point the provider URL at its final destination")
		return
	}

	if resp.StatusCode >= 400 {
		limited, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		status := resp.StatusCode
		metric.recordError()
		failure := fmt.Sprintf("upstream status %d", status)
		finishSummary("failed", &status, int64(len(limited)), upstreamRequestID, &failure)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(limited)
		return
	}

	if !streaming {
		buffered, err := io.ReadAll(io.LimitReader(resp.Body, aBodyLimit))
		if err != nil {
			failure := "upstream read failed"
			finishSummary("failed", &resp.StatusCode, 0, upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error", "the provider response could not be read")
			return
		}
		var openaiResp openaiResponse
		if err := json.Unmarshal(buffered, &openaiResp); err != nil || len(openaiResp.Choices) == 0 && openaiResp.Error == nil {
			failure := "upstream response was not a chat completion"
			finishSummary("failed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error", "the provider response was not a chat completion")
			return
		}
		if openaiResp.Error != nil {
			failure := openaiResp.Error.Message
			finishSummary("failed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error", openaiResp.Error.Message)
			return
		}
		answer := convertFromOpenAI(&openaiResp, req.Model)
		// A non-streaming request gets the Anthropic message JSON, exactly as
		// /v1/messages without stream is defined.
		out, _ := json.Marshal(answer)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(out)
		metric.recordBytes(int64(len(out)))
		finishSummary("completed", &resp.StatusCode, int64(len(out)), upstreamRequestID, nil)
		return
	}

	// Some providers answer a streaming request with a plain JSON body. That
	// is a complete answer, not an empty stream, so it converts as one.
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		buffered, err := io.ReadAll(io.LimitReader(resp.Body, aBodyLimit))
		if err != nil {
			failure := "upstream read failed"
			finishSummary("failed", &resp.StatusCode, 0, upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error", "the provider response could not be read")
			return
		}
		var openaiResp openaiResponse
		if err := json.Unmarshal(buffered, &openaiResp); err != nil {
			failure := "upstream response was not a chat completion"
			finishSummary("failed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error", "the provider response was not a chat completion")
			return
		}
		if openaiResp.Error != nil {
			failure := openaiResp.Error.Message
			finishSummary("failed", &resp.StatusCode, int64(len(buffered)), upstreamRequestID, &failure)
			g.writeAnthropicError(w, http.StatusBadGateway, "api_error", openaiResp.Error.Message)
			return
		}
		answer := convertFromOpenAI(&openaiResp, req.Model)
		// The caller asked for a stream, so a complete JSON answer is replayed
		// as the equivalent SSE sequence. Handing back a JSON body here would
		// break the client's event parser.
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		written := writeSyntheticSSE(w, answer)
		metric.recordBytes(written)
		finishSummary("completed", &resp.StatusCode, written, upstreamRequestID, nil)
		return
	}

	// Streaming: convert OpenAI chunks into Anthropic SSE events.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	converter := newSSEConverter(w, req.Model)
	converter.emitMessageStart()
	var copied int64
	sawEnd := false
	scanner := bufio.NewScanner(resp.Body)
	// One line can carry a whole tool-call argument delta, so the scanner
	// bound matches the argument cap instead of capping it at 1 MiB.
	scanner.Buffer(make([]byte, 0, 64<<10), maxToolCallArgs+64<<10)
	for scanner.Scan() {
		line := scanner.Bytes()
		copied += int64(len(line)) + 1
		metric.recordBytes(int64(len(line)) + 1)
		if len(line) == 0 {
			converter.flushPending()
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue // comments and event names pass by
		}
		data := bytes.TrimSpace(line[len("data:"):])
		if bytes.Equal(data, []byte("[DONE]")) {
			sawEnd = true
			break
		}
		if err := converter.ingest(data); err != nil {
			// An error frame, an unreadable chunk, or an over-long tool call
			// is a failed stream: tell the client instead of inventing a
			// successful end.
			converter.fail(err.Error())
			failure := err.Error()
			finishSummary("failed", &resp.StatusCode, copied, upstreamRequestID, &failure)
			return
		}
		if converter.sawFinishReason {
			sawEnd = true
		}
	}
	if err := scanner.Err(); err != nil {
		converter.fail("stream interrupted")
		failure := "stream interrupted"
		finishSummary("failed", &resp.StatusCode, copied, upstreamRequestID, &failure)
		return
	}
	if r.Context().Err() != nil {
		finishSummary("cancelled", &resp.StatusCode, copied, upstreamRequestID, nil)
		return
	}
	if !sawEnd {
		// The upstream closed without a finish reason or [DONE]. Reporting a
		// normal message_stop here would label a truncated answer complete.
		converter.fail("the provider stream ended without completing the message")
		failure := "stream ended without a finish reason"
		finishSummary("failed", &resp.StatusCode, copied, upstreamRequestID, &failure)
		return
	}
	if err := converter.finish(); err != nil {
		converter.fail(err.Error())
		failure := err.Error()
		finishSummary("failed", &resp.StatusCode, copied, upstreamRequestID, &failure)
		return
	}
	finishSummary("completed", &resp.StatusCode, copied, upstreamRequestID, nil)
}

// writeSyntheticSSE renders a complete Anthropic message as the SSE sequence
// a streaming client expects: message_start, one block per content entry, the
// terminating message_delta and message_stop. It returns the bytes written.
func writeSyntheticSSE(w http.ResponseWriter, answer *anthropicResponse) int64 {
	var written int64
	emit := func(name string, payload any) {
		out, _ := json.Marshal(payload)
		n, _ := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, out)
		written += int64(n)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}
	stopReason := "end_turn"
	if answer.StopReason != nil {
		stopReason = *answer.StopReason
	}
	emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": answer.ID, "type": "message", "role": "assistant",
			"model": answer.Model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens": answer.Usage.InputTokens, "output_tokens": 0,
			},
		},
	})
	index := 0
	for _, block := range answer.Content {
		switch block["type"] {
		case "text":
			text, _ := block["text"].(string)
			emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
			emit("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": index,
				"delta": map[string]any{"type": "text_delta", "text": text},
			})
		case "tool_use":
			emit("content_block_start", map[string]any{
				"type": "content_block_start", "index": index,
				"content_block": map[string]any{
					"type": "tool_use", "id": block["id"], "name": block["name"], "input": map[string]any{},
				},
			})
			// input is a RawMessage when the answer came from the OpenAI
			// converter and a decoded object when it came from an Anthropic
			// upstream; re-marshalling handles both.
			if input, ok := block["input"]; ok {
				if raw, err := json.Marshal(input); err == nil && len(raw) > 0 && string(raw) != "null" {
					emit("content_block_delta", map[string]any{
						"type": "content_block_delta", "index": index,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": string(raw)},
					})
				}
			}
		default:
			// Skipped block types (thinking) must not leave holes in the
			// indexes the client walks.
			continue
		}
		emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
		index++
	}
	emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": answer.Usage.OutputTokens},
	})
	emit("message_stop", map[string]any{"type": "message_stop"})
	return written
}

// ---------------------------------------------------------------------------
// Streaming conversion
// ---------------------------------------------------------------------------

// sseConverter turns OpenAI chat.completion.chunk frames into an Anthropic
// event stream. State is bounded: the tool-call argument accumulators are
// capped, and nothing else is buffered whole.
type sseConverter struct {
	w          http.ResponseWriter
	model      string
	messageID  string
	blockIndex int
	openText   bool
	// toolBlocks accumulates streamed tool-call metadata by OpenAI index.
	toolBlocks      map[int]*toolBlockState
	stopReason      string
	sawFinishReason bool
	usage           anthropicUsage
	// totalArgs bounds argument memory for the whole stream, not just one
	// call: parallel tool calls must not multiply the per-call limit.
	totalArgs int
}

type toolBlockState struct {
	blockIndex int
	id         string
	name       string
	args       []byte
	emitted    bool
}

func newSSEConverter(w http.ResponseWriter, model string) *sseConverter {
	return &sseConverter{
		w:          w,
		model:      model,
		messageID:  "msg_" + store.NewID("gw"),
		toolBlocks: map[int]*toolBlockState{},
	}
}

func (c *sseConverter) event(name string, payload any) {
	out, _ := json.Marshal(payload)
	fmt.Fprintf(c.w, "event: %s\ndata: %s\n\n", name, out)
	if flusher, ok := c.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (c *sseConverter) emitMessageStart() {
	c.event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": c.messageID, "type": "message", "role": "assistant",
			"model": c.model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": c.usage.InputTokens, "output_tokens": 0},
		},
	})
}

// ingest consumes one OpenAI chunk payload. A nil error means the chunk was
// understood; anything else must fail the stream, because silently skipping a
// chunk would hand the client a partly invented answer.
func (c *sseConverter) ingest(data []byte) error {
	var chunk struct {
		Choices []struct {
			Delta struct {
				Role      string          `json:"role"`
				Content   string          `json:"content"`
				ToolCalls []openaiToolUse `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string `json:"finish_reason"`
		} `json:"choices"`
		Usage *openaiUsage `json:"usage"`
		Error *openaiError `json:"error"`
	}
	if err := json.Unmarshal(data, &chunk); err != nil {
		return fmt.Errorf("the provider sent an unreadable chunk")
	}
	if chunk.Error != nil && chunk.Error.Message != "" {
		return fmt.Errorf("provider error: %s", chunk.Error.Message)
	}
	if chunk.Usage != nil {
		c.usage.InputTokens = chunk.Usage.PromptTokens
		c.usage.OutputTokens = chunk.Usage.CompletionTokens
	}
	for _, choice := range chunk.Choices {
		if choice.FinishReason != nil {
			c.stopReason = convertStopReason(*choice.FinishReason)
			c.sawFinishReason = true
			continue
		}
		if choice.Delta.Content != "" {
			if !c.openText {
				c.openText = true
				c.event("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": c.blockIndex,
					"content_block": map[string]any{
						"type": "text", "text": "",
					},
				})
			}
			c.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": c.blockIndex,
				"delta": map[string]any{"type": "text_delta", "text": choice.Delta.Content},
			})
		}
		for _, call := range choice.Delta.ToolCalls {
			index := 0
			if call.Index != nil {
				index = *call.Index
			}
			block, ok := c.toolBlocks[index]
			if !ok {
				block = &toolBlockState{blockIndex: -1}
				c.toolBlocks[index] = block
			}
			if call.ID != "" {
				block.id = call.ID
			}
			if call.Function.Name != "" {
				block.name = call.Function.Name
			}
			if len(call.Function.Arguments) > 0 {
				if len(block.args)+len(call.Function.Arguments) > maxToolCallArgs ||
					c.totalArgs+len(call.Function.Arguments) > maxToolCallArgs {
					// Truncating and emitting "{}" below would run the tool
					// with empty arguments; failing is the honest outcome.
					return fmt.Errorf("the streamed tool calls exceeded the %d-byte argument limit", maxToolCallArgs)
				}
				block.args = append(block.args, call.Function.Arguments...)
				c.totalArgs += len(call.Function.Arguments)
			}
			// Emit once the id and name are known; arguments stream after.
			if !block.emitted && block.id != "" && block.name != "" {
				block.emitted = true
				if c.openText {
					c.closeText()
				}
				block.blockIndex = c.blockIndex
				c.blockIndex++
				c.event("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": block.blockIndex,
					"content_block": map[string]any{
						"type": "tool_use", "id": block.id, "name": block.name, "input": map[string]any{},
					},
				})
			}
		}
	}
	return nil
}

// closeText ends the open text block before a tool block starts.
func (c *sseConverter) closeText() {
	if !c.openText {
		return
	}
	c.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.blockIndex})
	c.blockIndex++
	c.openText = false
}

// flushPending is called on blank lines to keep the stream warm without
// inventing content.
func (c *sseConverter) flushPending() {
	if flusher, ok := c.w.(http.Flusher); ok {
		flusher.Flush()
	}
}

// finish closes open blocks and emits the terminating events. Tool-call
// arguments accumulated across the stream are flushed as one final
// input_json_delta per tool block. Everything is validated before the first
// event is written, so a malformed tool call fails the stream instead of
// producing a half-written answer or inventing empty arguments.
func (c *sseConverter) finish() error {
	indices := make([]int, 0, len(c.toolBlocks))
	for i := range c.toolBlocks {
		indices = append(indices, i)
	}
	for i := 0; i < len(indices); i++ {
		for j := i + 1; j < len(indices); j++ {
			if indices[j] < indices[i] {
				indices[i], indices[j] = indices[j], indices[i]
			}
		}
	}
	for _, idx := range indices {
		block := c.toolBlocks[idx]
		if !block.emitted {
			return fmt.Errorf("a tool call arrived without an id or name")
		}
		if len(block.args) > 0 && !json.Valid(block.args) {
			return fmt.Errorf("a tool call's arguments were not valid JSON")
		}
	}

	if c.openText {
		c.closeText()
	}
	for _, idx := range indices {
		block := c.toolBlocks[idx]
		if len(block.args) > 0 {
			c.event("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": block.blockIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(block.args)},
			})
		}
		c.event("content_block_stop", map[string]any{"type": "content_block_stop", "index": block.blockIndex})
	}
	if c.stopReason == "" {
		c.stopReason = "end_turn"
	}
	c.event("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": c.stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": c.usage.OutputTokens},
	})
	c.event("message_stop", map[string]any{"type": "message_stop"})
	return nil
}

// fail ends the stream with an SSE error event so the client sees a
// terminated, not truncated-success, stream.
func (c *sseConverter) fail(reason string) {
	c.event("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": reason},
	})
}
