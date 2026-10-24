// Package bridge speaks the phone-pane plugin's file protocol.
//
// A Claude Code session that loads the phone-pane plugin can raise real turns
// inside its own process, and that is the only delivery path that keeps the
// desktop window live while a phone drives the conversation. The plugin and
// this package talk through a directory:
//
//	queue/<n>.json   one message to submit, consumed by the plugin
//	outbox.json      the current turn, rewritten as text streams in
//	heartbeat.txt    rewritten every few seconds while the plugin is loaded
//	auth.json        the per-process token a queued message must carry
//	approval.json    the port and token of the approval long-poll endpoint
//	abort.txt        a stamp that asks the plugin to cancel its turn, followed
//	                 by the runtime session it is addressed to
//	tools.json       the tool calls seen in the running turn
//
// The field names are frozen: a plugin build already installed on the desktop
// reads them, so renaming anything silently breaks the bridge.
package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultHeartbeatTTL is how stale the heartbeat may be before the bridge is
// considered absent.
const DefaultHeartbeatTTL = 30 * time.Second

// outboxStateKeys are forwarded to the server as session state; "branch" is the
// git branch, not a duplicate of the working directory.
var outboxStateKeys = []string{"model", "effort", "cwd", "branch", "usage", "context", "costUsd"}

// Bridge is a handle on one bridge directory.
type Bridge struct {
	Dir string

	mu      sync.Mutex
	token   string
	lastSeq int
}

// New returns a bridge rooted at dir.
func New(dir string) *Bridge { return &Bridge{Dir: dir} }

// Enabled reports whether delivery through the plugin is switched on.
func Enabled() bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv("AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE")))
	return value != "" && value != "0" && value != "false" && value != "no" && value != "off"
}

// DefaultDir is where the plugin and connector meet when nothing overrides it.
func DefaultDir() string {
	if explicit := strings.TrimSpace(os.Getenv("AGENT_CONNECTOR_CLAUDE_BRIDGE_DIR")); explicit != "" {
		return explicit
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".claude", "phone-bridge")
	}
	return filepath.Join(home, ".claude", "phone-bridge")
}

// Path helpers. Each file the plugin touches has exactly one here.
func (b *Bridge) QueueDir() string      { return filepath.Join(b.Dir, "queue") }
func (b *Bridge) OutboxPath() string    { return filepath.Join(b.Dir, "outbox.json") }
func (b *Bridge) HeartbeatPath() string { return filepath.Join(b.Dir, "heartbeat.txt") }
func (b *Bridge) AbortPath() string     { return filepath.Join(b.Dir, "abort.txt") }
func (b *Bridge) ToolsPath() string     { return filepath.Join(b.Dir, "tools.json") }
func (b *Bridge) TokenPath() string     { return filepath.Join(b.Dir, "auth.json") }
func (b *Bridge) ApprovalPath() string  { return filepath.Join(b.Dir, "approval.json") }

// Token returns the queue token, creating it on first use. A queued message
// must carry it or the plugin refuses to submit it as the person's words.
func (b *Bridge) Token() (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tokenLocked()
}

// tokenLocked is the body of Token for callers that already hold b.mu.
func (b *Bridge) tokenLocked() (string, error) {
	if b.token != "" {
		return b.token, nil
	}
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	if err := writeAtomic(b.TokenPath(), mustJSON(map[string]string{"token": token})); err != nil {
		return "", fmt.Errorf("write queue token: %w", err)
	}
	b.token = token
	return token, nil
}

// QueueMessage is one entry in queue/. The field names match the plugin.
type QueueMessage struct {
	ID        string `json:"id"`
	Action    string `json:"action"`
	Text      string `json:"text,omitempty"`
	Command   string `json:"command,omitempty"`
	Args      string `json:"args,omitempty"`
	QueuedAt  int64  `json:"queuedAt"`
	Token     string `json:"token"`
	SessionID string `json:"sessionId,omitempty"`
	// ExternalSessionID is the runtime's own session id (the transcript file
	// name). The plugin compares it with its own session so a message is only
	// submitted into the conversation it was addressed to.
	ExternalSessionID string `json:"externalSessionId,omitempty"`
}

// Submission describes what the caller needs to follow the turn it queued.
type Submission struct {
	QueuedAt int64
	// PreviousTurnID is the turn the outbox already showed. The plugin rewrites
	// the same file, so a snapshot carrying this id predates the new message.
	PreviousTurnID string
}

// Submit queues a prompt and returns the handle used to follow it.
func (b *Bridge) Submit(sessionID, externalSessionID, messageID, text, action string) (*Submission, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("empty message")
	}
	previous, err := b.previousTurnID()
	if err != nil {
		return nil, err
	}
	msg := QueueMessage{
		ID:                messageID,
		Action:            action,
		Text:              text,
		QueuedAt:          time.Now().UnixMilli(),
		SessionID:         sessionID,
		ExternalSessionID: externalSessionID,
	}
	if msg.ID == "" {
		msg.ID = "bridge-" + strconv.FormatInt(msg.QueuedAt, 10)
	}
	if err := b.writeQueue(msg); err != nil {
		return nil, err
	}
	return &Submission{QueuedAt: msg.QueuedAt, PreviousTurnID: previous}, nil
}

// previousTurnID reads the turn the outbox showed before this message was
// queued. The plugin rewrites the file while text streams in, so a read can
// catch it half-written; retrying beats treating a torn file as "no previous
// turn", which would let the watcher adopt a stale turn as the answer.
func (b *Bridge) previousTurnID() (string, error) {
	var last error
	for attempt := 0; attempt < 10; attempt++ {
		box, err := b.ReadOutbox()
		if err == nil {
			if box == nil {
				return "", nil
			}
			return box.TurnID, nil
		}
		last = err
		time.Sleep(20 * time.Millisecond)
	}
	return "", fmt.Errorf("read outbox: %w", last)
}

// writeQueueRaw queues an already-shaped message. Both prompts and commands go
// through here so the sequence number and token stay in one place.
func (b *Bridge) writeQueueRaw(payload map[string]any) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	dir := b.QueueDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	token, err := b.tokenLocked()
	if err != nil {
		return err
	}
	payload["token"] = token

	seq := b.lastSeq
	if next := b.nextSeq(dir); next > seq {
		seq = next
	}
	seq++
	b.lastSeq = seq

	final := filepath.Join(dir, strconv.Itoa(seq)+".json")
	return writeAtomic(final, mustJSON(payload))
}

func (b *Bridge) writeQueue(msg QueueMessage) error {
	payload := map[string]any{
		"id":       msg.ID,
		"action":   msg.Action,
		"queuedAt": msg.QueuedAt,
	}
	if msg.Text != "" {
		payload["text"] = msg.Text
	}
	if msg.Command != "" {
		payload["command"] = msg.Command
	}
	if msg.Args != "" {
		payload["args"] = msg.Args
	}
	if msg.SessionID != "" {
		payload["sessionId"] = msg.SessionID
	}
	if msg.ExternalSessionID != "" {
		payload["externalSessionId"] = msg.ExternalSessionID
	}
	return b.writeQueueRaw(payload)
}

// nextSeq is the highest sequence already used, counting both the files still
// in the queue and the cursor the plugin has consumed up to.
//
// The cursor matters: the plugin only reads files numbered above it, so if the
// queue is emptied and numbering restarted at 1, every new message would sit
// there invisible forever.
func (b *Bridge) nextSeq(dir string) int {
	best := highestSeq(dir)
	if cursor := readCursor(b.Dir); cursor > best {
		best = cursor
	}
	return best
}

// highestSeq scans the queue for the largest sequence already present.
func highestSeq(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	best := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSuffix(name, ".json"))
		if err == nil && n > best {
			best = n
		}
	}
	return best
}

// readCursor reads the plugin's progress marker, which lives beside the queue.
func readCursor(dir string) int {
	raw, err := os.ReadFile(filepath.Join(dir, "cursor.txt"))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0
	}
	return n
}

// Outbox is the plugin's snapshot of the turn it is running.
type Outbox struct {
	// SessionID is the runtime session that wrote the snapshot. Older plugin
	// builds did not write it, so it can be empty.
	SessionID string
	TurnID    string
	Status    string
	Chunks    int
	Text      string
	TS        int64
	State     map[string]any
}

// BelongsTo reports whether the snapshot was written by one of the given
// sessions. An unattributed snapshot comes from an older plugin and is
// accepted rather than ignored.
func (o *Outbox) BelongsTo(sessionIDs ...string) bool {
	return matchesSession(o.SessionID, sessionIDs)
}

// matchesSession reports whether a declared session id is one of the expected
// ones. An empty declaration (an older plugin) cannot be attributed, and an
// empty expectation means the connector has no id to compare, so both are
// accepted rather than risking a session's own turn being ignored.
func matchesSession(declared string, sessionIDs []string) bool {
	if declared == "" {
		return true
	}
	for _, id := range sessionIDs {
		if id == "" || id == declared {
			return true
		}
	}
	return false
}

// Running reports whether text is still arriving.
func (o *Outbox) Running() bool { return o.Status == "streaming" }

// Finished reports whether the turn completed.
func (o *Outbox) Finished() bool { return o.Status == "done" }

// ReadOutbox returns the current snapshot, or nil when the plugin has not
// written one yet. A read that catches the file mid-rewrite returns an error;
// the caller polls again.
func (b *Bridge) ReadOutbox() (*Outbox, error) {
	raw, err := os.ReadFile(b.OutboxPath())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("parse outbox: %w", err)
	}
	out := &Outbox{State: map[string]any{}}
	if v, ok := data["sessionId"].(string); ok {
		out.SessionID = v
	}
	if v, ok := data["turnId"].(string); ok {
		out.TurnID = v
	}
	if v, ok := data["status"].(string); ok {
		out.Status = v
	}
	if v, ok := data["chunks"].(float64); ok {
		out.Chunks = int(v)
	}
	if v, ok := data["text"].(string); ok {
		out.Text = v
	}
	if v, ok := data["ts"].(float64); ok {
		out.TS = int64(v)
	}
	for _, key := range outboxStateKeys {
		if v, ok := data[key]; ok && v != nil {
			out.State[key] = v
		}
	}
	return out, nil
}

// ToolEntry is one tool call the plugin observed. The field names match the
// plugin's writeTools output.
type ToolEntry struct {
	ID      string          `json:"id"`
	Tool    string          `json:"tool"`
	Input   json.RawMessage `json:"input"`
	Output  json.RawMessage `json:"output,omitempty"`
	Done    bool            `json:"done"`
	IsError bool            `json:"isError,omitempty"`
}

// Tools returns the tool calls recorded for the running turn, oldest first.
// A snapshot stamped with a different runtime session is skipped.
func (b *Bridge) Tools(sessionIDs ...string) []ToolEntry {
	raw, err := os.ReadFile(b.ToolsPath())
	if err != nil {
		return nil
	}
	var payload struct {
		SessionID string      `json:"sessionId"`
		Entries   []ToolEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	if !matchesSession(payload.SessionID, sessionIDs) {
		return nil
	}
	return payload.Entries
}

// RequestAbort asks the plugin to cancel the turn it is running. The stamp is
// followed by the runtime session id on a second line, so a plugin shared by
// several sessions can ignore a request addressed to another one; an older
// plugin parses only the first line.
func (b *Bridge) RequestAbort(sessionID string) error {
	body := strconv.FormatInt(time.Now().UnixMilli(), 10)
	if sessionID != "" {
		body += "\n" + sessionID
	}
	return writeAtomic(b.AbortPath(), []byte(body))
}

// HeartbeatAge returns how long ago the plugin reported it was alive, or
// (0, false) when it never has.
func (b *Bridge) HeartbeatAge() (time.Duration, bool) {
	raw, err := os.ReadFile(b.HeartbeatPath())
	if err != nil {
		return 0, false
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	age := time.Since(time.UnixMilli(ms))
	if age < 0 {
		age = 0
	}
	return age, true
}

// Available reports whether a live plugin is answering right now.
func (b *Bridge) Available() bool {
	age, ok := b.HeartbeatAge()
	return ok && age <= DefaultHeartbeatTTL
}

// readJSONFile decodes a small JSON file, returning ok=false when it is absent
// or unreadable.
func readJSONFile(path string, dst any) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(raw, dst) == nil
}

// writeAtomic writes through a temporary file in the same directory and then
// renames, because the plugin lists the directory continuously and would read
// a half-written file as invalid JSON.
func writeAtomic(path string, body []byte) error {
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

func mustJSON(value any) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return body
}

// SubmitCommand queues a slash command. The plugin reads the name separately
// from the argument tail, so it cannot be sent as ordinary prompt text.
func (b *Bridge) SubmitCommand(sessionID, messageID, command, args string) (*Submission, error) {
	name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "/"))
	if name == "" {
		return nil, errors.New("empty command")
	}
	body := map[string]any{
		"id":        messageID,
		"action":    "command",
		"command":   name,
		"queuedAt":  time.Now().UnixMilli(),
		"sessionId": sessionID,
	}
	if args != "" {
		body["args"] = args
	}
	previous := ""
	if box, err := b.ReadOutbox(); err == nil && box != nil {
		previous = box.TurnID
	}
	if err := b.writeQueueRaw(body); err != nil {
		return nil, err
	}
	return &Submission{QueuedAt: time.Now().UnixMilli(), PreviousTurnID: previous}, nil
}

// AgentEntry is one subagent or workflow running beside the main turn.
type AgentEntry struct {
	AgentID   string `json:"agentId"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
	UpdatedAt int64  `json:"updatedAt"`
}

// Agents returns the subagents the plugin currently knows about. A snapshot
// stamped with a different runtime session is skipped.
func (b *Bridge) Agents(sessionIDs ...string) []AgentEntry {
	raw, err := os.ReadFile(filepath.Join(b.Dir, "agents.json"))
	if err != nil {
		return nil
	}
	var payload struct {
		SessionID string       `json:"sessionId"`
		Entries   []AgentEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil
	}
	if !matchesSession(payload.SessionID, sessionIDs) {
		return nil
	}
	return payload.Entries
}
