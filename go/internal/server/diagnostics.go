package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

// diagnosticsDir is where clients may leave a scroll trace. One file per
// session, trimmed to the most recent entries; a person reporting a jump hands
// over the trace by simply leaving the app open.
const diagnosticsDir = "/var/lib/agents-anywhere/diagnostics"

// diagnosticsLimit bounds one session's file. A trace is text, so a few
// megabytes already covers hours of reading.
const diagnosticsLimit = 4 << 20

// diagnosticsBodyLimit bounds one upload; the client sends a ring buffer.
const diagnosticsBodyLimit = 256 << 10

type diagnosticsUpload struct {
	Trace      string `json:"trace"`
	CapturedAt int64  `json:"capturedAt"`
	AppVersion string `json:"appVersion"`
	Build      string `json:"build"`
	Device     string `json:"device"`
}

// handleUploadDiagnostics appends a client trace to the session's log file.
func (a *App) handleUploadDiagnostics(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	var body diagnosticsUpload
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	if body.Trace == "" {
		httpx.OK(w, map[string]any{"stored": false, "serverTime": timestampNow()})
		return
	}
	if len(body.Trace) > diagnosticsBodyLimit {
		body.Trace = body.Trace[len(body.Trace)-diagnosticsBodyLimit:]
		if index := bytes.IndexByte([]byte(body.Trace), '\n'); index >= 0 {
			body.Trace = body.Trace[index+1:]
		}
	}
	if err := os.MkdirAll(diagnosticsDir, 0o755); err != nil {
		httpx.Fail(w, err)
		return
	}
	path := filepath.Join(diagnosticsDir, session.ID+".log")
	entry := fmt.Sprintf("\n=== %s app=%s build=%s device=%s ===\n%s\n",
		time.Now().Format(time.RFC3339), body.AppVersion, body.Build, body.Device, body.Trace)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	_, writeErr := file.WriteString(entry)
	closeErr := file.Close()
	if writeErr != nil {
		httpx.Fail(w, writeErr)
		return
	}
	if closeErr != nil {
		httpx.Fail(w, closeErr)
		return
	}
	trimDiagnostics(path)
	httpx.OK(w, map[string]any{"stored": true, "serverTime": timestampNow()})
}

// trimDiagnostics keeps the newest diagnosticsLimit bytes, starting at a whole
// entry. Losing the oldest trace is fine; a partial first line would read as
// corruption of the newest one.
func trimDiagnostics(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= diagnosticsLimit {
		return
	}
	file, err := os.Open(path)
	if err != nil {
		return
	}
	data, err := func() ([]byte, error) {
		defer file.Close()
		if _, err := file.Seek(info.Size()-diagnosticsLimit, 0); err != nil {
			return nil, err
		}
		return io.ReadAll(file)
	}()
	if err != nil {
		return
	}
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		data = data[index+1:]
	}
	_ = os.WriteFile(path, data, 0o644)
}
