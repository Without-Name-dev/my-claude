//go:build darwin

package connector

import (
	"fmt"
	"os/exec"
	"strings"
)

// sessionOpenSupported reports whether this machine can be asked to start a
// terminal. Terminal.app ships with macOS and is scripted through osascript.
func sessionOpenSupported() bool {
	_, err := exec.LookPath("osascript")
	return err == nil
}

// startSessionTerminal opens Terminal.app and runs Claude Code in cwd.
//
// There is no command-line flag that starts a program in Terminal.app, so the
// window is opened by asking it to run a shell line. Every value in that line
// is single-quoted for the shell first, and the whole line is escaped again as
// an AppleScript string literal, so a project path containing quotes, dollar
// signs or backticks stays one argument.
func startSessionTerminal(cwd, externalSessionID, title string, resume bool) (string, error) {
	flag := "--session-id"
	if resume {
		flag = "--resume"
	}
	line := "cd -- " + posixQuote(cwd) + " && exec claude " + flag + " " + posixQuote(externalSessionID)
	script := "tell application \"Terminal\"\nactivate\ndo script " + appleScriptString(line) + "\nend tell"
	args := []string{"-e", script}
	if err := exec.Command("/usr/bin/osascript", args...).Run(); err != nil {
		return "", fmt.Errorf("open Terminal: %w", err)
	}
	return formatCommand("osascript", args), nil
}

// posixQuote quotes one value for the shell Terminal.app runs. Single quotes
// stop every expansion; an embedded single quote is closed, escaped with a
// backslash and reopened.
func posixQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// appleScriptString renders value as an AppleScript string literal. Backslashes
// must be escaped before quotes, and the replacer does both in one pass so the
// escapes themselves are not escaped again.
func appleScriptString(value string) string {
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"\t", `\t`,
	).Replace(value)
	return `"` + escaped + `"`
}
