//go:build windows

package connector

import (
	"reflect"
	"strings"
	"testing"
)

func TestSessionTerminalCommandLine(t *testing.T) {
	const (
		cwd = `C:\work\repo`
		id  = "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6"
	)

	wtArgs, cmdArgs := sessionTerminalCommandLine(cwd, id, true)
	wantWT := []string{"-w", "0", "new-tab", "-d", cwd, "--", "cmd.exe", "/k", "claude", "--resume", id}
	wantCmd := []string{"/c", "start", "", "cmd.exe", "/k", "claude", "--resume", id}
	if !reflect.DeepEqual(wtArgs, wantWT) {
		t.Fatalf("Windows Terminal args = %#v, want %#v", wtArgs, wantWT)
	}
	if !reflect.DeepEqual(cmdArgs, wantCmd) {
		t.Fatalf("cmd fallback args = %#v, want %#v", cmdArgs, wantCmd)
	}

	wtArgs, cmdArgs = sessionTerminalCommandLine(cwd, id, false)
	if wtArgs[len(wtArgs)-2] != "--session-id" {
		t.Fatalf("new conversation wt args = %#v, want --session-id", wtArgs)
	}
	if cmdArgs[len(cmdArgs)-2] != "--session-id" {
		t.Fatalf("new conversation cmd args = %#v, want --session-id", cmdArgs)
	}
}

// cmd.exe re-parses its own command line. The fallback must take the directory
// through exec.Cmd.Dir, never as an argument, or a project path containing "&"
// would be read as a second command.
func TestCmdFallbackNeverCarriesTheWorkingDirectory(t *testing.T) {
	cwd := `C:\work & calc`
	wtArgs, cmdArgs := sessionTerminalCommandLine(cwd, "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6", true)

	for _, arg := range cmdArgs {
		if strings.Contains(arg, "calc") || strings.Contains(arg, cwd) {
			t.Fatalf("fallback argument %q carries the working directory", arg)
		}
	}
	// In the Windows Terminal call it must be exactly one argument, so Go
	// escapes it for CreateProcess instead of emitting raw shell text.
	position := -1
	for i, arg := range wtArgs {
		if arg == cwd {
			position = i
		}
	}
	if position < 0 || wtArgs[position-1] != "-d" {
		t.Fatalf("wt args = %#v, want the cwd as one -d argument", wtArgs)
	}
}

// wt.exe treats ';' as a command separator even inside one argument. A
// directory name containing one must not become a second command.
func TestWindowsTerminalPathEscapesTheCommandSeparator(t *testing.T) {
	wtArgs, _ := sessionTerminalCommandLine(`C:\repos\a;b`, "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6", true)
	position := -1
	for i, arg := range wtArgs {
		if arg == `C:\repos\a\;b` {
			position = i
		}
	}
	if position < 0 || wtArgs[position-1] != "-d" {
		t.Fatalf("wt args = %#v, want the semicolon escaped in the -d value", wtArgs)
	}
}
