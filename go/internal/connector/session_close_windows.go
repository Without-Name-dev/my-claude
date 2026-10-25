//go:build windows

package connector

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// closeProcessTimeout bounds the search-and-kill script. PowerShell itself can
// take a second to start; two minutes covers a machine that is busy.
const closeProcessTimeout = 2 * time.Minute

// closeSessionProcess ends the Claude Code process that runs one conversation
// and the console window hosting it.
//
// The connector never kept a pid: `wt` and `cmd /c start` both hand the process
// to another host and return. The command line is the durable identity, so the
// process is found by the runtime session id it was started with, and its
// parent console is killed as a tree so the window closes instead of being left
// showing "Terminated".
//
// Only lines that pass safeRuntimeSessionID reach this function, so the id
// cannot escape the quoted PowerShell string.
func closeSessionProcess(externalSessionID string) (int, error) {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'SilentlyContinue'
$id = '%s'
$pattern = '--(resume|session-id)\s+' + [regex]::Escape($id) + '(\s|$)'
$killed = 0
$targets = Get-CimInstance Win32_Process | Where-Object {
    $_.CommandLine -and $_.CommandLine -match $pattern
}
foreach ($target in $targets) {
    $console = Get-CimInstance Win32_Process -Filter "ProcessId=$($target.ParentProcessId)"
    $victim = $target.ProcessId
    if ($console -and $console.Name -match '^(cmd|powershell|pwsh|bash)\.exe$' -and
        $console.CommandLine -and $console.CommandLine -match $pattern) {
        $victim = $console.ProcessId
    }
    & taskkill.exe /PID $victim /T /F | Out-Null
    $killed++
}
Write-Output $killed
`, externalSessionID)

	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	done := make(chan struct{})
	var output []byte
	var err error
	go func() {
		output, err = cmd.Output()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(closeProcessTimeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return 0, fmt.Errorf("timed out looking for the process")
	}
	if err != nil {
		return 0, fmt.Errorf("powershell: %w", err)
	}
	killed, convErr := strconv.Atoi(strings.TrimSpace(string(output)))
	if convErr != nil {
		return 0, fmt.Errorf("unexpected output %q", strings.TrimSpace(string(output)))
	}
	return killed, nil
}
