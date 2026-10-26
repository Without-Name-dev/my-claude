package server

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/agents-anywhere/agents-anywhere/internal/auth"
	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

const (
	fsTransferTTL   = 300 * time.Second
	fsTransferChunk = 1 << 20
	fsMaxPushBytes  = 256 << 20

	fsPreviewTokenKind  = "fs_preview"
	fsPreviewStageOpen  = "open"
	fsPreviewStageRead  = "access"
	fsPreviewTokenTTL   = 300
	fsDefaultTextBytes  = 1 << 20
	fsMaxTextBytes      = 4 << 20
	fsMaxReadRPCSeconds = 30 * time.Second
	// fs.size hashes the whole file on the connector to report sha256, so it
	// needs a budget of its own: a 256MiB file on a slow disk routinely
	// exceeds the 30s that one chunk read is allowed.
	fsMaxSizeRPCSeconds = 2 * time.Minute
)

// registerFileRoutes installs the connector filesystem endpoints.
//
// fs/list lives in app.go; everything here was missing from the Go rewrite and
// answered 404 to the web file panel.
func (a *App) registerFileRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/fs/read",
		a.requireUser(a.handleFSRead))
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/fs/readText",
		a.requireUser(a.handleFSReadText))
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/fs/write",
		a.requireUser(a.handleFSWrite))
	mux.HandleFunc("POST /api/v2/connectors/{connector_id}/fs/preview-token",
		a.requireUser(a.handleFSPreviewToken))
	mux.HandleFunc("POST /api/v2/connectors/fs/preview-session",
		a.handleFSPreviewSession)

	// The browser shares the preview access token with an <iframe>, which
	// cannot set an Authorization header. Both URL shapes are registered
	// because the panel in the wild has used both.
	mux.HandleFunc("POST /api/v2/fs/preview/read", a.handleFSPreviewRead)
	mux.HandleFunc("POST /api/v2/fs/preview/readText", a.handleFSPreviewReadText)
	mux.HandleFunc("POST /api/v2/connectors/fs/preview/read", a.handleFSPreviewRead)
	mux.HandleFunc("POST /api/v2/connectors/fs/preview/readText", a.handleFSPreviewReadText)

	mux.HandleFunc("GET /api/v2/connectors/{connector_id}/fs/transfers/{transfer_id}",
		a.handleFSTransferDownload)
	// The Python server lets the connector push prepared bytes here instead of
	// pulling them chunk by chunk. Both mechanisms are accepted.
	mux.HandleFunc("PUT /api/v2/connector/fs/transfers/{transfer_id}",
		a.handleFSTransferUpload)
}

// fsReadBody is shared by read, readText, write and preview-token.
type fsReadBody struct {
	Root          string  `json:"root"`
	Path          string  `json:"path"`
	MaxBytes      int     `json:"maxBytes"`
	ContentBase64 *string `json:"contentBase64"`
	Content       *string `json:"content"`
	Encoding      string  `json:"encoding"`
	IfMatch       *string `json:"ifMatch"`
}

// fsResolved is a request path after lexical validation. Path stays in the
// form the client sent (relative paths remain relative) so the connector can
// resolve it against its own root, which may be on another operating system.
type fsResolved struct {
	Root string
	Path string
}

type fsRPCFile struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
}

type fsRPCRead struct {
	fsRPCFile
	Offset        int64  `json:"offset"`
	Length        int64  `json:"length"`
	EOF           bool   `json:"eof"`
	ContentBase64 string `json:"contentBase64"`
}

type fsRPCText struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
	Encoding  string `json:"encoding"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
}

// callFS runs one filesystem RPC and decodes its JSON result.
func (a *App) callFS(ctx context.Context, connectorID, method string, params, dst any) error {
	return a.callFSWithTimeout(ctx, connectorID, method, params, dst, fsMaxReadRPCSeconds)
}

// callFSWithTimeout is callFS with an explicit budget. fs.size gets a longer
// one because its answer requires hashing the whole file.
func (a *App) callFSWithTimeout(
	ctx context.Context, connectorID, method string, params, dst any, timeout time.Duration,
) error {
	raw, err := a.callConnector(ctx, connectorID, method, params, timeout)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return fmt.Errorf("connector returned an empty %s result", method)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return fmt.Errorf("connector returned invalid %s result: %w", method, err)
	}
	return nil
}

func (a *App) handleFSRead(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	var body fsReadBody
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	target, err := resolveFSTarget(bodyRoot(r, body.Root), body.Path)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !a.connectors.Online(connector.ID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	result, err := a.prepareFSDownload(r.Context(), connector.ID, target, "")
	if err != nil {
		a.writeFSError(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"ok": true, "result": result, "error": nil, "serverTime": timestampNow(),
	})
}

func (a *App) handleFSReadText(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	var body fsReadBody
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	target, err := resolveFSTarget(bodyRoot(r, body.Root), body.Path)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	maxBytes, err := fsTextLimit(body.MaxBytes)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !a.connectors.Online(connector.ID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	result, err := a.readFSText(r.Context(), connector.ID, target, maxBytes)
	if err != nil {
		a.writeFSError(w, err)
		return
	}
	httpx.OK(w, result)
}

func (a *App) handleFSWrite(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	var body fsReadBody
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	target, err := resolveFSTarget(bodyRoot(r, body.Root), body.Path)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	data, err := fsWriteContent(body)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if body.IfMatch != nil && *body.IfMatch != "" {
		if len(*body.IfMatch) != 64 {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
				"ifMatch must be a 64-character sha256"))
			return
		}
		if _, err := hex.DecodeString(*body.IfMatch); err != nil {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
				"ifMatch must be a hexadecimal sha256"))
			return
		}
	}
	if !a.connectors.Online(connector.ID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}

	params := map[string]any{
		"root":          target.Root,
		"path":          target.Path,
		"contentBase64": base64.StdEncoding.EncodeToString(data),
		"encoding":      "utf8",
	}
	if body.IfMatch != nil {
		params["ifMatch"] = *body.IfMatch
	}
	raw, err := a.callConnector(r.Context(), connector.ID, "fs.write", params, fsMaxReadRPCSeconds)
	if err != nil {
		a.writeFSError(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"ok": true, "result": json.RawMessage(raw), "error": nil, "serverTime": timestampNow(),
	})
}

func (a *App) handleFSPreviewToken(w http.ResponseWriter, r *http.Request, user store.User) {
	connector, ok := a.ownedConnector(w, r, user)
	if !ok {
		return
	}
	var body fsReadBody
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	target, err := resolveFSTarget(bodyRoot(r, body.Root), body.Path)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	// Preview does not require the connector to be online: issuing the token
	// can happen while the desktop sleeps, and the read fails later with 409.
	previewToken, err := auth.CreateSignedToken(fsPreviewTokenKind, map[string]any{
		"userId":      user.ID,
		"connectorId": connector.ID,
		"root":        target.Root,
		"path":        target.Path,
		"stage":       fsPreviewStageOpen,
		"jti":         store.NewID("fspt"),
	}, fsPreviewTokenTTL)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"previewToken": previewToken,
		"expiresAt":    fsExpiresAt(fsPreviewTokenTTL),
		"serverTime":   timestampNow(),
	})
}

func (a *App) handleFSPreviewSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PreviewToken string `json:"previewToken"`
	}
	if err := httpx.Decode(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	payload, err := auth.VerifySignedToken(fsPreviewTokenKind, body.PreviewToken)
	if err != nil || fsString(payload, "stage") != fsPreviewStageOpen {
		httpx.Fail(w, httpx.BadRequest("invalid preview token"))
		return
	}
	userID := fsString(payload, "userId")
	connectorID := fsString(payload, "connectorId")
	root := fsString(payload, "root")
	filePath := fsString(payload, "path")
	jti := fsString(payload, "jti")
	if userID == "" || connectorID == "" || root == "" || filePath == "" || jti == "" {
		httpx.Fail(w, httpx.BadRequest("invalid preview token"))
		return
	}
	connector, err := a.db.ConnectorByID(r.Context(), connectorID)
	if err != nil || connector.UserID != userID || connector.Revoked != 0 {
		httpx.Fail(w, httpx.BadRequest("invalid preview token"))
		return
	}
	if !fsPreviewUses.consume(jti, fsTokenExpiry(payload)) {
		httpx.Fail(w, httpx.BadRequest("preview token was already used or expired"))
		return
	}
	accessToken, err := auth.CreateSignedToken(fsPreviewTokenKind, map[string]any{
		"userId":      userID,
		"connectorId": connectorID,
		"root":        root,
		"path":        filePath,
		"stage":       fsPreviewStageRead,
	}, fsPreviewTokenTTL)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"previewAccessToken": accessToken,
		"expiresAt":          fsExpiresAt(fsPreviewTokenTTL),
		"connectorId":        connectorID,
		"root":               root,
		"path":               filePath,
		"serverTime":         timestampNow(),
	})
}

func (a *App) handleFSPreviewRead(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PreviewAccessToken string `json:"previewAccessToken"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	connector, target, ok := a.previewAccess(w, r, body.PreviewAccessToken)
	if !ok {
		return
	}
	if !a.connectors.Online(connector.ID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	// Preview always returns a transfer: an <iframe> or <img> must be able to
	// point at downloadUrl, and a download URL needs no Authorization header.
	result, err := a.prepareFSDownload(r.Context(), connector.ID, target, body.PreviewAccessToken)
	if err != nil {
		a.writeFSError(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"ok": true, "result": result, "error": nil, "serverTime": timestampNow(),
	})
}

func (a *App) handleFSPreviewReadText(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PreviewAccessToken string `json:"previewAccessToken"`
		MaxBytes           int    `json:"maxBytes"`
	}
	if err := httpx.DecodeOptional(r, &body); err != nil {
		httpx.Fail(w, err)
		return
	}
	connector, target, ok := a.previewAccess(w, r, body.PreviewAccessToken)
	if !ok {
		return
	}
	maxBytes, err := fsTextLimit(body.MaxBytes)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	if !a.connectors.Online(connector.ID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	result, err := a.readFSText(r.Context(), connector.ID, target, maxBytes)
	if err != nil {
		a.writeFSError(w, err)
		return
	}
	httpx.OK(w, result)
}

// previewAccess verifies a preview access token and the connector it names.
func (a *App) previewAccess(
	w http.ResponseWriter, r *http.Request, raw string,
) (store.Connector, fsResolved, bool) {
	if strings.TrimSpace(raw) == "" {
		httpx.Fail(w, httpx.Unauthorized("preview access token is required"))
		return store.Connector{}, fsResolved{}, false
	}
	payload, err := auth.VerifySignedToken(fsPreviewTokenKind, raw)
	if err != nil || fsString(payload, "stage") != fsPreviewStageRead {
		httpx.Fail(w, httpx.Unauthorized("invalid preview access token"))
		return store.Connector{}, fsResolved{}, false
	}
	userID := fsString(payload, "userId")
	connectorID := fsString(payload, "connectorId")
	target := fsResolved{Root: fsString(payload, "root"), Path: fsString(payload, "path")}
	if userID == "" || connectorID == "" || target.Root == "" || target.Path == "" {
		httpx.Fail(w, httpx.Unauthorized("invalid preview access token"))
		return store.Connector{}, fsResolved{}, false
	}
	connector, ok := a.connectorOwnedBy(r.Context(), connectorID, userID)
	if !ok {
		httpx.Fail(w, httpx.NotFound("connector not found"))
		return store.Connector{}, fsResolved{}, false
	}
	return connector, target, true
}

func (a *App) handleFSTransferDownload(w http.ResponseWriter, r *http.Request) {
	connectorID := r.PathValue("connector_id")
	transferID := r.PathValue("transfer_id")
	token := r.URL.Query().Get("token")
	previewRaw := r.URL.Query().Get("previewAccessToken")

	if previewRaw != "" {
		payload, err := auth.VerifySignedToken(fsPreviewTokenKind, previewRaw)
		if err != nil || fsString(payload, "stage") != fsPreviewStageRead {
			httpx.Fail(w, httpx.Unauthorized("invalid preview access token"))
			return
		}
		userID := fsString(payload, "userId")
		if userID == "" {
			httpx.Fail(w, httpx.Unauthorized("invalid preview access token"))
			return
		}
		if _, ok := a.connectorOwnedBy(r.Context(), connectorID, userID); !ok {
			httpx.Fail(w, httpx.NotFound("connector not found"))
			return
		}
	} else {
		user, ok := a.userFromRequest(r)
		if !ok {
			httpx.Fail(w, httpx.Unauthorized("sign in required"))
			return
		}
		if _, ok := a.connectorOwnedBy(r.Context(), connectorID, user.ID); !ok {
			httpx.Fail(w, httpx.NotFound("connector not found"))
			return
		}
	}

	transfer, ok := fsTransfers.get(transferID, token)
	if !ok || transfer.ConnectorID != connectorID {
		httpx.Fail(w, httpx.NotFound("transfer not found"))
		return
	}
	if previewRaw != "" {
		payload, _ := auth.VerifySignedToken(fsPreviewTokenKind, previewRaw)
		if fsString(payload, "connectorId") != connectorID ||
			fsString(payload, "root") != transfer.Root ||
			fsString(payload, "path") != transfer.Path {
			httpx.Fail(w, httpx.NotFound("transfer not found"))
			return
		}
	}
	if !a.connectors.Online(connectorID) {
		httpx.Fail(w, httpx.Conflict("connector is offline"))
		return
	}
	a.serveFSTransfer(w, r, transfer)
}

func (a *App) handleFSTransferUpload(w http.ResponseWriter, r *http.Request) {
	connector, ok := a.authenticateConnector(r)
	if !ok {
		httpx.Fail(w, httpx.Unauthorized("connector authentication required"))
		return
	}
	transferID := r.PathValue("transfer_id")
	token := r.URL.Query().Get("token")
	transfer, ok := fsTransfers.get(transferID, token)
	if !ok || transfer.ConnectorID != connector.ID {
		httpx.Fail(w, httpx.NotFound("transfer not found"))
		return
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, fsMaxPushBytes+1))
	if err != nil {
		if overBodyLimit(err) {
			httpx.Fail(w, httpx.NewError(http.StatusRequestEntityTooLarge, "transfer body is too large"))
			return
		}
		httpx.Fail(w, httpx.BadRequest("could not read the transfer body"))
		return
	}
	if int64(len(data)) > fsMaxPushBytes {
		httpx.Fail(w, httpx.NewError(http.StatusRequestEntityTooLarge, "transfer body is too large"))
		return
	}
	if !fsTransfers.setData(transferID, token, connector.ID, data) {
		httpx.Fail(w, httpx.NotFound("transfer not found"))
		return
	}
	httpx.OK(w, map[string]any{
		"status": "accepted", "transferId": transferID, "size": len(data),
		"serverTime": timestampNow(),
	})
}

// prepareFSDownload asks the connector for metadata and registers a
// short-lived transfer. Every size goes through the transfer URL: both shipped
// clients decode downloadUrl as a required field, and the browser preview
// needs a URL it can put in an <img> without an Authorization header.
func (a *App) prepareFSDownload(
	ctx context.Context, connectorID string, target fsResolved, previewAccess string,
) (map[string]any, error) {
	var meta fsRPCFile
	if err := a.callFSWithTimeout(ctx, connectorID, "fs.size", map[string]any{
		"root": target.Root, "path": target.Path,
	}, &meta, fsMaxSizeRPCSeconds); err != nil {
		return nil, err
	}
	result := map[string]any{
		"path":      meta.Path,
		"name":      meta.Name,
		"size":      meta.Size,
		"sha256":    meta.SHA256,
		"mediaType": meta.MediaType,
	}

	transfer := fsTransfers.create(fsTransfer{
		ID:          store.NewID("fsdl"),
		ConnectorID: connectorID,
		Root:        target.Root,
		Path:        target.Path,
		Name:        meta.Name,
		SHA256:      meta.SHA256,
		MediaType:   meta.MediaType,
		Size:        meta.Size,
	})
	result["transferId"] = transfer.ID
	result["token"] = transfer.Token
	result["downloadUrl"] = fsDownloadURL(connectorID, transfer.ID, transfer.Token, previewAccess)
	return result, nil
}

// readFSText returns the shared FsReadTextResponse shape. Both "text" (the Go
// API field) and "content" (what the shipped browser client reads) carry the
// same string so neither client needs a special case.
func (a *App) readFSText(
	ctx context.Context, connectorID string, target fsResolved, maxBytes int,
) (map[string]any, error) {
	var result fsRPCText
	if err := a.callFS(ctx, connectorID, "fs.readText", map[string]any{
		"root": target.Root, "path": target.Path, "maxBytes": maxBytes,
	}, &result); err != nil {
		return nil, err
	}
	if !utf8.ValidString(result.Text) {
		return nil, httpx.NewError(http.StatusUnprocessableEntity, "file is not valid UTF-8")
	}
	return map[string]any{
		"path":       result.Path,
		"name":       result.Name,
		"size":       result.Size,
		"sha256":     result.SHA256,
		"encoding":   "utf-8",
		"text":       result.Text,
		"content":    result.Text,
		"truncated":  result.Truncated,
		"binary":     false,
		"serverTime": timestampNow(),
	}, nil
}

func fsWriteContent(body fsReadBody) ([]byte, error) {
	switch {
	case body.ContentBase64 != nil:
		data, err := base64.StdEncoding.DecodeString(*body.ContentBase64)
		if err != nil {
			return nil, httpx.NewError(http.StatusUnprocessableEntity,
				"contentBase64 is not valid base64")
		}
		return data, nil
	case body.Content != nil:
		switch strings.ToLower(strings.TrimSpace(body.Encoding)) {
		case "", "utf8", "utf-8":
			return []byte(*body.Content), nil
		case "base64":
			data, err := base64.StdEncoding.DecodeString(*body.Content)
			if err != nil {
				return nil, httpx.NewError(http.StatusUnprocessableEntity,
					"content is not valid base64")
			}
			return data, nil
		default:
			return nil, httpx.NewError(http.StatusUnprocessableEntity,
				"only utf8 and base64 encodings are supported")
		}
	default:
		return nil, httpx.NewError(http.StatusUnprocessableEntity,
			"contentBase64 or content is required")
	}
}

func fsTextLimit(raw int) (int, error) {
	if raw == 0 {
		return fsDefaultTextBytes, nil
	}
	if raw < 1 || raw > fsMaxTextBytes {
		return 0, httpx.NewError(http.StatusUnprocessableEntity,
			fmt.Sprintf("maxBytes must be between 1 and %d", fsMaxTextBytes))
	}
	return raw, nil
}

// bodyRoot follows the existing fs/list precedence: query wins over body.
func bodyRoot(r *http.Request, fallback string) string {
	if root := strings.TrimSpace(r.URL.Query().Get("root")); root != "" {
		return root
	}
	return fallback
}

// resolveFSTarget validates root/path before any RPC is made.
//
// The server and the connector may run on different operating systems, so the
// check is lexical and treats both separators as path separators. ".." cannot
// climb above root, an absolute path must already sit under it, and a Windows
// drive or UNC host change fails the containment test. Symlinks are invisible
// to a lexical check; the connector repeats the check with EvalSymlinks.
func resolveFSTarget(rawRoot, rawPath string) (fsResolved, error) {
	root := strings.TrimSpace(rawRoot)
	if root == "" {
		return fsResolved{}, httpx.NewError(http.StatusUnprocessableEntity, "root is required")
	}
	if strings.TrimSpace(rawPath) == "" {
		return fsResolved{}, httpx.NewError(http.StatusUnprocessableEntity, "path is required")
	}
	if strings.IndexByte(root, 0) >= 0 || strings.IndexByte(rawPath, 0) >= 0 {
		return fsResolved{}, httpx.NewError(http.StatusUnprocessableEntity, "path contains a NUL byte")
	}

	rootPath := fsNormalize(root)
	filePath := fsNormalize(rawPath)
	if fsHasDrivePrefix(rootPath) && !fsIsAbsolute(rootPath) {
		return fsResolved{}, httpx.NewError(http.StatusUnprocessableEntity,
			"root must be an absolute drive path")
	}
	if fsHasDrivePrefix(filePath) && !fsIsAbsolute(filePath) {
		return fsResolved{}, httpx.NewError(http.StatusUnprocessableEntity,
			"drive-relative paths cannot be bounded to the connector root")
	}
	target := filePath
	if !fsIsAbsolute(filePath) {
		target = fsJoin(rootPath, filePath)
	}
	if !fsWithinRoot(rootPath, target) {
		return fsResolved{}, httpx.NewError(http.StatusUnprocessableEntity,
			"path escapes the connector root")
	}
	return fsResolved{Root: rootPath, Path: filePath}, nil
}

func fsNormalize(value string) string {
	value = strings.ReplaceAll(value, `\`, "/")
	unc := strings.HasPrefix(value, "//")
	cleaned := path.Clean(value)
	if unc && !strings.HasPrefix(cleaned, "//") {
		cleaned = "/" + cleaned
	}
	// path.Clean drops the trailing slash that distinguishes "C:/" from the
	// drive-relative "C:"; keep the root spelling intact.
	if len(cleaned) == 2 && fsHasDrivePrefix(cleaned) && strings.HasSuffix(value, "/") {
		cleaned += "/"
	}
	return cleaned
}

func fsHasDrivePrefix(value string) bool {
	if len(value) < 2 || value[1] != ':' {
		return false
	}
	return (value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')
}

func fsIsAbsolute(value string) bool {
	if value == "~" || strings.HasPrefix(value, "~/") {
		return true
	}
	if strings.HasPrefix(value, "/") {
		return true
	}
	return fsHasDrivePrefix(value) && len(value) >= 3 && value[2] == '/'
}

func fsJoin(root, relative string) string {
	if strings.HasPrefix(root, "//") {
		return "/" + path.Join(strings.TrimPrefix(root, "/"), relative)
	}
	return path.Join(root, relative)
}

func fsWithinRoot(root, target string) bool {
	if root == "." {
		if fsIsAbsolute(target) {
			return false
		}
		return target != ".." && !strings.HasPrefix(target, "../")
	}
	if fsWindowsStyle(root) {
		root = strings.ToLower(root)
		target = strings.ToLower(target)
	}
	root = strings.TrimSuffix(root, "/")
	if root == "" {
		root = "/"
	}
	if root == "/" {
		return strings.HasPrefix(target, "/")
	}
	return target == root || strings.HasPrefix(target, root+"/")
}

func fsWindowsStyle(value string) bool {
	return fsHasDrivePrefix(value) || strings.HasPrefix(value, "//")
}

// writeFSError maps connector RPC failures onto the statuses the file panel
// understands. Contract errors keep a string detail, like every other handler.
func (a *App) writeFSError(w http.ResponseWriter, err error) {
	var apiErr *httpx.Error
	if errors.As(err, &apiErr) {
		httpx.Fail(w, apiErr)
		return
	}
	var failure *rpcFailure
	switch {
	case errors.Is(err, ErrUnavailable):
		httpx.Fail(w, httpx.Conflict("connector is offline"))
	case errors.As(err, &failure):
		status := http.StatusBadGateway
		switch failure.code {
		case "not_found":
			status = http.StatusNotFound
		case "bad_params", "is_directory", "not_text":
			status = http.StatusUnprocessableEntity
		case "stale":
			status = http.StatusPreconditionFailed
		}
		httpx.Fail(w, httpx.NewError(status, failure.message))
	case errors.Is(err, context.DeadlineExceeded):
		httpx.Fail(w, httpx.NewError(http.StatusGatewayTimeout, "connector did not answer in time"))
	default:
		httpx.Fail(w, httpx.NewError(http.StatusGatewayTimeout, err.Error()))
	}
}

func (a *App) connectorOwnedBy(ctx context.Context, connectorID, userID string) (store.Connector, bool) {
	connector, err := a.db.ConnectorByID(ctx, connectorID)
	if err != nil || connector.UserID != userID || connector.Revoked != 0 {
		return store.Connector{}, false
	}
	return connector, true
}

// fsTransfer is one registered download relay.
type fsTransfer struct {
	ID          string
	Token       string
	ConnectorID string
	Root        string
	Path        string
	Name        string
	SHA256      string
	MediaType   string
	Size        int64
	ExpiresAt   time.Time
	pushed      []byte
	hasData     bool
}

type fsTransferRegistry struct {
	mu        sync.Mutex
	transfers map[string]*fsTransfer
}

var fsTransfers = &fsTransferRegistry{transfers: map[string]*fsTransfer{}}

func (r *fsTransferRegistry) create(transfer fsTransfer) fsTransfer {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	transfer.Token = NewToken("fst")
	transfer.ExpiresAt = now.Add(fsTransferTTL)
	stored := transfer
	r.transfers[transfer.ID] = &stored
	return transfer
}

func (r *fsTransferRegistry) get(id, token string) (fsTransfer, bool) {
	if id == "" || token == "" {
		return fsTransfer{}, false
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	stored, ok := r.transfers[id]
	if !ok || subtle.ConstantTimeCompare([]byte(stored.Token), []byte(token)) != 1 {
		return fsTransfer{}, false
	}
	snapshot := *stored
	if stored.hasData {
		snapshot.pushed = append([]byte(nil), stored.pushed...)
	}
	return snapshot, true
}

func (r *fsTransferRegistry) setData(id, token, connectorID string, data []byte) bool {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(now)
	stored, ok := r.transfers[id]
	if !ok || stored.ConnectorID != connectorID ||
		subtle.ConstantTimeCompare([]byte(stored.Token), []byte(token)) != 1 {
		return false
	}
	stored.pushed = append([]byte(nil), data...)
	stored.hasData = true
	return true
}

func (r *fsTransferRegistry) pruneLocked(now time.Time) {
	for id, transfer := range r.transfers {
		if now.After(transfer.ExpiresAt) {
			delete(r.transfers, id)
		}
	}
}

type fsPreviewUseRegistry struct {
	mu   sync.Mutex
	seen map[string]int64
}

var fsPreviewUses = &fsPreviewUseRegistry{seen: map[string]int64{}}

func (r *fsPreviewUseRegistry) consume(jti string, exp int64) bool {
	if jti == "" {
		return false
	}
	now := time.Now().Unix()
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, deadline := range r.seen {
		if deadline < now {
			delete(r.seen, key)
		}
	}
	if _, used := r.seen[jti]; used {
		return false
	}
	r.seen[jti] = exp
	return true
}

func fsDownloadURL(connectorID, transferID, token, previewAccess string) string {
	target := "/api/v2/connectors/" + url.PathEscape(connectorID) +
		"/fs/transfers/" + url.PathEscape(transferID) +
		"?token=" + url.QueryEscape(token)
	if previewAccess != "" {
		target += "&previewAccessToken=" + url.QueryEscape(previewAccess)
	}
	return target
}

func (a *App) serveFSTransfer(w http.ResponseWriter, r *http.Request, transfer fsTransfer) {
	name := transfer.Name
	if name == "" {
		name = "download"
	}
	mediaType := transfer.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}

	total := transfer.Size
	var pushed []byte
	if transfer.hasData {
		pushed = transfer.pushed
		total = int64(len(pushed))
	}
	if total < 0 {
		total = 0
	}
	start, length, ranged, unsatisfiable := fsParseRange(r.Header.Get("Range"), total)
	if unsatisfiable {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", total))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	// Read the first chunk before committing to a status line. Once the
	// headers are out, a connector-side failure (file removed, permission
	// denied) can no longer become an HTTP error, only a truncated body. A
	// later failure aborts the connection instead.
	var first []byte
	if length > 0 && !transfer.hasData {
		chunkLength := int64(fsTransferChunk)
		if length < chunkLength {
			chunkLength = length
		}
		chunk, err := a.readFSTransferChunk(r.Context(), transfer, start, chunkLength)
		if err != nil {
			a.writeFSError(w, err)
			return
		}
		if int64(len(chunk)) != chunkLength {
			a.writeFSError(w, httpx.NewError(http.StatusBadGateway,
				fmt.Sprintf("connector returned %d bytes for %s; expected %d",
					len(chunk), transfer.Path, chunkLength)))
			return
		}
		first = chunk
	}

	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", contentDisposition("attachment", name))
	w.Header().Set("X-File-Name", safeHeaderValue(name))
	w.Header().Set("X-File-Sha256", transfer.SHA256)
	w.Header().Set("X-File-Size", strconv.FormatInt(transfer.Size, 10))
	w.Header().Set("Accept-Ranges", "bytes")
	if ranged {
		w.Header().Set("Content-Range",
			fmt.Sprintf("bytes %d-%d/%d", start, start+length-1, total))
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusOK)
	}
	if length == 0 {
		return
	}
	if transfer.hasData {
		end := start + length
		if end > int64(len(pushed)) {
			end = int64(len(pushed))
		}
		_, _ = w.Write(pushed[start:end])
		return
	}
	if _, err := w.Write(first); err != nil {
		return
	}
	remaining := length - int64(len(first))
	if remaining > 0 {
		a.pullFSTransfer(r.Context(), w, transfer, start+int64(len(first)), remaining)
	}
}

// readFSTransferChunk asks the connector for one range of a transfer.
func (a *App) readFSTransferChunk(
	ctx context.Context, transfer fsTransfer, offset, length int64,
) ([]byte, error) {
	var chunk fsRPCRead
	if err := a.callFS(ctx, transfer.ConnectorID, "fs.read", map[string]any{
		"root": transfer.Root, "path": transfer.Path,
		"offset": offset, "length": length,
	}, &chunk); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(chunk.ContentBase64)
	if err != nil {
		return nil, httpx.NewError(http.StatusBadGateway, "connector returned invalid base64")
	}
	if int64(len(data)) > length {
		data = data[:length]
	}
	return data, nil
}

// pullFSTransfer asks the connector for consecutive chunks. The transfer was
// created for a file larger than the inline limit, so it is streamed rather
// than held in memory. Headers are already written at this point, so a failure
// aborts the connection instead of returning a successful-looking short body.
func (a *App) pullFSTransfer(ctx context.Context, w http.ResponseWriter, transfer fsTransfer, start, length int64) {
	flusher, _ := w.(http.Flusher)
	offset := start
	remaining := length
	for remaining > 0 {
		chunkLength := int64(fsTransferChunk)
		if remaining < chunkLength {
			chunkLength = remaining
		}
		data, err := a.readFSTransferChunk(ctx, transfer, offset, chunkLength)
		if err != nil {
			a.log.Warn("fs transfer read failed",
				"transfer", transfer.ID, "offset", offset, "error", err)
			panic(http.ErrAbortHandler)
		}
		if int64(len(data)) != chunkLength {
			a.log.Warn("fs transfer returned a short chunk",
				"transfer", transfer.ID, "offset", offset,
				"received", len(data), "expected", chunkLength)
			panic(http.ErrAbortHandler)
		}
		if _, err := w.Write(data); err != nil {
			return
		}
		offset += int64(len(data))
		remaining -= int64(len(data))
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// fsParseRange supports a single byte range. Multiple ranges are ignored and
// answered with the whole file, which RFC 9110 permits.
func fsParseRange(header string, total int64) (start, length int64, ranged, unsatisfiable bool) {
	trimmed := strings.TrimSpace(header)
	if trimmed == "" || len(trimmed) < 6 || !strings.EqualFold(trimmed[:6], "bytes=") {
		return 0, total, false, false
	}
	spec := strings.TrimSpace(trimmed[6:])
	if spec == "" || strings.Contains(spec, ",") {
		return 0, total, false, false
	}
	if total <= 0 {
		return 0, 0, false, true
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false, true
	}
	startRaw := strings.TrimSpace(spec[:dash])
	endRaw := strings.TrimSpace(spec[dash+1:])
	if startRaw == "" {
		suffix, err := strconv.ParseInt(endRaw, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, true
		}
		if suffix > total {
			suffix = total
		}
		return total - suffix, suffix, true, false
	}
	start, err := strconv.ParseInt(startRaw, 10, 64)
	if err != nil || start < 0 || start >= total {
		return 0, 0, false, true
	}
	end := total - 1
	if endRaw != "" {
		end, err = strconv.ParseInt(endRaw, 10, 64)
		if err != nil || end < start {
			return 0, 0, false, true
		}
		if end >= total {
			end = total - 1
		}
	}
	return start, end - start + 1, true, false
}

func fsString(payload map[string]any, key string) string {
	value, _ := payload[key].(string)
	return value
}

func fsExpiresAt(seconds int) string {
	return isoTime(time.Now().Add(time.Duration(seconds) * time.Second).UnixMilli())
}

func fsTokenExpiry(payload map[string]any) int64 {
	if value, ok := payload["exp"].(float64); ok {
		return int64(value)
	}
	return time.Now().Unix() + fsPreviewTokenTTL
}
