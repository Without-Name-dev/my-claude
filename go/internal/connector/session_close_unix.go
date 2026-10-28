//go:build !windows

package connector

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// closeSessionProcess stops the Claude Code process that runs one
// conversation.
//
// The connector never kept a pid: the terminal is launched by an external
// program and the connector does not stay in its parent chain. The command line
// is the durable identity, so the process is found by the runtime session id it
// was started with. A terminal window left showing the exit stays open; on
// macOS the tab belongs to Terminal and closing it would mean driving another
// application's UI.
func closeSessionProcess(externalSessionID string, _ time.Time) (int, error) {
	killed := 0
	self := os.Getpid()
	for _, flag := range []string{"--resume", "--session-id"} {
		pids, err := findProcesses(flag + " " + externalSessionID)
		if err != nil {
			return killed, err
		}
		for _, pid := range pids {
			if pid == 0 || pid == self {
				continue
			}
			process, err := os.FindProcess(pid)
			if err != nil {
				continue
			}
			if err := process.Signal(syscall.SIGTERM); err != nil {
				continue
			}
			killed++
			go func(p *os.Process) {
				// A process that ignores the polite signal is ended anyway;
				// the person asked for this window to be gone.
				time.Sleep(3 * time.Second)
				_ = p.Kill()
			}(process)
		}
	}
	return killed, nil
}

// findProcesses lists the pids whose command line contains pattern.
func findProcesses(pattern string) ([]int, error) {
	out, err := exec.Command("pgrep", "-f", "--", pattern).Output()
	if err != nil {
		// pgrep exits 1 when nothing matched, which is not an error here.
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil, nil
		}
		return nil, fmt.Errorf("pgrep: %w", err)
	}
	var pids []int
	for _, field := range strings.Fields(string(out)) {
		if pid, convErr := strconv.Atoi(field); convErr == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
