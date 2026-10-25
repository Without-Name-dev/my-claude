package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

const (
	// attachmentDownloadTimeout bounds one file. The phone uploads at most
	// 25 MiB and the server is the other end of a normal link, so anything
	// slower than this is a broken transfer, not a large file.
	attachmentDownloadTimeout = 60 * time.Second
	// attachmentMaxBytes mirrors the server's per-file upload ceiling.
	attachmentMaxBytes = 25 << 20
	// attachmentTTL is how long a downloaded file stays next to the bridge.
	// Claude may re-read a path later in the same conversation, so the file
	// outlives the turn that introduced it; a week is long past useful.
	attachmentTTL = 7 * 24 * time.Hour
)

// localAttachment is one file fetched from the server and written to disk.
type localAttachment struct {
	Path      string
	Name      string
	MediaType string
}

// attachmentsDir is where downloaded files live for one conversation.
func (c *Client) attachmentsDir(externalSessionID string) string {
	name := "shared"
	if safeRuntimeSessionID(externalSessionID) {
		name = externalSessionID
	}
	return filepath.Join(c.Bridge.Dir, "attachments", name)
}

// materializeAttachments downloads every attachment the message carries.
//
// The bytes are written under the bridge directory so the plugin, and the
// Claude Code process behind it, can open them as ordinary files: the bridge
// protocol carries text only, so a local path in the prompt is what turns an
// uploaded image into something the model can actually read.
//
// A file that cannot be fetched is reported instead of aborting the message:
// the person's words are worth more than the picture, and the plugin tells the
// model which attachment is missing.
func (c *Client) materializeAttachments(
	ctx context.Context, msg protocol.Submit,
) (files []localAttachment, failures []string) {
	dir := c.attachmentsDir(msg.ExternalSessionID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		for _, a := range msg.Attachments {
			failures = append(failures, a.Name+": could not create the attachment directory")
		}
		c.Log.Warn("could not create the attachment directory", "dir", dir, "error", err)
		return nil, failures
	}
	pruneAttachments(dir, attachmentTTL)

	base := c.httpBase()
	for _, a := range msg.Attachments {
		local, err := c.downloadAttachment(ctx, base, dir, a)
		if err != nil {
			c.Log.Warn("could not fetch an attachment",
				"session", msg.SessionID, "file", a.FileID, "name", a.Name, "error", err)
			failures = append(failures, a.Name+": "+err.Error())
			continue
		}
		files = append(files, local)
	}
	return files, failures
}

// downloadAttachment fetches one file and writes it into dir.
func (c *Client) downloadAttachment(
	ctx context.Context, base, dir string, a protocol.SubmitAttachment,
) (localAttachment, error) {
	if !safeFileID(a.FileID) {
		return localAttachment{}, fmt.Errorf("invalid file id")
	}
	// Only the route the server publishes is fetched: a frame cannot turn the
	// connector into a client for an arbitrary address.
	if !strings.HasPrefix(a.DownloadURL, "/api/v2/connector/sessions/") {
		return localAttachment{}, fmt.Errorf("unexpected download url")
	}
	requestCtx, cancel := context.WithTimeout(ctx, attachmentDownloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, base+a.DownloadURL, nil)
	if err != nil {
		return localAttachment{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return localAttachment{}, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return localAttachment{}, fmt.Errorf("download failed with status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, attachmentMaxBytes+1))
	if err != nil {
		return localAttachment{}, fmt.Errorf("read: %w", err)
	}
	if len(data) == 0 {
		return localAttachment{}, fmt.Errorf("the file is empty")
	}
	if len(data) > attachmentMaxBytes {
		return localAttachment{}, fmt.Errorf("the file exceeds %d bytes", attachmentMaxBytes)
	}
	if a.SHA256 != "" {
		sum := sha256.Sum256(data)
		if !strings.EqualFold(hex.EncodeToString(sum[:]), a.SHA256) {
			return localAttachment{}, fmt.Errorf("the file does not match its checksum")
		}
	}

	name := attachmentFileName(a.Name, a.FileID)
	path := filepath.Join(dir, name)
	if err := writeFileAtomic(path, data); err != nil {
		return localAttachment{}, fmt.Errorf("write: %w", err)
	}
	return localAttachment{Path: path, Name: a.Name, MediaType: a.MediaType}, nil
}

// httpBase turns the connector's websocket URL into the HTTP origin the
// attachment route lives on. ServerURL is set from the pairing file; the
// websocket URL is the same server, so it is the fallback.
func (c *Client) httpBase() string {
	base := strings.TrimSpace(c.ServerURL)
	if base == "" {
		base = c.URL
	}
	switch {
	case strings.HasPrefix(base, "wss://"):
		base = "https://" + strings.TrimPrefix(base, "wss://")
	case strings.HasPrefix(base, "ws://"):
		base = "http://" + strings.TrimPrefix(base, "ws://")
	}
	if trimmed, ok := strings.CutSuffix(base, "/api/v2/connector/ws"); ok {
		base = trimmed
	}
	return strings.TrimRight(base, "/")
}

// promptWithAttachments renders the text the plugin submits. The plugin API
// takes text only, so the paths are part of the prompt; Claude Code's Read
// tool turns each one back into the image or document the person sent.
func promptWithAttachments(text string, files []localAttachment, failures []string) string {
	if len(files) == 0 && len(failures) == 0 {
		return text
	}
	var b strings.Builder
	if len(files) > 0 {
		b.WriteString("[attached files from the My Claude app — open each one with the Read tool]")
		for _, f := range files {
			b.WriteString("\n- ")
			b.WriteString(f.Path)
			if f.MediaType != "" {
				b.WriteString(" (")
				b.WriteString(f.MediaType)
				b.WriteString(")")
			}
		}
	}
	if len(failures) > 0 {
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString("[attachments that could not be delivered — tell the person]")
		for _, f := range failures {
			b.WriteString("\n- ")
			b.WriteString(f)
		}
	}
	if strings.TrimSpace(text) == "" {
		return b.String()
	}
	b.WriteString("\n\n")
	b.WriteString(text)
	return b.String()
}

// attachmentFileName builds a safe on-disk name. The file id keeps two files
// with the same display name apart, and the display name is reduced to
// characters that cannot escape the directory.
func attachmentFileName(displayName, fileID string) string {
	base := filepath.Base(strings.TrimSpace(displayName))
	var b strings.Builder
	for _, r := range base {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '.' || r == '-' || r == '_' || r == ' ':
			b.WriteRune(r)
		}
	}
	cleaned := strings.Trim(b.String(), " .")
	if cleaned == "" {
		cleaned = "attachment"
	}
	if len(cleaned) > 80 {
		cleaned = cleaned[:80]
	}
	return fileID + "-" + cleaned
}

// safeFileID accepts the ids the server mints ("file_...") and nothing that
// could act as a path segment.
func safeFileID(id string) bool {
	if id == "" || len(id) > 128 {
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

// pruneAttachments deletes files older than ttl. It runs per download batch,
// so a connector that never receives attachments never scans the directory.
func pruneAttachments(dir string, ttl time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-ttl)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}
