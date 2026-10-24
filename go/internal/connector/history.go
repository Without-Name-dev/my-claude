package connector

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// maxHistoryItems bounds how much of a long conversation is converted. The
// phone opens on the end of the thread; older turns are not worth the transfer.
const maxHistoryItems = 400

// maxItemText bounds one message so a pasted file cannot dominate the payload.
const maxItemText = 20000

// historyRecord is the subset of a transcript line that becomes a timeline item.
type historyRecord struct {
	Type        string `json:"type"`
	UUID        string `json:"uuid"`
	Timestamp   string `json:"timestamp"`
	IsSidechain bool   `json:"isSidechain"`
	IsMeta      bool   `json:"isMeta"`
	Message     struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

// contentBlock covers the block kinds a message can contain.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// PathFor returns the transcript file that holds a session.
//
// Several files can claim one session id (a resumed session, or a subagent
// transcript carrying its parent's id). Picking an arbitrary map entry sometimes
// returned a tiny sidechain file, so the canonical transcript — the one named
// after the session — always wins, and the most recently written otherwise.
func (d *Discovery) PathFor(externalID string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	best := ""
	bestCanonical := false
	bestUpdated := int64(-1)
	for path, cached := range d.cache {
		if !cached.ok || cached.record.ExternalID != externalID {
			continue
		}
		canonical := strings.TrimSuffix(filepath.Base(path), ".jsonl") == externalID
		switch {
		case canonical && !bestCanonical:
			best, bestCanonical, bestUpdated = path, true, cached.record.UpdatedAt
		case canonical == bestCanonical && cached.record.UpdatedAt > bestUpdated:
			best, bestUpdated = path, cached.record.UpdatedAt
		}
	}
	return best, best != ""
}

// ReadHistory converts a transcript into timeline items.
//
// Claude Code writes one assistant message as several records that share a
// message id, each carrying a single content block: thinking, then text, then a
// tool call. Reading them one at a time and deduplicating by id keeps only the
// first block — which is usually thinking — and the whole answer disappears.
// Blocks are therefore accumulated per message id and emitted when the id
// changes.
func ReadHistory(path string, limit int) ([]protocol.HistoryItem, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	if limit <= 0 || limit > maxHistoryItems {
		limit = maxHistoryItems
	}

	builder := &historyBuilder{toolIndex: map[string]int{}}
	reader := bufio.NewReaderSize(file, 1<<16)
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var record historyRecord
			if json.Unmarshal(line, &record) == nil && record.Type != "" {
				// A subagent's turns are a separate thread; the phone shows the
				// main conversation only.
				if !record.IsSidechain && !record.IsMeta {
					builder.add(record)
				}
			}
		}
		if readErr != nil {
			break
		}
	}
	builder.flush()

	items := builder.items
	if len(items) > limit {
		items = items[len(items)-limit:]
	}
	return items, nil
}

// historyBuilder assembles timeline items, merging the block-per-record
// assistant messages into one item each.
type historyBuilder struct {
	items     []protocol.HistoryItem
	toolIndex map[string]int

	pendingID    string
	pendingStamp int64
	pendingUUID  string
	pendingText  []string
	pendingTools []contentBlock
	hasPending   bool
}

func (b *historyBuilder) add(record historyRecord) {
	if record.Type == "assistant" {
		messageID := record.Message.ID
		if b.hasPending && (messageID == "" || messageID != b.pendingID) {
			b.flush()
		}
		if !b.hasPending {
			b.hasPending = true
			b.pendingID = messageID
			b.pendingStamp = parseStamp(record.Timestamp)
			b.pendingUUID = record.UUID
		}
		blocks, text := decodeContent(record.Message.Content)
		if text != "" {
			b.pendingText = append(b.pendingText, text)
		}
		for _, block := range blocks {
			if block.Type == "tool_use" && block.ID != "" {
				b.pendingTools = append(b.pendingTools, block)
			}
		}
		return
	}

	b.flush()
	if record.Type != "user" {
		return
	}
	b.addUser(record)
}

// flush emits the assistant message accumulated so far.
func (b *historyBuilder) flush() {
	if !b.hasPending {
		return
	}
	text := strings.TrimSpace(strings.Join(b.pendingText, "\n\n"))
	if text != "" {
		b.items = append(b.items, protocol.HistoryItem{
			ID:        "hist_" + firstNonEmpty(b.pendingUUID, b.pendingID),
			Type:      "message",
			Role:      "assistant",
			Status:    "done",
			Text:      truncate(text, maxItemText),
			Timestamp: b.pendingStamp,
		})
	}
	for _, block := range b.pendingTools {
		b.toolIndex[block.ID] = len(b.items)
		b.items = append(b.items, protocol.HistoryItem{
			ID:        "tool_" + block.ID,
			Type:      "tool",
			Role:      "tool",
			Status:    "done",
			Tool:      block.Name,
			Input:     block.Input,
			Timestamp: b.pendingStamp,
		})
	}
	b.hasPending = false
	b.pendingID = ""
	b.pendingUUID = ""
	b.pendingText = nil
	b.pendingTools = nil
}

// addUser handles a person's line: either their words, or the tool results for
// the previous assistant turn.
func (b *historyBuilder) addUser(record historyRecord) {
	blocks, text := decodeContent(record.Message.Content)
	stamp := parseStamp(record.Timestamp)

	var results []contentBlock
	for _, block := range blocks {
		if block.Type == "tool_result" {
			results = append(results, block)
		}
	}
	if len(results) > 0 {
		for _, block := range results {
			index, ok := b.toolIndex[block.ToolUseID]
			if !ok {
				continue
			}
			item := &b.items[index]
			item.Output = block.Content
			if block.IsError {
				item.Status = "failed"
			}
		}
		return
	}
	text = cleanTranscriptText(text)
	if text == "" {
		return
	}
	b.items = append(b.items, protocol.HistoryItem{
		ID:        "hist_" + record.UUID,
		Type:      "message",
		Role:      "user",
		Status:    "done",
		Text:      truncate(text, maxItemText),
		Timestamp: stamp,
	})
}

// decodeContent splits a message content field into blocks and plain text.
func decodeContent(raw json.RawMessage) ([]contentBlock, string) {
	if len(raw) == 0 {
		return nil, ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return nil, strings.TrimSpace(text)
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
			parts = append(parts, block.Text)
		}
	}
	return blocks, strings.TrimSpace(strings.Join(parts, "\n\n"))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return "unknown"
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "\n\n… (truncated)"
}

// Transcript text arrives with terminal control sequences and slash-command
// framing baked in: ANSI colour codes from command output, and tags like
// <command-name>/model</command-name>. Showing those raw makes a conversation
// look like a log file.
var (
	ansiCSI       = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")
	ansiOSC       = regexp.MustCompile("\x1b\\][^\x07]*\x07")
	commandName   = regexp.MustCompile(`(?s)<\\?command-name>.*?<\\?/command-name>`)
	commandMsg    = regexp.MustCompile(`(?s)<\\?command-message>.*?<\\?/command-message>`)
	commandArgs   = regexp.MustCompile(`(?s)<\\?command-args>.*?<\\?/command-args>`)
	systemRemind  = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	stdoutTagOpen = regexp.MustCompile(`<\\?/?local-command-stdout>`)
)

// cleanTranscriptText strips the framing that is meant for a terminal.
func cleanTranscriptText(text string) string {
	cleaned := ansiOSC.ReplaceAllString(text, "")
	cleaned = ansiCSI.ReplaceAllString(cleaned, "")
	cleaned = commandName.ReplaceAllString(cleaned, "")
	cleaned = commandMsg.ReplaceAllString(cleaned, "")
	cleaned = commandArgs.ReplaceAllString(cleaned, "")
	cleaned = systemRemind.ReplaceAllString(cleaned, "")
	// The command's output itself is worth keeping; only its wrapper goes.
	cleaned = stdoutTagOpen.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}
