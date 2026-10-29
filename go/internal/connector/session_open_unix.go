//go:build unix && !darwin

package connector

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// unixTerminalCandidates are the emulators tried when $TERMINAL is unset. The
// order prefers the desktop's configured default, then the common ones.
var unixTerminalCandidates = []string{
	"x-terminal-emulator",
	"gnome-terminal",
	"konsole",
	"xfce4-terminal",
	"xterm",
}

// unixTerminalName returns the emulator to use, or "" when none exists.
// $TERMINAL is only honoured when it names an executable: a value carrying
// arguments is not a path LookPath can resolve, so the candidates below are
// tried instead of failing the whole spawn.
func unixTerminalName() string {
	if terminal := strings.TrimSpace(os.Getenv("TERMINAL")); terminal != "" {
		if _, err := exec.LookPath(terminal); err == nil {
			return terminal
		}
	}
	for _, name := range unixTerminalCandidates {
		if _, err := exec.LookPath(name); err == nil {
			return name
		}
	}
	return ""
}

// sessionOpenSupported reports whether this machine has a terminal emulator the
// connector knows how to drive. A headless box has none, so the Server must not
// promise the phone that a terminal can be opened there.
func sessionOpenSupported() bool { return unixTerminalName() != "" }

// unixTerminalArgs is the command-line shape of one emulator: most accept -e,
// gnome-terminal wants --, and xfce4-terminal wants -x.
func unixTerminalArgs(name string, inner []string) []string {
	switch filepath.Base(name) {
	case "gnome-terminal":
		return append([]string{"--"}, inner...)
	case "xfce4-terminal":
		return append([]string{"-x"}, inner...)
	default:
		return append([]string{"-e"}, inner...)
	}
}

// startSessionTerminal opens a new terminal rooted at cwd and runs Claude Code
// in it.
func startSessionTerminal(cwd, externalSessionID, title string, resume bool) (string, error) {
	flag := "--session-id"
	if resume {
		flag = "--resume"
	}
	inner := []string{"claude", flag, externalSessionID}

	name := unixTerminalName()
	if name == "" {
		return "", errors.New("no terminal emulator found (set $TERMINAL or install x-terminal-emulator, gnome-terminal, konsole, xfce4-terminal or xterm)")
	}
	command, ok := launchUnixTerminal(name, unixTerminalArgs(name, inner), cwd)
	if !ok {
		return "", fmt.Errorf("could not start %s", name)
	}
	return command, nil
}

// launchUnixTerminal starts one emulator, reporting whether it accepted the
// launch. Starting is all that can be checked synchronously: the terminal runs
// the command in a window of its own.
func launchUnixTerminal(name string, args []string, cwd string) (string, bool) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", false
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = cwd
	// The terminal inherits this environment, so a wired model gateway reaches
	// the Claude process it starts.
	cmd.Env = childEnvironment()
	if err := cmd.Start(); err != nil {
		return "", false
	}
	_ = cmd.Process.Release()
	return formatCommand(path, args), true
}
