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
	"crypto/sha256"
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

// DefaultLiveTTL is how stale a per-session presence stamp may be.
//
// The plugin refreshes the stamp every two seconds while it can submit a
// prompt, so a process that dies without cleaning up disappears from presence
// within this window. The window is wider than the stamp cadence because the
// Claude Code process occasionally holds its event loop through a long tool
// call; a stamp that is a few seconds late is a busy session, not a dead one,
// and dropping the dot then made a working conversation look offline.
const DefaultLiveTTL = 12 * time.Second

const requestOutboxCapabilityTTL = DefaultLiveTTL

const (
	acceptedRequestRetention = 90 * 24 * time.Hour
	acceptedRequestCapacity  = 50000
)

var ErrRequestConflict = errors.New("request id was already used for different content")

// outboxStateKeys are forwarded to the server as session state; "branch" is the
// git branch, not a duplicate of the working directory.
var outboxStateKeys = []string{"model", "effort", "cwd", "branch", "usage", "context", "costUsd"}

// Bridge is a handle on one bridge directory.
type Bridge struct {
	Dir string

	mu             sync.Mutex
	token          string
	lastSeq        int
	lastPruneSeq   int
	acceptedPrunes int
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
func (b *Bridge) QueueDir() string { return filepath.Join(b.Dir, "queue") }
func sessionFileID(sessionIDs []string) string {
	for i := len(sessionIDs) - 1; i >= 0; i-- {
		if sessionIDs[i] != "" {
			return sanitizeCursorID(sessionIDs[i])
		}
	}
	return ""
}
func (b *Bridge) OutboxPath(sessionIDs ...string) string {
	if id := sessionFileID(sessionIDs); id != "" {
		return filepath.Join(b.Dir, "outbox."+id+".json")
	}
	return filepath.Join(b.Dir, "outbox.json")
}
func (b *Bridge) RequestOutboxPath(sessionID, requestID string) string {
	return filepath.Join(b.Dir, "outbox."+sanitizeCursorID(sessionID)+"."+sanitizeCursorID(requestID)+".json")
}
func (b *Bridge) RequestToolsPath(sessionID, requestID string) string {
	return filepath.Join(b.Dir, "tools."+sanitizeCursorID(sessionID)+"."+sanitizeCursorID(requestID)+".json")
}
func (b *Bridge) RequestAgentsPath(sessionID, requestID string) string {
	return filepath.Join(b.Dir, "agents."+sanitizeCursorID(sessionID)+"."+sanitizeCursorID(requestID)+".json")
}
func (b *Bridge) RequestReceiptPath(sessionID, requestID string) string {
	return filepath.Join(b.Dir, "receipt."+sanitizeCursorID(sessionID)+"."+sanitizeCursorID(requestID)+".json")
}

func (b *Bridge) acceptedRequestPath(sessionID, requestID string) string {
	key := sha256.Sum256([]byte(sessionID + "\x00" + requestID))
	return filepath.Join(b.Dir, "accepted-requests", hex.EncodeToString(key[:])+".json")
}

// AcceptedRequest is the durable idempotency record for a server submit. It
// outlives the plugin's request-scoped stream artifacts.
type AcceptedRequest struct {
	SessionID         string `json:"sessionId"`
	ExternalSessionID string `json:"externalSessionId,omitempty"`
	RequestID         string `json:"requestId"`
	PayloadHash       string `json:"payloadHash"`
	Action            string `json:"action"`
	QueuedAt          int64  `json:"queuedAt"`
	QueueSeq          int    `json:"queueSeq,omitempty"`
	PreviousTurnID    string `json:"previousTurnId,omitempty"`
	AcceptedAt        int64  `json:"acceptedAt"`
	TerminalAcked     bool   `json:"terminalAcked,omitempty"`
}

// AcceptedRequest returns a persisted request identity, if one exists.
func (b *Bridge) AcceptedRequest(sessionID, requestID string) (*AcceptedRequest, error) {
	if sessionID == "" || requestID == "" {
		return nil, nil
	}
	record, err := readAcceptedRequest(b.acceptedRequestPath(sessionID, requestID))
	if err != nil || record == nil {
		return record, err
	}
	if record.SessionID != sessionID || record.RequestID != requestID {
		return nil, errors.New("accepted request identity does not match its key")
	}
	return record, nil
}

// MarkTurnEndAcked records that the server committed the final result.
func (b *Bridge) MarkTurnEndAcked(sessionID, requestID string) error {
	if sessionID == "" || requestID == "" {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	path := b.acceptedRequestPath(sessionID, requestID)
	record, err := readAcceptedRequest(path)
	if err != nil || record == nil {
		return err
	}
	if record.SessionID != sessionID || record.RequestID != requestID {
		return errors.New("accepted request identity does not match its key")
	}
	if record.TerminalAcked {
		return nil
	}
	record.TerminalAcked = true
	return writeAtomic(path, mustJSON(record))
}

// PendingAcceptedRequests returns prompts not yet followed by a committed
// final result so a restarted Connector can resume their watchers.
func (b *Bridge) PendingAcceptedRequests() ([]AcceptedRequest, error) {
	dir := filepath.Join(b.Dir, "accepted-requests")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []AcceptedRequest
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var record AcceptedRequest
		if !readJSONFile(filepath.Join(dir, entry.Name()), &record) {
			continue
		}
		if (record.Action == "prompt" || record.Action == "command") && !record.TerminalAcked && record.SessionID != "" && record.RequestID != "" {
			out = append(out, record)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AcceptedAt != out[j].AcceptedAt {
			return out[i].AcceptedAt < out[j].AcceptedAt
		}
		return out[i].RequestID < out[j].RequestID
	})
	return out, nil
}

// RemoveRequestArtifacts releases the snapshots used to follow one submitted
// prompt after its watcher has finished consuming them.
func (b *Bridge) RemoveRequestArtifacts(sessionID, requestID string) error {
	if sessionID == "" || requestID == "" {
		return nil
	}
	paths := []string{
		b.RequestOutboxPath(sessionID, requestID),
		b.RequestToolsPath(sessionID, requestID),
		b.RequestAgentsPath(sessionID, requestID),
		b.RequestReceiptPath(sessionID, requestID),
	}
	var firstErr error
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (b *Bridge) clearRequestReceipt(sessionID, requestID string) error {
	if sessionID == "" || requestID == "" {
		return nil
	}
	err := os.Remove(b.RequestReceiptPath(sessionID, requestID))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (b *Bridge) RequestOutboxSupported(sessionID string) bool {
	if sessionID == "" {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(b.Dir, "capabilities."+sanitizeCursorID(sessionID)+".txt"))
	if err != nil {
		return false
	}
	parts := strings.SplitN(strings.TrimSpace(string(raw)), "\n", 2)
	if len(parts) != 2 || parts[0] != "request-outbox-v1" {
		return false
	}
	stamp, err := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return false
	}
	age := time.Since(time.UnixMilli(stamp))
	return age >= 0 && age <= requestOutboxCapabilityTTL
}

type RequestReceipt struct {
	SessionID string `json:"sessionId"`
	RequestID string `json:"requestId"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

func (b *Bridge) ReadRequestReceipt(sessionID, requestID string) (*RequestReceipt, error) {
	if sessionID == "" || requestID == "" {
		return nil, nil
	}
	var receipt RequestReceipt
	if !readJSONFile(b.RequestReceiptPath(sessionID, requestID), &receipt) {
		return nil, nil
	}
	if receipt.SessionID != sessionID || receipt.RequestID != requestID {
		return nil, nil
	}
	return &receipt, nil
}
func (b *Bridge) HeartbeatPath() string { return filepath.Join(b.Dir, "heartbeat.txt") }
func (b *Bridge) AbortPath() string     { return filepath.Join(b.Dir, "abort.txt") }
func (b *Bridge) ToolsPath(sessionIDs ...string) string {
	if id := sessionFileID(sessionIDs); id != "" {
		return filepath.Join(b.Dir, "tools."+id+".json")
	}
	return filepath.Join(b.Dir, "tools.json")
}
func (b *Bridge) AgentsPath(sessionIDs ...string) string {
	if id := sessionFileID(sessionIDs); id != "" {
		return filepath.Join(b.Dir, "agents."+id+".json")
	}
	return filepath.Join(b.Dir, "agents.json")
}
func (b *Bridge) TokenPath() string    { return filepath.Join(b.Dir, "auth.json") }
func (b *Bridge) ApprovalPath() string { return filepath.Join(b.Dir, "approval.json") }
func (b *Bridge) LiveDir() string      { return filepath.Join(b.Dir, "live") }

// RebindRequest is one /clear transition the plugin recorded: the terminal
// kept running, but the runtime session id (and transcript) changed.
type RebindRequest struct {
	Path string
	From string
	To   string
	At   int64
}

// RebindRequests lists rebind records the plugin left for this process. A
// malformed record is removed rather than retried forever.
func (b *Bridge) RebindRequests() []RebindRequest {
	entries, err := os.ReadDir(b.Dir)
	if err != nil {
		return nil
	}
	out := make([]RebindRequest, 0, 4)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "rebind.") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(b.Dir, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var body struct {
			From string `json:"from"`
			To   string `json:"to"`
			At   int64  `json:"at"`
		}
		if json.Unmarshal(raw, &body) != nil || body.From == "" || body.To == "" || body.From == body.To {
			_ = os.Remove(path)
			continue
		}
		out = append(out, RebindRequest{Path: path, From: body.From, To: body.To, At: body.At})
	}
	return out
}

// RemoveRebind deletes a processed record.
func (b *Bridge) RemoveRebind(path string) error {
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// RemoveLiveStamp withdraws one conversation's presence.
//
// Used when the connector itself ends the process: the plugin never gets to
// delete its own stamp, and until the TTL runs out the conversation would still
// look ready to take a message.
func (b *Bridge) RemoveLiveStamp(sessionID string) error {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) || sessionID == "." || sessionID == ".." {
		return nil
	}
	err := os.Remove(filepath.Join(b.LiveDir(), sessionID+".txt"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Live reports whether this one conversation's plugin refreshed its presence
// stamp inside ttl.
//
// Available() reads the shared heartbeat, which every Claude process on the
// machine rewrites. Using it to decide whether one session's plugin is still
// alive means a dead conversation stays "alive" as long as any other plugin
// runs, and its turn watcher never gives up.
func (b *Bridge) Live(sessionID string, ttl time.Duration) bool {
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) || sessionID == "." || sessionID == ".." {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(b.LiveDir(), sessionID+".txt"))
	if err != nil {
		return false
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return false
	}
	return ms >= time.Now().Add(-ttl).UnixMilli()
}

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
	// Reuse the token already on disk. Minting a fresh one on every restart
	// invalidated every message still waiting in the queue: the plugin checks
	// each entry against auth.json, refused the survivors and moved its cursor
	// past them, so a restart silently ate whatever was queued.
	if existing := b.readTokenFile(); existing != "" {
		b.token = existing
		return existing, nil
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

// readTokenFile returns the token already in auth.json, or "" when the file is
// missing or unusable.
func (b *Bridge) readTokenFile() string {
	raw, err := os.ReadFile(b.TokenPath())
	if err != nil {
		return ""
	}
	var payload struct {
		Token string `json:"token"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return ""
	}
	token := strings.TrimSpace(payload.Token)
	if len(token) < 16 {
		return ""
	}
	return token
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
	RequestHash       string `json:"requestHash,omitempty"`
	PreviousTurnID    string `json:"previousTurnId,omitempty"`
}

// Submission describes what the caller needs to follow the turn it queued.
type Submission struct {
	QueuedAt int64
	// PreviousTurnID is the turn the outbox already showed. The plugin rewrites
	// the same file, so a snapshot carrying this id predates the new message.
	PreviousTurnID string
	Duplicate      bool
	TerminalAcked  bool
}

// Submit queues a prompt and returns the handle used to follow it.
func (b *Bridge) Submit(sessionID, externalSessionID, messageID, text, action string) (*Submission, error) {
	return b.SubmitWithRequestHash(sessionID, externalSessionID, messageID, text, action, "")
}

// SubmitWithRequestHash queues a prompt and persists its request identity so a
// server retry cannot make the plugin execute the same prompt twice.
func (b *Bridge) SubmitWithRequestHash(sessionID, externalSessionID, messageID, text, action, requestHash string) (*Submission, error) {
	if strings.TrimSpace(text) == "" {
		return nil, errors.New("empty message")
	}
	if action == "" {
		action = "prompt"
	}
	previous, err := b.previousTurnID(externalSessionID)
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
		RequestHash:       requestHash,
		PreviousTurnID:    previous,
	}
	if msg.ID == "" {
		msg.ID = "bridge-" + strconv.FormatInt(msg.QueuedAt, 10)
	}
	if messageID == "" {
		if err := b.clearRequestReceipt(externalSessionID, msg.ID); err != nil {
			return nil, fmt.Errorf("clear previous request receipt: %w", err)
		}
		if err := b.writeQueue(msg); err != nil {
			return nil, err
		}
		return &Submission{QueuedAt: msg.QueuedAt, PreviousTurnID: previous}, nil
	}
	return b.persistAcceptedRequest(msg, requestHash)
}

// SubmitCommandWithRequestHash queues a command with the same durable
// idempotency guarantee as an ordinary prompt.
func (b *Bridge) SubmitCommandWithRequestHash(sessionID, externalSessionID, messageID, command, args, requestHash string) (*Submission, error) {
	name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(command), "/"))
	if name == "" {
		return nil, errors.New("empty command")
	}
	previous := ""
	if box, err := b.ReadOutbox(externalSessionID); err == nil && box != nil {
		previous = box.TurnID
	}
	msg := QueueMessage{
		ID: messageID, Action: "command", Command: name, Args: args,
		QueuedAt: time.Now().UnixMilli(), SessionID: sessionID,
		ExternalSessionID: externalSessionID, RequestHash: requestHash,
		PreviousTurnID: previous,
	}
	if msg.ID == "" {
		if err := b.writeQueue(msg); err != nil {
			return nil, err
		}
		return &Submission{QueuedAt: msg.QueuedAt, PreviousTurnID: previous}, nil
	}
	return b.persistAcceptedRequest(msg, requestHash)
}

func (b *Bridge) persistAcceptedRequest(msg QueueMessage, requestHash string) (*Submission, error) {
	if requestHash == "" {
		body, err := json.Marshal(struct {
			SessionID         string `json:"sessionId"`
			ExternalSessionID string `json:"externalSessionId"`
			Action            string `json:"action"`
			Text              string `json:"text"`
			Command           string `json:"command"`
			Args              string `json:"args"`
		}{msg.SessionID, msg.ExternalSessionID, msg.Action, msg.Text, msg.Command, msg.Args})
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(body)
		requestHash = hex.EncodeToString(hash[:])
		msg.RequestHash = requestHash
	}
	path := b.acceptedRequestPath(msg.SessionID, msg.ID)
	b.mu.Lock()
	defer b.mu.Unlock()
	existing, err := readAcceptedRequest(path)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.SessionID != msg.SessionID || existing.RequestID != msg.ID || existing.PayloadHash != requestHash {
			return nil, ErrRequestConflict
		}
		if !existing.TerminalAcked {
			queued, err := b.findQueuedRequestLocked(msg.ID)
			if err != nil {
				return nil, err
			}
			if queued == nil && existing.QueueSeq > 0 {
				cursorID := sanitizeCursorID(msg.ExternalSessionID)
				if cursorID == "" || readSessionCursor(b.Dir, cursorID) < existing.QueueSeq {
					msg.QueuedAt = existing.QueuedAt
					msg.PreviousTurnID = existing.PreviousTurnID
					if err := b.writeQueueAtSeqLocked(msg, existing.QueueSeq); err != nil {
						return nil, err
					}
				}
			}
		}
		return &Submission{
			QueuedAt: existing.QueuedAt, PreviousTurnID: existing.PreviousTurnID,
			Duplicate: true, TerminalAcked: existing.TerminalAcked,
		}, nil
	}
	queued, err := b.findQueuedRequestLocked(msg.ID)
	if err != nil {
		return nil, err
	}
	if queued != nil {
		if queued.SessionID != msg.SessionID || queued.ExternalSessionID != msg.ExternalSessionID || queued.Action != msg.Action || queued.Text != msg.Text || queued.Command != msg.Command || queued.Args != msg.Args || (queued.RequestHash != "" && queued.RequestHash != requestHash) {
			return nil, ErrRequestConflict
		}
		msg.QueuedAt = queued.QueuedAt
		if queued.PreviousTurnID != "" {
			msg.PreviousTurnID = queued.PreviousTurnID
		}
	}
	queueSeq := 0
	if queued == nil {
		if err := b.clearRequestReceipt(msg.ExternalSessionID, msg.ID); err != nil {
			return nil, fmt.Errorf("clear previous request receipt: %w", err)
		}
		queueSeq = b.nextQueueSequenceLocked(b.QueueDir())
	}
	record := AcceptedRequest{
		SessionID: msg.SessionID, ExternalSessionID: msg.ExternalSessionID,
		RequestID: msg.ID, PayloadHash: requestHash, Action: msg.Action,
		QueuedAt: msg.QueuedAt, QueueSeq: queueSeq, PreviousTurnID: msg.PreviousTurnID,
		AcceptedAt: time.Now().UnixMilli(),
	}
	if err := writeAtomic(path, mustJSON(record)); err != nil {
		return nil, fmt.Errorf("persist accepted request: %w", err)
	}
	if queued == nil {
		if err := b.writeQueueAtSeqLocked(msg, queueSeq); err != nil {
			return nil, err
		}
	}
	b.pruneAcceptedRequestsLocked()
	return &Submission{QueuedAt: msg.QueuedAt, PreviousTurnID: msg.PreviousTurnID}, nil
}

func readAcceptedRequest(path string) (*AcceptedRequest, error) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record AcceptedRequest
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, fmt.Errorf("parse accepted request: %w", err)
	}
	return &record, nil
}

func (b *Bridge) findQueuedRequestLocked(requestID string) (*QueueMessage, error) {
	entries, err := os.ReadDir(b.QueueDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var queued QueueMessage
		if !readJSONFile(filepath.Join(b.QueueDir(), entry.Name()), &queued) || queued.ID != requestID {
			continue
		}
		return &queued, nil
	}
	return nil, nil
}

func (b *Bridge) pruneAcceptedRequestsLocked() {
	b.acceptedPrunes++
	if b.acceptedPrunes%16 != 1 {
		return
	}
	dir := filepath.Join(b.Dir, "accepted-requests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type acceptedFile struct {
		path          string
		at            int64
		terminalAcked bool
	}
	files := make([]acceptedFile, 0, len(entries))
	cutoff := time.Now().Add(-acceptedRequestRetention).UnixMilli()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var record AcceptedRequest
		if !readJSONFile(path, &record) {
			continue
		}
		if record.TerminalAcked && record.AcceptedAt > 0 && record.AcceptedAt < cutoff {
			_ = os.Remove(path)
			continue
		}
		files = append(files, acceptedFile{
			path: path, at: record.AcceptedAt, terminalAcked: record.TerminalAcked,
		})
	}
	if len(files) <= acceptedRequestCapacity {
		return
	}
	// A request without a committed terminal ACK is the only recovery record
	// for a prompt that may still be running. Never evict it to meet the size
	// target; trim the oldest completed records instead.
	completed := make([]acceptedFile, 0, len(files))
	for _, file := range files {
		if file.terminalAcked {
			completed = append(completed, file)
		}
	}
	sort.Slice(completed, func(i, j int) bool { return completed[i].at < completed[j].at })
	removeCount := len(files) - acceptedRequestCapacity
	if removeCount > len(completed) {
		removeCount = len(completed)
	}
	for _, file := range completed[:removeCount] {
		_ = os.Remove(file.path)
	}
}

// previousTurnID reads the turn the outbox showed before this message was
// queued. The plugin rewrites the file while text streams in, so a read can
// catch it half-written; retrying beats treating a torn file as "no previous
// turn", which would let the watcher adopt a stale turn as the answer.
func (b *Bridge) previousTurnID(sessionID string) (string, error) {
	var last error
	for attempt := 0; attempt < 10; attempt++ {
		box, err := b.ReadOutbox(sessionID)
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
	return b.writeQueueRawLocked(payload)
}

func (b *Bridge) writeQueueRawLocked(payload map[string]any) error {
	dir := b.QueueDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	token, err := b.tokenLocked()
	if err != nil {
		return err
	}
	payload["token"] = token

	seq := b.nextQueueSequenceLocked(dir)
	return b.writeQueuePayloadAtSeqLocked(payload, seq)
}

func (b *Bridge) nextQueueSequenceLocked(dir string) int {
	seq := b.lastSeq
	if next := b.nextSeq(dir); next > seq {
		seq = next
	}
	seq++
	if b.lastPruneSeq == 0 || seq-b.lastPruneSeq >= 64 {
		b.pruneConsumedQueue(dir)
		b.lastPruneSeq = seq
		if next := b.nextSeq(dir); next >= seq {
			seq = next + 1
		}
	}
	b.lastSeq = seq
	return seq
}

func (b *Bridge) writeQueueAtSeqLocked(msg QueueMessage, seq int) error {
	dir := b.QueueDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	token, err := b.tokenLocked()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"id": msg.ID, "action": msg.Action, "queuedAt": msg.QueuedAt,
		"sessionId": msg.SessionID, "externalSessionId": msg.ExternalSessionID,
		"requestHash": msg.RequestHash, "previousTurnId": msg.PreviousTurnID,
		"token": token,
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
	return b.writeQueuePayloadAtSeqLocked(payload, seq)
}

func (b *Bridge) writeQueuePayloadAtSeqLocked(payload map[string]any, seq int) error {
	dir := b.QueueDir()
	final := filepath.Join(dir, strconv.Itoa(seq)+".json")
	if _, err := os.Stat(final); err == nil {
		return fmt.Errorf("queue sequence %d is already occupied", seq)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if seq > b.lastSeq {
		b.lastSeq = seq
	}
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
	if msg.RequestHash != "" {
		payload["requestHash"] = msg.RequestHash
	}
	if msg.PreviousTurnID != "" {
		payload["previousTurnId"] = msg.PreviousTurnID
	}
	return b.writeQueueRaw(payload)
}

func (b *Bridge) writeQueueLocked(msg QueueMessage) error {
	payload := map[string]any{
		"id": msg.ID, "action": msg.Action, "queuedAt": msg.QueuedAt,
		"sessionId": msg.SessionID, "externalSessionId": msg.ExternalSessionID,
		"requestHash": msg.RequestHash, "previousTurnId": msg.PreviousTurnID,
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
	return b.writeQueueRawLocked(payload)
}

// nextSeq is the highest sequence already used, counting both the files still
// in the queue and the cursor the plugin has consumed up to.
//
// The cursor matters: the plugin only reads files numbered above it, so if the
// queue is emptied and numbering restarted at 1, every new message would sit
// there invisible forever.
func (b *Bridge) nextSeq(dir string) int {
	best := highestSeq(dir)
	if cursor := highestCursor(b.Dir); cursor > best {
		best = cursor
	}
	return best
}

// pruneConsumedQueue drops queue files after the addressed plugin session has
// durably advanced its cursor past them. This bounds directory scans while
// preserving messages that their target session has not picked up yet.
//
// To prevent a fast session from deleting messages a slow session has not yet
// consumed, only prune up to the minimum cursor across all active sessions that
// have ever polled. The watermark file persists this safe bound.
func (b *Bridge) pruneConsumedQueue(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	// Load all session cursors and compute the global minimum safe watermark.
	cursors := b.loadAllSessionCursors()
	minCursor := b.computeSafeWatermark(cursors)

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		seq, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil || seq < 1 {
			continue
		}

		// Only prune messages below the global safe watermark.
		if seq <= minCursor {
			path := filepath.Join(dir, entry.Name())
			_ = os.Remove(path)
		}
	}

	// Persist the new watermark after successful cleanup.
	b.persistWatermark(minCursor)
}

// loadAllSessionCursors scans for all session cursor files and returns a map.
func (b *Bridge) loadAllSessionCursors() map[string]int {
	cursors := make(map[string]int)
	entries, err := os.ReadDir(b.Dir)
	if err != nil {
		return cursors
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "cursor.") || !strings.HasSuffix(name, ".txt") {
			continue
		}
		// Skip backup files.
		if strings.HasSuffix(name, ".txt.bak") {
			continue
		}
		sessionID := strings.TrimSuffix(strings.TrimPrefix(name, "cursor."), ".txt")
		if sessionID == "" {
			continue
		}
		cursor := readSessionCursor(b.Dir, sessionID)
		if cursor > 0 {
			cursors[sessionID] = cursor
		}
	}
	return cursors
}

// computeSafeWatermark returns the minimum cursor value, ensuring no active
// session's unread messages are deleted. Returns the persisted watermark if
// no sessions have cursors yet.
func (b *Bridge) computeSafeWatermark(cursors map[string]int) int {
	if len(cursors) == 0 {
		// No active sessions: use last persisted watermark to avoid replaying.
		return b.readWatermark()
	}
	minCursor := -1
	for _, cursor := range cursors {
		if minCursor == -1 || cursor < minCursor {
			minCursor = cursor
		}
	}
	// Never regress below the last persisted watermark.
	lastWatermark := b.readWatermark()
	if minCursor < lastWatermark {
		minCursor = lastWatermark
	}
	return minCursor
}

// readWatermark loads the last safe prune boundary from disk.
func (b *Bridge) readWatermark() int {
	raw, err := os.ReadFile(filepath.Join(b.Dir, "prune-watermark.txt"))
	if err != nil {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// persistWatermark writes the new safe prune boundary to disk.
func (b *Bridge) persistWatermark(seq int) {
	if seq <= 0 {
		return
	}
	path := filepath.Join(b.Dir, "prune-watermark.txt")
	_ = writeAtomic(path, []byte(strconv.Itoa(seq)))
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
	value, _ := readCursorFile(filepath.Join(dir, "cursor.txt"))
	return value
}

func readCursorFile(path string) (int, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, false
	}
	return n, true
}

func readSessionCursor(dir, sessionID string) int {
	path := filepath.Join(dir, "cursor."+sessionID+".txt")
	if value, ok := readCursorFile(path); ok {
		return value
	}
	backup, _ := readCursorFile(path + ".bak")
	return backup
}

func highestCursor(dir string) int {
	best := readCursor(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return best
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "cursor.") || !strings.HasSuffix(name, ".txt") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil && n > best {
			best = n
		}
	}
	return best
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
	RequestID string
	Status    string
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
		// An unowned snapshot cannot be attributed to anyone. Treating it as
		// "belongs to whoever asked" let one conversation adopt another's
		// shared outbox/tools/agents when an older plugin wrote them.
		return len(sessionIDs) == 0
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

// Aborted reports whether the conversation was ended (or /cleared) while the
// turn was still running. The plugin writes this once for the conversation it
// is leaving, because no further snapshot will ever arrive.
func (o *Outbox) Aborted() bool { return o.Status == "aborted" }

// ReadOutbox returns the current snapshot, or nil when the plugin has not
// written one yet. A read that catches the file mid-rewrite returns an error;
// the caller polls again.
func (b *Bridge) ReadOutbox(sessionIDs ...string) (*Outbox, error) {
	path := b.OutboxPath(sessionIDs...)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) && len(sessionIDs) > 0 && sessionIDs[0] != "" {
		// Older installed plugins still write the shared outbox. Accept it only
		// when its session id matches the requester's session.
		raw, err = os.ReadFile(b.OutboxPath())
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return parseOutbox(raw, sessionIDs...)
}

// ReadRequestOutbox reads the durable stream for one submitted message. Each
// response remains available even when a later queued turn starts quickly.
func (b *Bridge) ReadRequestOutbox(sessionID, requestID string) (*Outbox, error) {
	if sessionID == "" || requestID == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(b.RequestOutboxPath(sessionID, requestID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out, err := parseOutbox(raw, sessionID)
	if err != nil || out == nil || out.RequestID != requestID {
		return out, err
	}
	return out, nil
}

func parseOutbox(raw []byte, sessionIDs ...string) (*Outbox, error) {
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
	if v, ok := data["requestId"].(string); ok {
		out.RequestID = v
	}
	if v, ok := data["status"].(string); ok {
		out.Status = v
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
	if len(sessionIDs) > 0 && !out.BelongsTo(sessionIDs...) {
		return nil, nil
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
	raw, err := os.ReadFile(b.ToolsPath(sessionIDs...))
	if errors.Is(err, os.ErrNotExist) && len(sessionIDs) > 0 && sessionIDs[0] != "" {
		raw, err = os.ReadFile(b.ToolsPath())
	}
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

func (b *Bridge) RequestTools(sessionID, requestID string) []ToolEntry {
	if sessionID == "" || requestID == "" {
		return nil
	}
	var payload struct {
		SessionID string      `json:"sessionId"`
		Entries   []ToolEntry `json:"entries"`
	}
	if !readJSONFile(b.RequestToolsPath(sessionID, requestID), &payload) || !matchesSession(payload.SessionID, []string{sessionID}) {
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
	if err := temp.Sync(); err != nil {
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
	return b.SubmitCommandWithRequestHash(sessionID, externalSessionID, messageID, command, args, "")
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
	raw, err := os.ReadFile(b.AgentsPath(sessionIDs...))
	if errors.Is(err, os.ErrNotExist) && len(sessionIDs) > 0 && sessionIDs[0] != "" {
		raw, err = os.ReadFile(b.AgentsPath())
	}
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

func (b *Bridge) RequestAgents(sessionID, requestID string) []AgentEntry {
	if sessionID == "" || requestID == "" {
		return nil
	}
	var payload struct {
		SessionID string       `json:"sessionId"`
		Entries   []AgentEntry `json:"entries"`
	}
	if !readJSONFile(b.RequestAgentsPath(sessionID, requestID), &payload) || !matchesSession(payload.SessionID, []string{sessionID}) {
		return nil
	}
	return payload.Entries
}
