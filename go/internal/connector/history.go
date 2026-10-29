package connector

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// maxHistoryItems bounds how much of a long conversation is converted. The
// phone opens on the end of the thread; older turns are not worth the transfer.
const maxHistoryItems = 400

// maxItemText bounds one message so a pasted file cannot dominate the payload.
const maxItemText = 20000

const (
	historyPageItems        = 20
	historyValueMaxBytes    = 64 << 10
	historyPendingToolLimit = 400
)

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
	return readHistory(path, limit, true)
}

// readHistory can omit the final assistant message while its transcript is
// still changing. Completed earlier records and user prompts can then reach
// other devices without presenting an unfinished response as final.
func readHistory(path string, limit int, includePending bool) ([]protocol.HistoryItem, error) {
	if limit <= 0 || limit > maxHistoryItems {
		limit = maxHistoryItems
	}

	builder := &historyBuilder{toolIndex: map[string]int{}, limit: limit}
	if err := readHistoryFile(path, builder); err != nil {
		return nil, err
	}
	if includePending {
		builder.flush()
	}

	return builder.items, nil
}

// ReadHistoryPages converts a transcript and emits bounded pages.
//
// Only the newest maxHistoryItems entries are kept and sent. Streaming every
// page while reading uploaded the whole conversation — thousands of items for
// a long transcript — on every full sync, which made opening a session crawl.
// Older items are already in the server's timeline; the tail is what changed.
// The final page is always marked complete, including an empty transcript.
func ReadHistoryPages(
	path string, pageSize int, write func([]protocol.HistoryItem, bool) error,
) error {
	if write == nil {
		return fmt.Errorf("history page writer is required")
	}
	if pageSize <= 0 || pageSize > maxHistoryItems {
		pageSize = historyPageItems
	}
	builder := &historyBuilder{
		toolIndex: map[string]int{},
		// No writePage: the builder keeps only the newest limit items, so a
		// huge transcript cannot be buffered or uploaded whole.
		limit: maxHistoryItems,
	}
	if err := readHistoryFile(path, builder); err != nil {
		return err
	}
	builder.flush()
	if builder.err != nil {
		return builder.err
	}
	if len(builder.items) == 0 {
		return write([]protocol.HistoryItem{}, true)
	}
	items := builder.items
	for len(items) > pageSize {
		if err := write(items[:pageSize], false); err != nil {
			return err
		}
		items = items[pageSize:]
	}
	return write(items, true)
}

// maxHistoryTailBytes bounds how much of a transcript is parsed for a tail
// import. One of this user's transcripts is over a hundred megabytes; parsing
// all of it for the newest few hundred items took seconds on every request.
// Records are independent lines, so starting at a line boundary is safe.
const maxHistoryTailBytes = 16 << 20

func readHistoryFile(path string, builder *historyBuilder) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReaderSize(file, 1<<16)
	if info, err := file.Stat(); err == nil && info.Size() > maxHistoryTailBytes {
		if _, err := file.Seek(info.Size()-maxHistoryTailBytes, io.SeekStart); err == nil {
			// Drop the partial record at the window edge.
			if _, err := reader.ReadBytes('\n'); err != nil && err != io.EOF {
				return err
			}
		}
	}
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			var record historyRecord
			if json.Unmarshal(line, &record) == nil && record.Type != "" {
				// A subagent's turns are a separate thread; the phone shows the
				// main conversation only.
				if !record.IsSidechain && !record.IsMeta {
					builder.add(record)
					if builder.err != nil {
						return builder.err
					}
				}
			}
		}
		if readErr == nil {
			continue
		}
		if readErr == io.EOF {
			return nil
		}
		return readErr
	}
}

// historyBuilder assembles timeline items, merging the block-per-record
// assistant messages into one item each.
type historyBuilder struct {
	items     []protocol.HistoryItem
	toolIndex map[string]int
	limit     int
	pageSize  int
	writePage func([]protocol.HistoryItem, bool) error
	err       error

	pendingToolItems map[string]protocol.HistoryItem
	pendingToolOrder []string

	pendingID    string
	pendingStamp int64
	pendingUUID  string
	pendingText  strings.Builder
	pendingTools []contentBlock
	hasPending   bool
}

func (b *historyBuilder) add(record historyRecord) {
	if b.err != nil {
		return
	}
	if record.Type == "assistant" {
		messageID := record.Message.ID
		if b.hasPending && (messageID == "" || messageID != b.pendingID) {
			b.flush()
			b.flushFullPages()
		}
		if !b.hasPending {
			b.hasPending = true
			b.pendingID = messageID
			b.pendingStamp = parseStamp(record.Timestamp)
			b.pendingUUID = record.UUID
		}
		blocks, text := decodeContent(record.Message.Content)
		if text != "" {
			b.appendText(text)
		}
		for _, block := range blocks {
			if block.Type == "tool_use" && block.ID != "" {
				b.pendingTools = append(b.pendingTools, block)
			}
		}
		b.flushFullPages()
		return
	}

	b.flush()
	if record.Type == "user" {
		b.addUser(record)
	}
	b.flushFullPages()
}

func (b *historyBuilder) append(item protocol.HistoryItem) int {
	b.items = append(b.items, item)
	if b.writePage != nil {
		return len(b.items) - 1
	}
	if len(b.items) <= b.limit {
		return len(b.items) - 1
	}
	b.items[0] = protocol.HistoryItem{}
	b.items = b.items[1:]
	for id, index := range b.toolIndex {
		if index == 0 {
			delete(b.toolIndex, id)
		} else {
			b.toolIndex[id] = index - 1
		}
	}
	return len(b.items) - 1
}

// flush emits the assistant message accumulated so far.
func (b *historyBuilder) flush() {
	if !b.hasPending {
		return
	}
	text := strings.TrimSpace(b.pendingText.String())
	if text != "" {
		b.append(protocol.HistoryItem{
			ID:        "hist_" + firstNonEmpty(b.pendingUUID, b.pendingID),
			Type:      "message",
			Role:      "assistant",
			Status:    "done",
			Text:      truncate(text, maxItemText),
			Timestamp: b.pendingStamp,
		})
	}
	for _, block := range b.pendingTools {
		index := b.append(protocol.HistoryItem{
			ID:        "tool_" + block.ID,
			Type:      "tool",
			Role:      "tool",
			Status:    "done",
			Tool:      block.Name,
			Input:     boundedHistoryJSON(block.Input),
			Timestamp: b.pendingStamp,
		})
		b.toolIndex[block.ID] = index
	}
	b.hasPending = false
	b.pendingID = ""
	b.pendingUUID = ""
	b.pendingText.Reset()
	b.pendingTools = nil
}

func (b *historyBuilder) appendText(text string) {
	separator := ""
	if b.pendingText.Len() > 0 {
		separator = "\n\n"
	}
	remaining := maxItemText + 1 - b.pendingText.Len() - len(separator)
	if remaining <= 0 {
		return
	}
	if len(text) > remaining {
		end := remaining
		for end > 0 && !utf8.ValidString(text[:end]) {
			end--
		}
		text = text[:end]
	}
	if text == "" {
		return
	}
	b.pendingText.WriteString(separator)
	b.pendingText.WriteString(text)
}

func (b *historyBuilder) flushFullPages() {
	if b.writePage == nil || b.err != nil {
		return
	}
	for len(b.items) > b.pageSize {
		if err := b.flushPage(b.pageSize, false); err != nil {
			b.err = err
			return
		}
	}
}

func (b *historyBuilder) flushPage(count int, complete bool) error {
	if count < 0 || count > len(b.items) {
		return fmt.Errorf("invalid history page size")
	}
	page := append([]protocol.HistoryItem(nil), b.items[:count]...)
	for id, index := range b.toolIndex {
		if index < count {
			b.rememberPendingTool(id, page[index])
			delete(b.toolIndex, id)
		} else {
			b.toolIndex[id] = index - count
		}
	}
	if err := b.writePage(page, complete); err != nil {
		return err
	}
	copy(b.items, b.items[count:])
	for i := len(b.items) - count; i < len(b.items); i++ {
		if i >= 0 {
			b.items[i] = protocol.HistoryItem{}
		}
	}
	b.items = b.items[:len(b.items)-count]
	return nil
}

// cleanedContentJSON removes the runtime's own framing from a tool result.
// A notification injected while the command ran is not the command's output,
// and showing it makes the transcript look like a log file. The rewrite runs
// on the encoded value (a string or a block array alike) and is only kept when
// the result is still valid JSON.
func cleanedContentJSON(raw json.RawMessage) (json.RawMessage, bool) {
	text := string(raw)
	cleaned := cleanTranscriptText(text)
	if cleaned == text {
		return nil, false
	}
	if !json.Valid([]byte(cleaned)) {
		return nil, false
	}
	return json.RawMessage(cleaned), true
}

func (b *historyBuilder) rememberPendingTool(id string, item protocol.HistoryItem) {
	if _, exists := b.pendingToolItems[id]; !exists {
		b.pendingToolOrder = append(b.pendingToolOrder, id)
	}
	b.pendingToolItems[id] = item
	for len(b.pendingToolOrder) > historyPendingToolLimit {
		oldest := b.pendingToolOrder[0]
		b.pendingToolOrder = b.pendingToolOrder[1:]
		delete(b.pendingToolItems, oldest)
	}
}

func boundedHistoryJSON(raw json.RawMessage) json.RawMessage {
	if cleaned, ok := cleanedContentJSON(raw); ok {
		raw = cleaned
	}
	if len(raw) <= historyValueMaxBytes {
		return raw
	}
	preview := string(raw[:historyValueMaxBytes])
	encoded, _ := json.Marshal(map[string]any{
		"truncated":     true,
		"originalBytes": len(raw),
		"preview":       preview,
	})
	return encoded
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
				item, found := b.pendingToolItems[block.ToolUseID]
				if !found {
					continue
				}
				delete(b.pendingToolItems, block.ToolUseID)
				index = b.append(item)
				b.toolIndex[block.ToolUseID] = index
			}
			item := &b.items[index]
			item.Output = boundedHistoryJSON(block.Content)
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
	b.append(protocol.HistoryItem{
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
	end := limit
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return value[:end] + "\n\n… (truncated)"
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
	taskNotify    = regexp.MustCompile(`(?s)<task-notification>.*?</task-notification>`)
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
	cleaned = taskNotify.ReplaceAllString(cleaned, "")
	// The command's output itself is worth keeping; only its wrapper goes.
	cleaned = stdoutTagOpen.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}
