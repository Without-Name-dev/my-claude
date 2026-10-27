package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// handleRPC runs one server-to-desktop call.
func (c *Client) handleRPC(raw json.RawMessage) {
	var req protocol.RPCRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.ID == "" {
		return
	}

	// Run RPC with timeout to prevent blocking the read loop indefinitely.
	// Large file operations have their own chunking limits; this timeout is a
	// backstop against stuck filesystem calls or runaway processing.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	var (
		result json.RawMessage
		rpcErr *protocol.RPCError
	)

	go func() {
		defer close(done)
		switch req.Method {
		case "fs.list":
			result, rpcErr = c.fsList(req.Params)
		case "fs.read":
			result, rpcErr = c.fsRead(req.Params)
		case "fs.readText":
			result, rpcErr = c.fsReadText(req.Params)
		case "fs.write":
			result, rpcErr = c.fsWrite(req.Params)
		case "fs.size":
			result, rpcErr = c.fsSize(req.Params)
		case "ping":
			result, _ = json.Marshal(map[string]any{"ok": true})
		default:
			rpcErr = &protocol.RPCError{Code: "unknown_method", Message: "unknown method: " + req.Method}
		}
	}()

	select {
	case <-done:
		// RPC completed normally
	case <-ctx.Done():
		rpcErr = &protocol.RPCError{Code: "timeout", Message: "RPC call exceeded timeout"}
	}

	_ = c.send(protocol.TypeRPCResponse, protocol.RPCResponse{
		ID: req.ID, Result: result, Error: rpcErr,
	})
}

// maxFSEntries caps one directory listing.
const maxFSEntries = 2000

// fsEntry is one row in a directory listing.
type fsEntry struct {
	Name       string `json:"name"`
	Path       string `json:"path"`
	Type       string `json:"type"`
	Size       int64  `json:"size,omitempty"`
	ModifiedAt int64  `json:"modifiedAt,omitempty"`
}

// fsList reads a directory for the phone's file browser.
func (c *Client) fsList(raw json.RawMessage) (json.RawMessage, *protocol.RPCError) {
	var params struct {
		Root string `json:"root"`
		Path string `json:"path"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, &protocol.RPCError{Code: "bad_params", Message: err.Error()}
		}
	}
	pathRaw := strings.TrimSpace(params.Path)
	root := strings.TrimSpace(params.Root)
	var cleaned string
	if root != "" {
		// Resolve exactly like fs.read and fs.write. Listing used to pass a
		// relative path straight to ReadDir, so `{root:"/repo", path:"src"}`
		// listed the connector's own working directory, and an absolute path
		// through a symlink was listed even though reading it was refused.
		listPath := pathRaw
		if listPath == "" {
			listPath = "."
		}
		_, target, err := resolveFSPath(root, listPath)
		if err != nil {
			return nil, &protocol.RPCError{Code: "fs_error", Message: err.Error()}
		}
		cleaned = target
	} else {
		target := pathRaw
		// The client sends "~" for the home directory, which Windows does not
		// expand on its own.
		if target == "~" || strings.HasPrefix(target, "~/") || strings.HasPrefix(target, "~\\") {
			home, _ := os.UserHomeDir()
			target = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(target, "~"), string(os.PathSeparator)))
		}
		if target == "" {
			target, _ = os.UserHomeDir()
		}
		cleaned = filepath.Clean(target)
	}

	entries, err := os.ReadDir(cleaned)
	if err != nil {
		return nil, &protocol.RPCError{Code: "fs_error", Message: err.Error()}
	}
	// A directory with a hundred thousand files would otherwise become a
	// hundred-thousand-row JSON payload the phone cannot render.
	truncated := false
	if len(entries) > maxFSEntries {
		entries = entries[:maxFSEntries]
		truncated = true
	}
	out := make([]fsEntry, 0, len(entries))
	for _, entry := range entries {
		info, infoErr := entry.Info()
		item := fsEntry{
			Name: entry.Name(),
			Path: filepath.Join(cleaned, entry.Name()),
			Type: "file",
		}
		if entry.IsDir() {
			item.Type = "directory"
		}
		if infoErr == nil {
			if !entry.IsDir() {
				item.Size = info.Size()
			}
			item.ModifiedAt = info.ModTime().UnixMilli()
		}
		out = append(out, item)
	}
	// Directories first, then names — the order a person expects in a picker.
	sort.SliceStable(out, func(i, j int) bool {
		if (out[i].Type == "directory") != (out[j].Type == "directory") {
			return out[i].Type == "directory"
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})

	body, _ := json.Marshal(map[string]any{
		"path":      cleaned,
		"entries":   out,
		"truncated": truncated,
	})
	return body, nil
}

// maxFSReadChunk keeps one fs.read answer well under the connector
// websocket's 8 MiB frame limit after base64 inflation.
const maxFSReadChunk = 2 << 20

const (
	defaultFSReadTextBytes = 1 << 20
	maxFSReadTextBytes     = 4 << 20
)

// fsRPCParams is the superset of parameters used by the filesystem RPCs.
type fsRPCParams struct {
	Root          string  `json:"root"`
	Path          string  `json:"path"`
	Offset        int64   `json:"offset"`
	Length        int64   `json:"length"`
	MaxBytes      *int    `json:"maxBytes"`
	ContentBase64 string  `json:"contentBase64"`
	Encoding      string  `json:"encoding"`
	IfMatch       *string `json:"ifMatch"`
}

// fsFileInfo is the metadata every file RPC returns.
type fsFileInfo struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
}

func decodeFSParams(raw json.RawMessage) (fsRPCParams, *protocol.RPCError) {
	var params fsRPCParams
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return params, &protocol.RPCError{Code: "bad_params", Message: err.Error()}
		}
	}
	return params, nil
}

// resolveFSPath anchors path inside root and rejects escapes.
//
// Two checks are needed: a lexical one catches ".." before touching the disk,
// and a symlink-aware one catches a link inside the root that points outside
// it. filepath.Rel also rejects a target on another Windows volume.
func resolveFSPath(rootRaw, pathRaw string) (string, string, error) {
	rootRaw = strings.TrimSpace(rootRaw)
	if rootRaw == "" {
		return "", "", errors.New("root is required")
	}
	if strings.TrimSpace(pathRaw) == "" {
		return "", "", errors.New("path is required")
	}
	if strings.IndexByte(rootRaw, 0) >= 0 || strings.IndexByte(pathRaw, 0) >= 0 {
		return "", "", errors.New("path contains a NUL byte")
	}

	rootAbs, err := filepath.Abs(expandFSHome(rootRaw))
	if err != nil {
		return "", "", err
	}
	rootReal, err := filepath.EvalSymlinks(rootAbs)
	if err != nil {
		return "", "", fmt.Errorf("root is not accessible: %w", err)
	}
	rootInfo, err := os.Stat(rootReal)
	if err != nil {
		return "", "", err
	}
	if !rootInfo.IsDir() {
		return "", "", errors.New("root is not a directory")
	}

	target := expandFSHome(pathRaw)
	if !filepath.IsAbs(target) {
		target = filepath.Join(rootAbs, target)
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	if !fsWithin(rootAbs, targetAbs) {
		return "", "", fmt.Errorf("path escapes root: %s", pathRaw)
	}

	realTarget := targetAbs
	if resolved, err := filepath.EvalSymlinks(targetAbs); err == nil {
		realTarget = resolved
	} else if errors.Is(err, os.ErrNotExist) {
		// A write target does not exist yet; resolve its parent so a symlinked
		// directory outside the root cannot be used as a hiding place.
		parentReal, parentErr := filepath.EvalSymlinks(filepath.Dir(targetAbs))
		if parentErr != nil {
			return "", "", fmt.Errorf("parent directory is not accessible: %w", parentErr)
		}
		realTarget = filepath.Join(parentReal, filepath.Base(targetAbs))
	} else {
		return "", "", err
	}
	if !fsWithin(rootReal, realTarget) {
		return "", "", fmt.Errorf("path escapes root through a symbolic link: %s", pathRaw)
	}
	return rootReal, realTarget, nil
}

func fsWithin(base, target string) bool {
	rel, err := filepath.Rel(base, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func expandFSHome(value string) string {
	if value != "~" && !strings.HasPrefix(value, "~/") && !strings.HasPrefix(value, `~\`) {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return value
	}
	rest := strings.TrimLeft(value[1:], `/\`)
	if rest == "" {
		return home
	}
	return filepath.Join(home, rest)
}

func (c *Client) fsSize(raw json.RawMessage) (json.RawMessage, *protocol.RPCError) {
	params, rpcErr := decodeFSParams(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	_, target, err := resolveFSPath(params.Root, params.Path)
	if err != nil {
		return nil, fsResolveError(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, fsIOError(err)
	}
	if info.IsDir() {
		return nil, &protocol.RPCError{Code: "is_directory", Message: "path is a directory: " + target}
	}
	if !info.Mode().IsRegular() {
		return nil, &protocol.RPCError{Code: "bad_params", Message: "path is not a regular file: " + target}
	}
	digest, err := hashFSPath(target)
	if err != nil {
		return nil, fsIOError(err)
	}
	body, _ := json.Marshal(fsFileInfo{
		Path:      target,
		Name:      filepath.Base(target),
		Size:      info.Size(),
		SHA256:    digest,
		MediaType: fsMediaType(target),
	})
	return body, nil
}

func (c *Client) fsRead(raw json.RawMessage) (json.RawMessage, *protocol.RPCError) {
	params, rpcErr := decodeFSParams(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	_, target, err := resolveFSPath(params.Root, params.Path)
	if err != nil {
		return nil, fsResolveError(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, fsIOError(err)
	}
	if info.IsDir() {
		return nil, &protocol.RPCError{Code: "is_directory", Message: "path is a directory: " + target}
	}

	offset := params.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > info.Size() {
		offset = info.Size()
	}
	length := params.Length
	if length < 0 || length > maxFSReadChunk {
		length = maxFSReadChunk
	}
	if remaining := info.Size() - offset; length > remaining {
		length = remaining
	}

	var data []byte
	if length > 0 {
		file, err := os.Open(target)
		if err != nil {
			return nil, fsIOError(err)
		}
		_, seekErr := file.Seek(offset, io.SeekStart)
		if seekErr == nil {
			data, seekErr = io.ReadAll(io.LimitReader(file, length))
		}
		closeErr := file.Close()
		if seekErr != nil {
			return nil, fsIOError(seekErr)
		}
		if closeErr != nil {
			return nil, fsIOError(closeErr)
		}
	}

	body, _ := json.Marshal(map[string]any{
		"path":          target,
		"name":          filepath.Base(target),
		"size":          info.Size(),
		"mediaType":     fsMediaType(target),
		"offset":        offset,
		"length":        len(data),
		"eof":           offset+int64(len(data)) >= info.Size(),
		"contentBase64": base64.StdEncoding.EncodeToString(data),
	})
	return body, nil
}

func (c *Client) fsReadText(raw json.RawMessage) (json.RawMessage, *protocol.RPCError) {
	params, rpcErr := decodeFSParams(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	_, target, err := resolveFSPath(params.Root, params.Path)
	if err != nil {
		return nil, fsResolveError(err)
	}
	info, err := os.Stat(target)
	if err != nil {
		return nil, fsIOError(err)
	}
	if info.IsDir() {
		return nil, &protocol.RPCError{Code: "is_directory", Message: "path is a directory: " + target}
	}

	maxBytes := defaultFSReadTextBytes
	if params.MaxBytes != nil {
		maxBytes = *params.MaxBytes
	}
	if maxBytes < 1 {
		maxBytes = 1
	}
	if maxBytes > maxFSReadTextBytes {
		maxBytes = maxFSReadTextBytes
	}

	file, err := os.Open(target)
	if err != nil {
		return nil, fsIOError(err)
	}
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		_ = file.Close()
		return nil, fsIOError(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, fsIOError(err)
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(maxBytes)))
	closeErr := file.Close()
	if err != nil {
		return nil, fsIOError(err)
	}
	if closeErr != nil {
		return nil, fsIOError(closeErr)
	}
	truncated := info.Size() > int64(len(data))

	// A NUL byte is the cheap, reliable signal that a file is binary. The
	// browser editor cannot represent one, and the server answers 422.
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, &protocol.RPCError{Code: "not_text", Message: "file contains binary data"}
	}
	textBytes := data
	if truncated && !utf8.Valid(textBytes) {
		// The byte limit may have cut a multi-byte rune in half. Drop at most
		// one incomplete rune from the tail before deciding it is invalid.
		for i := 0; i < utf8.UTFMax-1 && len(textBytes) > 0 && !utf8.Valid(textBytes); i++ {
			textBytes = textBytes[:len(textBytes)-1]
		}
	}
	if !utf8.Valid(textBytes) {
		return nil, &protocol.RPCError{Code: "not_text", Message: "file is not valid UTF-8"}
	}

	body, _ := json.Marshal(map[string]any{
		"path":      target,
		"name":      filepath.Base(target),
		"size":      info.Size(),
		"sha256":    hex.EncodeToString(hasher.Sum(nil)),
		"mediaType": fsMediaType(target),
		"encoding":  "utf-8",
		"text":      string(textBytes),
		"truncated": truncated,
		"binary":    false,
	})
	return body, nil
}

func (c *Client) fsWrite(raw json.RawMessage) (json.RawMessage, *protocol.RPCError) {
	params, rpcErr := decodeFSParams(raw)
	if rpcErr != nil {
		return nil, rpcErr
	}
	switch params.Encoding {
	case "", "utf8", "utf-8", "base64":
	default:
		return nil, &protocol.RPCError{Code: "bad_params", Message: "only utf8 and base64 encodings are supported"}
	}
	data, err := base64.StdEncoding.DecodeString(params.ContentBase64)
	if err != nil {
		return nil, &protocol.RPCError{Code: "bad_params", Message: "contentBase64 is not valid base64"}
	}

	_, target, err := resolveFSPath(params.Root, params.Path)
	if err != nil {
		return nil, fsResolveError(err)
	}
	info, statErr := os.Stat(target)
	if statErr == nil && info.IsDir() {
		return nil, &protocol.RPCError{Code: "is_directory", Message: "path is a directory: " + target}
	}
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return nil, fsIOError(statErr)
	}

	// ifMatch is the optimistic-concurrency guard the editor sends back with
	// its previous sha256; an empty value means "the file must not exist".
	if params.IfMatch != nil {
		current := ""
		if statErr == nil {
			current, err = hashFSPath(target)
			if err != nil {
				return nil, fsIOError(err)
			}
		}
		if *params.IfMatch != current {
			return nil, &protocol.RPCError{
				Code: "stale",
				Message: fmt.Sprintf(
					"file changed on disk (expected sha256=%s, found sha256=%s)",
					*params.IfMatch, current,
				),
			}
		}
	}

	// Write a sibling temporary file and rename it into place, so a crash or a
	// full disk cannot leave the user with a half-written file.
	temp, err := os.CreateTemp(filepath.Dir(target), ".aa-fs-write-*")
	if err != nil {
		return nil, fsIOError(err)
	}
	tempName := temp.Name()
	removeTemp := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	if _, err := temp.Write(data); err != nil {
		removeTemp()
		return nil, fsIOError(err)
	}
	if err := temp.Sync(); err != nil {
		removeTemp()
		return nil, fsIOError(err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return nil, fsIOError(err)
	}
	mode := os.FileMode(0o644)
	if statErr == nil {
		mode = info.Mode().Perm()
	}
	if err := os.Chmod(tempName, mode); err != nil {
		_ = os.Remove(tempName)
		return nil, fsIOError(err)
	}
	if err := os.Rename(tempName, target); err != nil {
		_ = os.Remove(tempName)
		return nil, fsIOError(err)
	}

	digest := sha256.Sum256(data)
	body, _ := json.Marshal(map[string]any{
		"path":         target,
		"encoding":     "utf8",
		"bytesWritten": len(data),
		"sha256":       hex.EncodeToString(digest[:]),
	})
	return body, nil
}

func hashFSPath(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func fsMediaType(path string) string {
	guessed := mime.TypeByExtension(filepath.Ext(path))
	if guessed == "" {
		return "application/octet-stream"
	}
	if parsed, _, err := mime.ParseMediaType(guessed); err == nil {
		return parsed
	}
	return guessed
}

func fsResolveError(err error) *protocol.RPCError {
	if errors.Is(err, os.ErrNotExist) {
		return &protocol.RPCError{Code: "not_found", Message: err.Error()}
	}
	return &protocol.RPCError{Code: "bad_params", Message: err.Error()}
}

func fsIOError(err error) *protocol.RPCError {
	if errors.Is(err, os.ErrNotExist) {
		return &protocol.RPCError{Code: "not_found", Message: err.Error()}
	}
	return &protocol.RPCError{Code: "fs_error", Message: err.Error()}
}
