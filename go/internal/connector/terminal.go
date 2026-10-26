package connector

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

const (
	// terminalReadChunk is small enough that a burst of output does not build a
	// huge frame, and large enough that a fast compiler log is not chopped into
	// thousands of tiny websocket messages.
	terminalReadChunk = 32 * 1024
	// terminalLinkQueue bounds output waiting for the link socket. A terminal
	// that produces faster than the link can drain drops old output rather than
	// blocking the PTY reader; lifecycle frames stay queued for delivery.
	terminalLinkQueue = 512
	// terminalOpenTimeout covers dialing the link and starting the shell. It
	// stays under the server's 10 second create wait so a failure is reported
	// as an error instead of a create that silently times out.
	terminalOpenTimeout = 8 * time.Second
	// terminalInputLimit rejects a paste larger than this; xterm does not send
	// anything close, and an unbounded base64 string would allocate twice.
	terminalInputLimit = 1 << 20
	// terminalScrollbackChunks / terminalScrollbackBytes bound the recent
	// output kept per PTY so a new server process (or a reconnected output
	// link) can rebuild the viewer's screen. Output beyond this is dropped,
	// matching the server's own replay policy.
	terminalScrollbackChunks = 512
	terminalScrollbackBytes  = 1 << 20
)

// terminalProcess is the platform PTY handle. Implementations live in
// terminal_windows.go (ConPTY) and terminal_unix.go (creack/pty).
type terminalProcess interface {
	Read([]byte) (int, error)
	Write([]byte) (int, error)
	Resize(cols, rows int) error
	// Wait blocks until the process tree is gone and returns its exit code.
	Wait() (int, error)
	// Kill terminates the process tree. Close is still required afterwards.
	Kill() error
	// Close shuts the console down and makes Read return; output already in
	// the pipe remains readable until EOF.
	Close() error
	// Release frees the remaining OS handles after Wait and the read loop are
	// done. It never blocks.
	Release()
	PID() int
}

// localTerminal is one running PTY on this machine.
type localTerminal struct {
	id   string
	proc terminalProcess
	// readDone closes when the PTY read loop has drained every byte it can.
	readDone chan struct{}
	reason   atomic.Pointer[string]

	// mu guards the replay state below. The read loop is the only writer;
	// link (re)connects take snapshots from another goroutine.
	mu              sync.Mutex
	seq             uint64
	scrollback      [][]byte
	scrollbackBytes int
}

// appendScrollbackLocked keeps the newest bounded slice of output.
func (lt *localTerminal) appendScrollbackLocked(data []byte) {
	lt.scrollback = append(lt.scrollback, data)
	lt.scrollbackBytes += len(data)
	for (len(lt.scrollback) > terminalScrollbackChunks ||
		lt.scrollbackBytes > terminalScrollbackBytes) && len(lt.scrollback) > 1 {
		lt.scrollbackBytes -= len(lt.scrollback[0])
		lt.scrollback = lt.scrollback[1:]
	}
}

// state renders a snapshot for the server: whether the PTY is alive, the last
// sequence emitted, and the recent output still held.
func (lt *localTerminal) state() protocol.TerminalState {
	lt.mu.Lock()
	defer lt.mu.Unlock()
	size := 0
	for _, chunk := range lt.scrollback {
		size += len(chunk)
	}
	raw := make([]byte, 0, size)
	for _, chunk := range lt.scrollback {
		raw = append(raw, chunk...)
	}
	return protocol.TerminalState{
		TerminalID: lt.id,
		Running:    true,
		Seq:        lt.seq,
		Data:       base64.StdEncoding.EncodeToString(raw),
	}
}

func (lt *localTerminal) exitReason() string {
	if p := lt.reason.Load(); p != nil {
		return *p
	}
	return "exit"
}

func (lt *localTerminal) setReason(reason string) {
	lt.reason.Store(&reason)
}

// terminalManager owns every PTY this connector has started and the single
// socket used to push their output to the server.
type terminalManager struct {
	client *Client
	link   *terminalLink

	mu    sync.Mutex
	terms map[string]*localTerminal
}

// terminalManagers keeps one manager per Client. Client is constructed by
// cmd/aa-connector and by tests; it has no field for terminal state and this
// file may not change the existing struct.
var terminalManagers sync.Map

// terminalManager returns the manager bound to this client.
func (c *Client) terminalManager() *terminalManager {
	if value, ok := terminalManagers.Load(c); ok {
		return value.(*terminalManager)
	}
	manager := &terminalManager{
		client: c,
		terms:  map[string]*localTerminal{},
	}
	manager.link = newTerminalLink(c)
	// Every time the output socket is established (first connect, link
	// reconnect, or a server restart) state each live terminal to the server so
	// it can rebuild its replay buffer.
	manager.link.onConnect = manager.announceRunning
	actual, _ := terminalManagers.LoadOrStore(c, manager)
	return actual.(*terminalManager)
}

// handleTerminalFrame runs one TypeTerminal control frame delivered on the
// main connector socket.
func (c *Client) handleTerminalFrame(raw json.RawMessage) {
	var cmd protocol.TerminalCommand
	if err := json.Unmarshal(raw, &cmd); err != nil {
		c.Log.Warn("bad terminal command", "error", err)
		return
	}
	if cmd.TerminalID == "" {
		c.Log.Warn("terminal command without an id")
		return
	}
	manager := c.terminalManager()
	// Any terminal frame implies somebody cares about a terminal. Make sure
	// the output link exists even when no PTY was opened in this process, so
	// errors such as "terminal_not_found" for a stale server row are actually
	// delivered instead of queued behind a link that was never started.
	manager.link.start()

	switch cmd.Op {
	case protocol.TerminalOpOpen:
		// Open is synchronous on the read loop: the server sends input for a
		// terminal only after it saw opened, and handling this inline keeps
		// that ordering obvious.
		if err := manager.open(cmd); err != nil {
			c.Log.Warn("could not open terminal", "terminal", cmd.TerminalID, "error", err)
			manager.link.emit(protocol.TypeTerminalError, protocol.TerminalError{
				TerminalID: cmd.TerminalID,
				Code:       "terminal_open_failed",
				Message:    err.Error(),
			})
		}
	case protocol.TerminalOpInput:
		manager.write(cmd)
	case protocol.TerminalOpResize:
		manager.resize(cmd)
	case protocol.TerminalOpClose:
		manager.close(cmd.TerminalID)
	case protocol.TerminalOpProbe:
		// ensure may have to dial the server, so do not block the main
		// connector read loop (turn frames and heartbeats share it).
		go manager.probe(cmd.TerminalID)
	default:
		c.Log.Debug("ignoring unknown terminal operation", "op", cmd.Op)
	}
}

// announceRunning sends a state snapshot for every live PTY. It runs whenever
// the output link is established, restoring history after a server restart.
func (m *terminalManager) announceRunning() {
	m.mu.Lock()
	terminals := make([]*localTerminal, 0, len(m.terms))
	for _, lt := range m.terms {
		terminals = append(terminals, lt)
	}
	m.mu.Unlock()
	for _, lt := range terminals {
		m.link.emit(protocol.TypeTerminalState, lt.state())
	}
}

// probe answers the server's liveness question, bringing the link up first so
// the answer has a path home.
func (m *terminalManager) probe(terminalID string) {
	ctx, cancel := context.WithTimeout(context.Background(), terminalOpenTimeout)
	defer cancel()
	if err := m.link.ensure(ctx); err != nil {
		// Nothing can be reported; the server treats the silence as an
		// unavailable terminal.
		return
	}
	state := protocol.TerminalState{TerminalID: terminalID, Running: false}
	if lt := m.lookup(terminalID); lt != nil {
		state = lt.state()
	}
	// The server is waiting on this answer; retry it across a link reconnect
	// rather than dropping it into a dead socket.
	if err := m.link.emitAwait(ctx, protocol.TypeTerminalState, state); err != nil {
		m.client.Log.Debug("could not deliver terminal probe", "terminal", terminalID, "error", err)
	}
}

// open starts a shell and begins forwarding its output.
func (m *terminalManager) open(cmd protocol.TerminalCommand) error {
	cols, rows := clampTerminalSize(cmd.Cols, cmd.Rows)

	m.mu.Lock()
	_, exists := m.terms[cmd.TerminalID]
	m.mu.Unlock()
	if exists {
		return fmt.Errorf("terminal %s already exists", cmd.TerminalID)
	}

	// The output link must be up before the PTY starts, otherwise the banner
	// of a fast shell is produced with nowhere to go.
	ctx, cancel := context.WithTimeout(context.Background(), terminalOpenTimeout)
	defer cancel()
	if err := m.link.ensure(ctx); err != nil {
		return fmt.Errorf("terminal link is not available: %w", err)
	}

	// Resolve the working directory on this machine: the server's
	// filepath.Join would use the server OS if it did this, which breaks a
	// Linux-hosted server driving a Windows desktop.
	proc, err := startTerminal(cmd.Shell, resolveTerminalWorkingDir(cmd.Root, cmd.CWD), cols, rows)
	if err != nil {
		return err
	}

	lt := &localTerminal{id: cmd.TerminalID, proc: proc, readDone: make(chan struct{})}
	m.mu.Lock()
	if _, duplicate := m.terms[cmd.TerminalID]; duplicate {
		m.mu.Unlock()
		_ = proc.Kill()
		_ = proc.Close()
		proc.Release()
		return fmt.Errorf("terminal %s already exists", cmd.TerminalID)
	}
	m.terms[cmd.TerminalID] = lt
	m.mu.Unlock()

	m.link.emit(protocol.TypeTerminalOpened, protocol.TerminalOpened{
		TerminalID: cmd.TerminalID,
		PID:        proc.PID(),
	})
	go m.pump(lt)
	go m.supervise(lt)
	return nil
}

// write forwards decoded keystrokes to the PTY.
func (m *terminalManager) write(cmd protocol.TerminalCommand) {
	if len(cmd.Data) > terminalInputLimit {
		m.emitError(cmd.TerminalID, "input_too_large", "terminal input exceeds 1 MiB")
		return
	}
	data, err := base64.StdEncoding.DecodeString(cmd.Data)
	if err != nil {
		m.emitError(cmd.TerminalID, "bad_input", "terminal input is not valid base64")
		return
	}
	lt := m.lookup(cmd.TerminalID)
	if lt == nil {
		m.emitError(cmd.TerminalID, "terminal_not_found", "terminal is not running on this connector")
		return
	}
	if _, err := lt.proc.Write(data); err != nil {
		m.emitError(cmd.TerminalID, "write_failed", err.Error())
	}
}

// resize applies a new window size to the PTY.
func (m *terminalManager) resize(cmd protocol.TerminalCommand) {
	cols, rows := clampTerminalSize(cmd.Cols, cmd.Rows)
	lt := m.lookup(cmd.TerminalID)
	if lt == nil {
		return
	}
	if err := lt.proc.Resize(cols, rows); err != nil {
		m.emitError(cmd.TerminalID, "resize_failed", err.Error())
	}
}

// close asks the process tree to die. Exit is reported by supervise, exactly
// once, after the last output chunk was read.
func (m *terminalManager) close(terminalID string) {
	lt := m.lookup(terminalID)
	if lt == nil {
		return
	}
	lt.setReason("closed")
	if err := lt.proc.Kill(); err != nil {
		m.emitError(terminalID, "close_failed", err.Error())
	}
}

// lookup finds a running terminal.
func (m *terminalManager) lookup(id string) *localTerminal {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.terms[id]
}

// emitError reports a failed operation to the server.
func (m *terminalManager) emitError(terminalID, code, message string) {
	m.link.emit(protocol.TypeTerminalError, protocol.TerminalError{
		TerminalID: terminalID,
		Code:       code,
		Message:    message,
	})
}

// pump reads the PTY and queues each chunk with a rising sequence number. The
// same bytes are kept in a bounded scrollback so the server can rebuild its
// replay after a restart.
func (m *terminalManager) pump(lt *localTerminal) {
	defer close(lt.readDone)
	buf := make([]byte, terminalReadChunk)
	for {
		n, err := lt.proc.Read(buf)
		if n > 0 {
			// Copy: buf is reused by the next read and the replay window
			// outlives this iteration.
			chunk := append([]byte(nil), buf[:n]...)
			lt.mu.Lock()
			lt.seq++
			seq := lt.seq
			lt.appendScrollbackLocked(chunk)
			lt.mu.Unlock()
			m.link.emit(protocol.TypeTerminalOutput, protocol.TerminalOutput{
				TerminalID: lt.id,
				Seq:        seq,
				Data:       base64.StdEncoding.EncodeToString(chunk),
			})
		}
		if err != nil {
			return
		}
	}
}

// supervise waits for the process to end, drains the console, then reports the
// exit. It is the only place that emits terminal.exit, so a close and a
// natural exit cannot produce two frames.
func (m *terminalManager) supervise(lt *localTerminal) {
	waitCh := make(chan int, 1)
	go func() {
		code, err := lt.proc.Wait()
		if err != nil {
			waitCh <- -1
			return
		}
		waitCh <- code
	}()

	select {
	case code := <-waitCh:
		_ = lt.proc.Close()
		waitRead(lt.readDone, 3*time.Second)
		m.finish(lt, code, true)
	case <-lt.readDone:
		// The read side ended while the process might still be alive. Give a
		// shell that is shutting down a moment, then kill it so the handle is
		// not leaked.
		select {
		case code := <-waitCh:
			_ = lt.proc.Close()
			m.finish(lt, code, true)
		case <-time.After(2 * time.Second):
			_ = lt.proc.Kill()
			code := <-waitCh
			_ = lt.proc.Close()
			m.finish(lt, code, code >= 0)
		}
	}
}

// finish releases the PTY, reports the exit and forgets the terminal.
func (m *terminalManager) finish(lt *localTerminal, code int, haveCode bool) {
	lt.proc.Release()
	var exitCode *int
	if haveCode {
		exitCode = &code
	}
	m.link.emit(protocol.TypeTerminalExit, protocol.TerminalExit{
		TerminalID: lt.id,
		ExitCode:   exitCode,
		Reason:     lt.exitReason(),
	})
	m.mu.Lock()
	if m.terms[lt.id] == lt {
		delete(m.terms, lt.id)
	}
	m.mu.Unlock()
}

// waitRead waits for a reader to finish, giving up after timeout.
func waitRead(done <-chan struct{}, timeout time.Duration) {
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// resolveTerminalWorkingDir joins a workspace root and a relative cwd using
// this connector's path rules.
func resolveTerminalWorkingDir(root, cwd string) string {
	root = strings.TrimSpace(root)
	cwd = strings.TrimSpace(cwd)
	switch {
	case cwd == "":
		return root
	case root == "" || filepath.IsAbs(cwd):
		return cwd
	default:
		return filepath.Join(root, cwd)
	}
}

func clampTerminalSize(cols, rows int) (int, int) {
	if cols < 1 {
		cols = 80
	}
	if cols > 500 {
		cols = 500
	}
	if rows < 1 {
		rows = 24
	}
	if rows > 200 {
		rows = 200
	}
	return cols, rows
}

// terminalOutFrame is one queued envelope. Droppable frames are terminal
// output; lifecycle and probe frames remain queued across output pressure.
type terminalOutFrame struct {
	body      []byte
	ack       chan error
	droppable bool
}

// terminalLink is the connector-opened websocket that carries terminal output.
// It reconnects for as long as the process lives, so a server restart or a
// network blip does not kill the shells.
type terminalLink struct {
	client *Client

	outMu    sync.Mutex
	out      []terminalOutFrame
	outWake  chan struct{}
	outCount int
	startOne sync.Once
	// onConnect runs after each successful dial, before the queue drains. It
	// is how live terminals restate their scrollback to a new server.
	onConnect func()

	mu   sync.Mutex
	conn *websocket.Conn
	// connected is closed while conn is live and replaced at the start of each
	// disconnect, so a waiter can tell "up now" from "up once, long ago".
	connected chan struct{}

	dropped atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
}

func newTerminalLink(c *Client) *terminalLink {
	ctx, cancel := context.WithCancel(context.Background())
	return &terminalLink{
		client:    c,
		outWake:   make(chan struct{}, 1),
		connected: make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
	}
}

// start begins the reconnect loop without waiting for a socket. Callers that
// only need queued frames to have a future path (an error for a terminal this
// process never opened) use this rather than blocking their read loop.
func (l *terminalLink) start() {
	l.startOne.Do(func() { go l.run() })
}

// ensure starts the link loop and waits until a socket is connected now. A
// link that was connected earlier but died (for example because the server
// restarted) must not count: the caller's next write would vanish.
func (l *terminalLink) ensure(ctx context.Context) error {
	l.start()
	for {
		l.mu.Lock()
		connected := l.conn != nil
		ch := l.connected
		l.mu.Unlock()
		if connected {
			return nil
		}
		select {
		case <-ch:
			// Re-check: the channel may have been closed for a socket that
			// has since disconnected again.
		case <-ctx.Done():
			return ctx.Err()
		case <-l.ctx.Done():
			return errors.New("terminal link is closed")
		}
	}
}

// encode builds one envelope.
func (l *terminalLink) encode(frameType string, value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(protocol.Envelope{Type: frameType, Data: data})
}

// enqueue adds a frame, dropping the oldest output when the output queue is
// full. Control frames are not displaced by terminal output.
func (l *terminalLink) enqueue(frame terminalOutFrame) {
	l.outMu.Lock()
	if frame.droppable && l.outCount >= terminalLinkQueue {
		for i, queued := range l.out {
			if !queued.droppable {
				continue
			}
			copy(l.out[i:], l.out[i+1:])
			l.out[len(l.out)-1] = terminalOutFrame{}
			l.out = l.out[:len(l.out)-1]
			l.outCount--
			l.dropped.Add(1)
			break
		}
	}
	l.out = append(l.out, frame)
	if frame.droppable {
		l.outCount++
	}
	l.outMu.Unlock()
	select {
	case l.outWake <- struct{}{}:
	default:
	}
}

// emit queues one envelope without ever blocking the PTY reader. When the
// queue is full the oldest frame is discarded, matching the server's
// slow-consumer policy.
func (l *terminalLink) emit(frameType string, value any) {
	body, err := l.encode(frameType, value)
	if err != nil {
		return
	}
	l.enqueue(terminalOutFrame{
		body:      body,
		droppable: frameType == protocol.TypeTerminalOutput,
	})
}

// emitAwait queues a frame and waits until it was written to a live socket.
// If the socket dies under it, the frame is retried after reconnecting. The
// probe answer uses this: losing it would make the server declare a healthy
// terminal dead.
func (l *terminalLink) emitAwait(ctx context.Context, frameType string, value any) error {
	body, err := l.encode(frameType, value)
	if err != nil {
		return err
	}
	for {
		if err := l.ensure(ctx); err != nil {
			return err
		}
		ack := make(chan error, 1)
		l.enqueue(terminalOutFrame{body: body, ack: ack})
		select {
		case err := <-ack:
			if err == nil {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		// The write did not land. Give the reconnect loop a moment, then wait
		// for the new socket before retrying.
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// run dials and reconnects forever.
func (l *terminalLink) run() {
	backoff := 250 * time.Millisecond
	for l.ctx.Err() == nil {
		target, err := terminalLinkURL(l.client.URL)
		if err != nil {
			l.client.Log.Error("could not derive the terminal link URL", "error", err)
			return
		}
		dialCtx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
		conn, _, err := websocket.Dial(dialCtx, target, &websocket.DialOptions{
			HTTPHeader: map[string][]string{"Authorization": {"Bearer " + l.client.Token}},
		})
		cancel()
		if err != nil {
			l.client.Log.Debug("terminal link dial failed", "error", err, "retry_in", backoff)
			if !l.sleep(backoff) {
				return
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		conn.SetReadLimit(1 << 20)
		l.setConn(conn)
		l.client.Log.Info("terminal link established", "url", target)
		backoff = 250 * time.Millisecond
		if l.onConnect != nil {
			l.onConnect()
		}

		// Reading is what notices a dead peer. Without it an idle link keeps
		// pointing at a socket the server already closed, and the next probe
		// answer is written into the void.
		connDone := make(chan struct{})
		go l.readLoop(conn, connDone)
		l.writeLoop(conn, connDone)

		l.clearConn(conn)
		conn.CloseNow()
		if !l.sleep(backoff) {
			return
		}
	}
}

// readLoop consumes frames the server never sends, only to detect closure.
func (l *terminalLink) readLoop(conn *websocket.Conn, done chan struct{}) {
	defer close(done)
	for {
		if _, _, err := conn.Read(l.ctx); err != nil {
			return
		}
	}
}

// writeLoop drains the queue into one socket until it fails.
func (l *terminalLink) writeLoop(conn *websocket.Conn, done <-chan struct{}) {
	for {
		frame, ok := l.nextFrame(done)
		if !ok {
			return
		}
		ctx, cancel := context.WithTimeout(l.ctx, 10*time.Second)
		err := conn.Write(ctx, websocket.MessageText, frame.body)
		cancel()
		if frame.ack != nil {
			select {
			case frame.ack <- err:
			default:
			}
		}
		if err != nil {
			if frame.ack == nil && !frame.droppable {
				l.requeueFront(frame)
			}
			return
		}
	}
}

func (l *terminalLink) requeueFront(frame terminalOutFrame) {
	l.outMu.Lock()
	l.out = append(l.out, terminalOutFrame{})
	copy(l.out[1:], l.out[:len(l.out)-1])
	l.out[0] = frame
	l.outMu.Unlock()
	select {
	case l.outWake <- struct{}{}:
	default:
	}
}

func (l *terminalLink) nextFrame(done <-chan struct{}) (terminalOutFrame, bool) {
	for {
		l.outMu.Lock()
		if len(l.out) > 0 {
			frame := l.out[0]
			l.out[0] = terminalOutFrame{}
			l.out = l.out[1:]
			if frame.droppable {
				l.outCount--
			}
			hasMore := len(l.out) > 0
			l.outMu.Unlock()
			if hasMore {
				select {
				case l.outWake <- struct{}{}:
				default:
				}
			}
			return frame, true
		}
		l.outMu.Unlock()
		select {
		case <-l.ctx.Done():
			return terminalOutFrame{}, false
		case <-done:
			return terminalOutFrame{}, false
		case <-l.outWake:
		}
	}
}

func (l *terminalLink) setConn(conn *websocket.Conn) {
	l.mu.Lock()
	l.conn = conn
	select {
	case <-l.connected:
	default:
		close(l.connected)
	}
	l.mu.Unlock()
}

func (l *terminalLink) clearConn(conn *websocket.Conn) {
	l.mu.Lock()
	if l.conn == conn {
		l.conn = nil
		l.connected = make(chan struct{})
	}
	l.mu.Unlock()
}

// sleep waits out a backoff, reporting false when the link is finished.
func (l *terminalLink) sleep(delay time.Duration) bool {
	select {
	case <-l.ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

// terminalLinkURL turns the main connector websocket URL into the terminal
// link URL. Both live under /api/v2/connector/, so the suffix is swapped and
// any deployment prefix survives.
func terminalLinkURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	path := strings.TrimRight(parsed.Path, "/")
	path = strings.TrimSuffix(path, "/api/v2/connector/ws")
	parsed.Path = path + protocol.TerminalLinkPath
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}
