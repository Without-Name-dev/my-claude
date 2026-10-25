package connector

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/bridge"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// sessionOpenThrottle bounds how often one conversation may be spawned. A
// second click, a retrying client, or the server opening the session just
// before a message arrives must not litter the desktop with terminals. The
// window is long enough for Claude Code to start and stamp its presence.
const sessionOpenThrottle = 60 * time.Second

// transcriptQuietWindow is how recently a transcript must have been written for
// the connector to treat a conversation as already running. It catches plugin
// builds that predate the live stamps, which would otherwise look dead while
// they are actively serving the conversation.
//
// It is deliberately short: a conversation the person just closed from the
// phone, or whose terminal exited, must be reopenable on the next message
// instead of failing that request for minutes.
const transcriptQuietWindow = 30 * time.Second

// sessionOpenState is the per-connector spawn throttle.
type sessionOpenState struct {
	mu sync.Mutex
	// last records the start of the most recent spawn attempt per runtime
	// session id.
	last map[string]time.Time
}

// allow reserves a spawn slot for id. It returns false and the remaining wait
// when the id was spawned too recently.
func (s *sessionOpenState) allow(id string, now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, at := range s.last {
		if now.Sub(at) >= sessionOpenThrottle {
			delete(s.last, key)
		}
	}
	if at, ok := s.last[id]; ok {
		if wait := sessionOpenThrottle - now.Sub(at); wait > 0 {
			return wait, false
		}
	}
	s.last[id] = now
	return 0, true
}

// sessionOpenStates keeps one throttle per Client without adding mutable state
// to the shared Client struct.
var sessionOpenStates sync.Map

func (c *Client) sessionOpenState() *sessionOpenState {
	if value, ok := sessionOpenStates.Load(c); ok {
		return value.(*sessionOpenState)
	}
	state := &sessionOpenState{last: map[string]time.Time{}}
	actual, _ := sessionOpenStates.LoadOrStore(c, state)
	return actual.(*sessionOpenState)
}

// startSessionTerminalProcess launches the terminal. It is a variable so tests
// can observe a spawn without starting one.
var startSessionTerminalProcess = startSessionTerminal

// handleSessionOpen runs one session.open frame.
//
// The frame arrives on the connector's read loop, which also carries heartbeats
// and turn frames. Starting a terminal can take seconds, so the work is moved
// to its own goroutine and the read loop never waits for it.
//
// The spawn intent is recorded before that goroutine starts: the submit for
// this conversation arrives on the same read loop immediately afterwards and
// may reach the turn worker while the terminal is still starting. Recording it
// here lets runTurn prime the new conversation's queue cursor first.
func (c *Client) handleSessionOpen(raw json.RawMessage) {
	var msg protocol.SessionOpen
	if err := json.Unmarshal(raw, &msg); err != nil {
		c.Log.Warn("bad session.open", "error", err)
		return
	}
	if id := strings.TrimSpace(msg.ExternalSessionID); id != "" && safeRuntimeSessionID(id) {
		c.noteSpawnRequested(id)
	}
	go c.openSession(msg)
}

// spawnAllowed reports whether AA_ALLOW_SPAWN permits spawning. Only the exact
// value "0" disables it; the desktop is where a conversation can actually run.
func spawnAllowed() bool {
	return strings.TrimSpace(os.Getenv("AA_ALLOW_SPAWN")) != "0"
}

// spawnRequestTTL bounds how long an unclaimed spawn request is remembered. The
// matching submit normally follows within a second; a request whose message was
// never sent is only noise after this.
const spawnRequestTTL = 10 * time.Minute

// noteSpawnRequested records that the server asked to open this conversation.
func (c *Client) noteSpawnRequested(externalSessionID string) {
	if externalSessionID == "" {
		return
	}
	now := time.Now()
	c.spawnMu.Lock()
	defer c.spawnMu.Unlock()
	for id, at := range c.spawnRequests {
		if now.Sub(at) >= spawnRequestTTL {
			delete(c.spawnRequests, id)
		}
	}
	c.spawnRequests[externalSessionID] = now
}

// consumeSpawnRequest reports whether the server asked to open this
// conversation since the previous submit, and takes the request off the books.
func (c *Client) consumeSpawnRequest(externalSessionID string) bool {
	if externalSessionID == "" {
		return false
	}
	c.spawnMu.Lock()
	defer c.spawnMu.Unlock()
	if _, ok := c.spawnRequests[externalSessionID]; !ok {
		return false
	}
	delete(c.spawnRequests, externalSessionID)
	return true
}

// openSession validates one request and starts a terminal for it.
//
// Every refusal names its reason in the log. AA_ALLOW_SPAWN=0 disables
// spawning; an absent value, or any other value, allows it, because the
// desktop is where a conversation can actually run. A platform with no
// terminal emulator the connector can drive refuses too, and advertises that
// through the sessionOpen capability so the server never promises otherwise.
func (c *Client) openSession(msg protocol.SessionOpen) {
	externalID := strings.TrimSpace(msg.ExternalSessionID)
	if externalID == "" {
		c.refuseSessionOpen(msg, "no runtime session id")
		return
	}
	if !safeRuntimeSessionID(externalID) {
		c.refuseSessionOpen(msg, "runtime session id contains characters unsafe for a command line")
		return
	}
	if !spawnAllowed() {
		c.refuseSessionOpen(msg, "spawning is disabled by AA_ALLOW_SPAWN=0")
		return
	}
	if !sessionOpenSupported() {
		c.refuseSessionOpen(msg, "no terminal emulator is available on this machine")
		return
	}
	if c.Bridge != nil && sessionIsLive(c.Bridge, externalID) {
		c.refuseSessionOpen(msg, "the session is already live")
		return
	}
	// A plugin build that predates the live stamps can still be running this
	// conversation without announcing itself: its outbox carries no session id
	// and it writes no stamp. The transcript is rewritten whenever the
	// conversation does anything, so a file touched moments ago is proof enough
	// not to start a second process on the same session.
	// A close the person asked for is authoritative: the process was ended on
	// purpose, so the transcript being recent says nothing about it running.
	if c.Discovery != nil && !c.sessionWasClosed(msg.SessionID) {
		if path, ok := c.Discovery.PathFor(externalID); ok {
			if info, err := os.Stat(path); err == nil && time.Since(info.ModTime()) <= transcriptQuietWindow {
				c.refuseSessionOpen(msg, "the transcript was updated moments ago")
				return
			}
		}
	}
	cwd := strings.TrimSpace(msg.CWD)
	if cwd == "" {
		c.refuseSessionOpen(msg, "no working directory")
		return
	}
	// A control character cannot be represented in a shell or AppleScript
	// string, and a directory containing one is never a real project. Refuse it
	// instead of letting it split the command line on a platform that parses
	// one (AppleScript's do script, or Windows Terminal's command separators).
	if strings.ContainsFunc(cwd, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		c.refuseSessionOpen(msg, "working directory contains control characters")
		return
	}
	// A relative path would be resolved against the connector's own working
	// directory, not the session's project. The server always has the absolute
	// path the desktop reported, so anything else is a bad request.
	if !filepath.IsAbs(cwd) {
		c.refuseSessionOpen(msg, "working directory is not absolute")
		return
	}
	cwd = filepath.Clean(cwd)
	info, err := os.Stat(cwd)
	if err != nil {
		c.refuseSessionOpen(msg, "working directory is not accessible: "+err.Error())
		return
	}
	if !info.IsDir() {
		c.refuseSessionOpen(msg, "working directory is not a directory")
		return
	}
	if wait, ok := c.sessionOpenState().allow(externalID, time.Now()); !ok {
		c.Log.Info("refusing to open session",
			"reason", "throttled",
			"retry_in", wait.Round(time.Second).String(),
			"session", msg.SessionID,
			"external", externalID,
			"cwd", cwd)
		return
	}

	// The server mints an id for a conversation that has never run, but it
	// cannot know whether the previous open actually created the transcript. A
	// retry that resumed a missing transcript would fail forever, so decide the
	// flag from the filesystem: resume what exists, create what does not.
	resume := msg.Resume
	if c.Discovery != nil {
		resume = transcriptExists(c.Discovery.Root, externalID)
	}

	command, err := startSessionTerminalProcess(cwd, externalID, msg.Title, resume)
	if err != nil {
		c.Log.Warn("could not open session",
			"session", msg.SessionID,
			"external", externalID,
			"cwd", cwd,
			"resume", resume,
			"error", err)
		return
	}
	c.Log.Info("opened session",
		"session", msg.SessionID,
		"external", externalID,
		"cwd", cwd,
		"resume", resume,
		"command", command)
}

// transcriptExists reports whether this runtime session already has a
// transcript under the Claude projects tree. The id is already validated to
// contain no path separator or glob metacharacter.
func transcriptExists(root, externalID string) bool {
	if root == "" || externalID == "" {
		return false
	}
	matches, err := filepath.Glob(filepath.Join(root, "*", externalID+".jsonl"))
	return err == nil && len(matches) > 0
}

// refuseSessionOpen records a request that was deliberately not acted on.
func (c *Client) refuseSessionOpen(msg protocol.SessionOpen, reason string) {
	c.Log.Info("refusing to open session",
		"reason", reason,
		"session", msg.SessionID,
		"external", msg.ExternalSessionID,
		"cwd", msg.CWD,
		"resume", msg.Resume)
}

// sessionIsLive scans the plugin's presence stamps for one runtime session.
//
// Plugin builds that predate the live/ stamps still rewrite the outbox every
// few seconds with the session they own, so a fresh, matching snapshot is also
// evidence that the conversation is running. A stale snapshot proves nothing:
// the plugin may have died before it could stop writing.
func sessionIsLive(b *bridge.Bridge, externalID string) bool {
	for _, id := range b.LiveSessions(bridge.DefaultLiveTTL) {
		if id == externalID {
			return true
		}
	}
	if box, err := b.ReadOutbox(); err == nil && box != nil && box.SessionID == externalID && box.TS > 0 {
		if time.Since(time.UnixMilli(box.TS)) <= bridge.DefaultLiveTTL {
			return true
		}
	}
	return false
}

// safeRuntimeSessionID reports whether an id can be placed on a command line
// without shell interpretation. Claude Code uses UUIDs; the broader alphabet
// keeps older/test ids working while rejecting spaces and cmd metacharacters.
//
// The first character must be alphanumeric. A leading dash would otherwise be
// read by claude as an option rather than the value of --resume/--session-id,
// which is argument injection even though no shell is involved.
func safeRuntimeSessionID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	first := id[0]
	if !isASCIIAlphaNumeric(first) {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func isASCIIAlphaNumeric(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// formatCommand renders an argv as one log line, quoting only the arguments
// that would otherwise read as several words.
func formatCommand(name string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, name)
	for _, arg := range args {
		if arg == "" || strings.ContainsAny(arg, " \t\"") {
			parts = append(parts, strconv.Quote(arg))
			continue
		}
		parts = append(parts, arg)
	}
	return strings.Join(parts, " ")
}
