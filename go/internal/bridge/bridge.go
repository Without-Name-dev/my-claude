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
//	live/<id>.txt    when session <id>'s plugin last confirmed it can submit,
//	                 as epoch milliseconds; stale stamps are not live
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
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultHeartbeatTTL is how stale the heartbeat may be before the bridge is
// considered absent.
const DefaultHeartbeatTTL = 30 * time.Second

// DefaultLiveTTL is how stale a per-session presence stamp may be. The plugin
// refreshes it every five seconds while it can submit a prompt, so a process
// that dies without cleaning up disappears from presence within this window.
const DefaultLiveTTL = 30 * time.Second

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
func (b *Bridge) AgentsPath() string    { return filepath.Join(b.Dir, "agents.json") }
func (b *Bridge) TokenPath() string     { return filepath.Join(b.Dir, "auth.json") }
func (b *Bridge) ApprovalPath() string  { return filepath.Join(b.Dir, "approval.json") }
func (b *Bridge) LiveDir() string       { return filepath.Join(b.Dir, "live") }

// maxLiveStamps bounds one presence scan. Stamps that stopped moving are
// deleted as they are found (see expireLiveStamp), so a normal directory holds
// one file per conversation that can accept a prompt; the cap only matters in a
// pathological state and keeps one heartbeat from reading an unbounded set.
const maxLiveStamps = 1024

// LiveSessions returns the runtime session ids whose presence stamp is no older
// than ttl, sorted so the wire frame is stable.
//
// The stamp is the file's content, not its mtime: the plugin rewrites the same
// file every few seconds, and a stamp that stopped moving means the session can
// no longer take a message. A missing, empty or unparsable file is therefore
// simply not live, which also covers a plugin that could not delete its file on
// shutdown. Those dead files are removed here, because nothing else expires a
// stamp left behind by a process that was killed.
func (b *Bridge) LiveSessions(ttl time.Duration) []string {
	entries, err := os.ReadDir(b.LiveDir())
	if err != nil {
		return nil
	}
	cutoff := time.Now().Add(-ttl).UnixMilli()
	ids := make([]string, 0, len(entries))
	scanned := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".txt") {
			continue
		}
		id := strings.TrimSuffix(name, ".txt")
		if id == "" {
			continue
		}
		if scanned >= maxLiveStamps {
			break
		}
		scanned++
		path := filepath.Join(b.LiveDir(), name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		ms, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
		if err != nil || ms < cutoff {
			expireLiveStamp(entry, path, cutoff)
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// expireLiveStamp deletes a stamp no plugin refreshed inside the window.
//
// The mtime has to be old as well as the content: a plugin writing between our
// read and this check would replace the stale stamp with a fresh one, and a
// fresh write has a fresh mtime, so it is left alone. Deleting only when both
// signals agree keeps a live conversation from losing its dot; the next scan
// would report it again anyway, but not flicker it off.
func expireLiveStamp(entry os.DirEntry, path string, cutoff int64) {
	info, err := entry.Info()
	if err != nil || info.ModTime().UnixMilli() >= cutoff {
		return
	}
	_ = os.Remove(path)
}

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

// SeedSessionCursor primes a session's reader with the current queue head.
//
// The plugin starts a conversation that has never polled from the global
// cursor: the highest sequence any session has consumed. That is wrong while a
// terminal is starting, because the first message for the new conversation is
// queued before its plugin boots. If another live session consumes a later
// message in the meantime, the global cursor moves past the new message and the
// plugin starts beyond it, so the message is never submitted. Writing this
// conversation's own cursor before its message is queued pins the reader to the
// head and closes that window.
//
// Files are created only when absent with O_EXCL, so a running plugin's own
// cursor is never lowered (lowering it would replay old submissions).
func (b *Bridge) SeedSessionCursor(sessionID string) error {
	id := sanitizeCursorID(sessionID)
	if id == "" {
		return nil
	}
	base := filepath.Join(b.Dir, "cursor."+id+".txt")
	head := strconv.Itoa(highestSeq(b.QueueDir()))
	for _, path := range []string{base, base + ".bak"} {
		if err := writeExclusive(path, head); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return nil
}

// sanitizeCursorID mirrors the plugin's cursor filename: any character outside
// the runtime-session-id alphabet becomes an underscore.
func sanitizeCursorID(id string) string {
	var out strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-', r == '_', r == '.':
			out.WriteRune(r)
		default:
			out.WriteRune('_')
		}
	}
	return out.String()
}

// writeExclusive creates path with body, reporting os.ErrExist when another
// writer got there first.
func writeExclusive(path, body string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(body); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
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
// from the argument tail, so it cannot be sent as ordinary prompt text. The
// runtime session id is carried like a prompt's, so a plugin shared by several
// sessions only runs the command in the conversation it was addressed to.
func (b *Bridge) SubmitCommand(sessionID, externalSessionID, messageID, command, args string) (*Submission, error) {
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
	if externalSessionID != "" {
		body["externalSessionId"] = externalSessionID
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

// CancelQueued removes a message the plugin has not picked up yet.
//
// Deleting the queue file is the only cancellation the plugin understands: it
// reads the directory in order, so a file that is gone is a message that never
// runs. One the plugin has already read still runs, which is why this is only
// used after the pickup deadline has passed.
func (b *Bridge) CancelQueued(messageID string) error {
	if messageID == "" {
		return nil
	}
	dir := b.QueueDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var msg struct {
			ID string `json:"id"`
		}
		if !readJSONFile(path, &msg) || msg.ID != messageID {
			continue
		}
		return os.Remove(path)
	}
	return nil
}

// AgentEntry is one subagent or workflow running beside the main turn.
type AgentEntry struct {
	AgentID   string `json:"agentId"`
	Label     string `json:"label"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"`
	UpdatedAt int64  `json:"updatedAt"`
	// Live activity the plugin captures from the agent's loop, absent when the
	// runtime does not expose it.
	Tool   string `json:"tool,omitempty"`
	Detail string `json:"detail,omitempty"`
	Text   string `json:"text,omitempty"`
}

// Agents returns the subagents the plugin currently knows about. A snapshot
// stamped with a different runtime session is skipped.
func (b *Bridge) Agents(sessionIDs ...string) []AgentEntry {
	raw, err := os.ReadFile(b.AgentsPath())
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
