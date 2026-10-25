//go:build windows

package connector

import (
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// sessionOpenGrace is how long a Windows Terminal launch may run before it is
// treated as the terminal window rather than as a command that is about to
// exit with an error. `wt` normally hands the command to the terminal process
// and exits at once, so a failure is visible within this window.
const sessionOpenGrace = 3 * time.Second

// sessionOpenSupported reports whether this machine has a way to start a
// terminal. cmd.exe ships with Windows, so the fallback is always available.
func sessionOpenSupported() bool { return true }

// startSessionTerminal opens a new terminal rooted at cwd and runs Claude Code
// in it. Windows Terminal is preferred; a machine without it (or a `wt` that
// rejects the command line) falls back to `cmd /c start`.
//
// The exact command lines are:
//
//	wt.exe -w 0 new-tab -d <cwd> -- cmd.exe /k claude --resume <id>
//	wt.exe -w 0 new-tab -d <cwd> -- cmd.exe /k claude --session-id <id>
//	cmd.exe /c start "" cmd.exe /k claude --resume <id>
//
// The fallback is started with its working directory set to cwd rather than
// passing /D: cmd.exe re-parses its command line, so a path containing `&`
// would otherwise be read as a second command. `cmd /k` keeps the tab open
// after Claude Code exits so the person can read why it failed.
func startSessionTerminal(cwd, externalSessionID, title string, resume bool) (string, error) {
	wtArgs, cmdArgs := sessionTerminalCommandLine(cwd, externalSessionID, resume)
	for _, name := range []string{"wt.exe", "wt"} {
		wt, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if command, opened := launchWindowTerminal(wt, wtArgs); opened {
			return command, nil
		}
	}

	cmd := exec.Command("cmd.exe", cmdArgs...)
	cmd.Dir = cwd
	cmd.Env = childEnvironment()
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start cmd.exe: %w", err)
	}
	_ = cmd.Process.Release()
	return formatCommand("cmd.exe", cmdArgs), nil
}

// sessionTerminalCommandLine builds the two exact command lines this function
// can start, so the preference order and flags stay inspectable without
// launching anything.
//
// cwd appears only as one argument of the Windows Terminal call (Go escapes it
// for CreateProcess). It is deliberately absent from the cmd.exe fallback:
// cmd.exe re-parses its own command line, so the fallback's directory is set
// with exec.Cmd.Dir instead.
func sessionTerminalCommandLine(cwd, externalSessionID string, resume bool) (wtArgs, cmdArgs []string) {
	flag := "--session-id"
	if resume {
		flag = "--resume"
	}
	inner := []string{"cmd.exe", "/k", "claude", flag, externalSessionID}
	// -w -1 always opens a new window. Joining the most recent window as a tab
	// put two conversations side by side in one window, which is not what the
	// person asked for when they opened a conversation from the phone.
	wtArgs = append([]string{"-w", "-1", "new-tab", "-d", windowsTerminalPath(cwd), "--"}, inner...)
	cmdArgs = append([]string{"/c", "start", ""}, inner...)
	return wtArgs, cmdArgs
}

// windowsTerminalPath escapes a value for wt.exe's own command-line parser.
// Windows Terminal splits its command line on ';' even when the separator sits
// inside one argv element, so a project directory containing a semicolon would
// otherwise be read as a second command. A backslash is the documented escape.
// A real Windows path never contains a backslash inside a component, so this
// cannot turn a valid path into something else.
func windowsTerminalPath(path string) string {
	return strings.ReplaceAll(path, ";", `\;`)
}

// launchWindowTerminal starts one GUI launcher and reports whether it accepted
// the request. A launcher that exits non-zero within the grace window is a
// refusal, and the caller tries the next strategy.
func launchWindowTerminal(name string, args []string) (string, bool) {
	command := formatCommand(name, args)
	cmd := exec.Command(name, args...)
	cmd.Env = childEnvironment()
	if err := cmd.Start(); err != nil {
		return command, false
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return command, err == nil
	case <-time.After(sessionOpenGrace):
		// The launcher is still alive; that is the terminal process, not a
		// rejected command line. Reap it in the background so it does not
		// become a zombie if it exits later.
		go func() { <-done }()
		return command, true
	}
}
