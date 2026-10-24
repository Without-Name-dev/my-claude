//go:build unix

package connector

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/creack/pty"
)

// unixTerminal is one PTY-backed shell on Linux or macOS.
type unixTerminal struct {
	cmd  *exec.Cmd
	ptmx *os.File
	pid  int

	closeOnce sync.Once
}

// startTerminal starts a shell in a new PTY. The shell gets its own process
// group so close can signal the whole tree rather than just the leader.
func startTerminal(shell, cwd string, cols, rows int) (terminalProcess, error) {
	if shell == "" {
		shell = os.Getenv("SHELL")
		if shell == "" {
			shell = "/bin/sh"
		}
	}
	fields := strings.Fields(shell)
	if len(fields) == 0 {
		fields = []string{"/bin/sh"}
	}

	cmd := exec.Command(fields[0], fields[1:]...)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	ptmx, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
	if err != nil {
		return nil, err
	}
	return &unixTerminal{cmd: cmd, ptmx: ptmx, pid: cmd.Process.Pid}, nil
}

func (t *unixTerminal) Read(p []byte) (int, error) { return t.ptmx.Read(p) }

func (t *unixTerminal) Write(p []byte) (int, error) { return t.ptmx.Write(p) }

func (t *unixTerminal) Resize(cols, rows int) error {
	return pty.Setsize(t.ptmx, &pty.Winsize{
		Cols: uint16(cols),
		Rows: uint16(rows),
	})
}

func (t *unixTerminal) Wait() (int, error) {
	err := t.cmd.Wait()
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// A signal death reports -1, which is a truthful "no ordinary code".
		return exitErr.ExitCode(), nil
	}
	return 0, err
}

// Kill signals the process group, so children started by the shell go too.
func (t *unixTerminal) Kill() error {
	if t.pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-t.pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		// Fall back to the leader if group signalling is not permitted.
		if killErr := t.cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
			return err
		}
	}
	return nil
}

// Close closes the PTY master. The read loop sees EIO/EOF and the process, if
// still alive, receives SIGHUP from the terminal driver.
func (t *unixTerminal) Close() error {
	var err error
	t.closeOnce.Do(func() { err = t.ptmx.Close() })
	return err
}

func (t *unixTerminal) Release() {}

func (t *unixTerminal) PID() int { return t.pid }
