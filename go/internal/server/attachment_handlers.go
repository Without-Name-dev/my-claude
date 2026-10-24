package server

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/agents-anywhere/agents-anywhere/internal/auth"
	"github.com/agents-anywhere/agents-anywhere/internal/httpx"
	"github.com/agents-anywhere/agents-anywhere/internal/store"
)

const (
	attachmentTokenKind = "local_file"
	attachmentTokenTTL  = 300
	maxAttachmentFiles  = 5
	maxAttachmentBytes  = 25 << 20
	// maxMultipartSlack covers boundaries, part headers and form fields that
	// are not "files", so the body cap only fires on actual abuse.
	maxMultipartSlack = 1 << 20
)

// UploadedAttachment is one file in an upload response.
type UploadedAttachment struct {
	FileID      string `json:"fileId"`
	SessionID   string `json:"sessionId"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	SHA256      string `json:"sha256"`
	MediaType   string `json:"mediaType"`
	CreatedAt   string `json:"createdAt"`
	DownloadURL string `json:"downloadUrl"`
	OpenURL     string `json:"openUrl"`
}

// registerAttachmentRoutes installs the session attachment endpoints.
//
// The download and local-download routes share one pattern because Go's
// ServeMux rejects "/sessions/local/{session_id}/{file_id}" next to
// "/sessions/{session_id}/attachments/{file_id}": the URL
// "/sessions/local/attachments/x" matches both and neither is more specific.
// Dispatching on the literal "attachments" segment keeps both public URL
// shapes, and mirrors the Python router where the attachment route is matched
// first.
func (a *App) registerAttachmentRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/v2/sessions/{session_id}/attachments",
		a.requireUser(a.handleUploadAttachments))
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/attachments/{file_id}/open",
		a.requireAttachmentViewer(a.handleOpenAttachment))

	download := a.requireUser(a.handleDownloadAttachment)
	mux.HandleFunc("GET /api/v2/sessions/{session_id}/{kind}/{file_id}",
		func(w http.ResponseWriter, r *http.Request) {
			if r.PathValue("kind") == "attachments" {
				download(w, r)
				return
			}
			if r.PathValue("session_id") == "local" {
				a.handleLocalAttachment(w, r, r.PathValue("kind"), r.PathValue("file_id"))
				return
			}
			// This pattern is a catch-all for /sessions/{id}/{kind}/{file};
			// anything else that reaches it is a route that does not exist, not
			// a missing file.
			httpx.JSON(w, http.StatusNotFound, map[string]string{"detail": "not found"})
		})
}

// requireAttachmentViewer accepts the bearer header or ?token= because the
// "open" URL is routinely loaded by <img> and <iframe>, which cannot set
// headers. The token is a user access token, never a file token.
func (a *App) requireAttachmentViewer(
	next func(http.ResponseWriter, *http.Request, store.User),
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, ok := a.userFromRequest(r)
		if !ok {
			if raw := r.URL.Query().Get("token"); raw != "" {
				if userID, err := auth.VerifyUserAccessToken(raw); err == nil {
					if candidate, err := a.db.UserByID(r.Context(), userID); err == nil && !candidate.Disabled {
						user, ok = candidate, true
					}
				}
			}
		}
		if !ok {
			httpx.Fail(w, httpx.Unauthorized("sign in required"))
			return
		}
		next(w, r, user)
	}
}

// handleUploadAttachments stages multipart "files" fields on disk for the
// connector to pick up. Every file is read before the first one is written, so
// a rejected request cannot leave a partial set behind.
func (a *App) handleUploadAttachments(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	// Cap the whole body as well as each part: without this, extra form fields
	// could stream unbounded data past the per-file limit.
	r.Body = http.MaxBytesReader(w, r.Body,
		int64(maxAttachmentFiles)*maxAttachmentBytes+maxMultipartSlack)
	reader, err := r.MultipartReader()
	if err != nil {
		httpx.Fail(w, httpx.BadRequest("multipart/form-data body is required"))
		return
	}

	type upload struct {
		name      string
		mediaType string
		data      []byte
	}
	uploads := make([]upload, 0, maxAttachmentFiles)
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			if overBodyLimit(err) {
				httpx.Fail(w, httpx.NewError(http.StatusRequestEntityTooLarge, "request body is too large"))
				return
			}
			httpx.Fail(w, httpx.BadRequest("invalid multipart body"))
			return
		}
		if part.FormName() != "files" {
			continue
		}
		if len(uploads) >= maxAttachmentFiles {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
				fmt.Sprintf("at most %d files per request", maxAttachmentFiles)))
			return
		}
		// One byte over the limit is enough to distinguish "at the limit" from
		// "too large" without buffering the rest of the part.
		data, err := io.ReadAll(io.LimitReader(part, maxAttachmentBytes+1))
		if err != nil {
			if overBodyLimit(err) {
				httpx.Fail(w, httpx.NewError(http.StatusRequestEntityTooLarge, "request body is too large"))
				return
			}
			httpx.Fail(w, httpx.BadRequest("could not read the uploaded file"))
			return
		}
		name := part.FileName()
		if name == "" {
			name = "attachment"
		}
		if len(data) == 0 {
			httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity,
				fmt.Sprintf("file %q is empty", name)))
			return
		}
		if int64(len(data)) > maxAttachmentBytes {
			httpx.Fail(w, httpx.NewError(http.StatusRequestEntityTooLarge,
				fmt.Sprintf("file %q exceeds %d bytes", name, maxAttachmentBytes)))
			return
		}
		mediaType := strings.TrimSpace(part.Header.Get("Content-Type"))
		if mediaType == "" {
			mediaType = mediaTypeForName(name)
		}
		uploads = append(uploads, upload{name: name, mediaType: mediaType, data: data})
	}
	if len(uploads) == 0 {
		httpx.Fail(w, httpx.NewError(http.StatusUnprocessableEntity, "no files were uploaded"))
		return
	}

	results := make([]UploadedAttachment, 0, len(uploads))
	for _, file := range uploads {
		nowMS := store.NowMS()
		meta := store.AttachmentMeta{
			FileID:    store.NewID("file"),
			SessionID: session.ID,
			Name:      file.name,
			MediaType: file.mediaType,
			Origin:    "user",
			CreatedAt: isoTime(nowMS),
		}
		if err := store.SaveAttachment(a.cfg.UploadDir, &meta, file.data); err != nil {
			a.log.Error("could not store an attachment", "error", err, "session", session.ID)
			httpx.Fail(w, err)
			return
		}
		if err := a.db.InsertAttachment(r.Context(), meta, nowMS); err != nil {
			// The blob is already on disk; without the index row nothing will
			// ever find it, so remove it rather than leak the file.
			_ = store.DeleteAttachment(a.cfg.UploadDir, session.ID, meta.FileID)
			httpx.Fail(w, err)
			return
		}
		downloadURL := "/api/v2/sessions/" + url.PathEscape(session.ID) +
			"/attachments/" + url.PathEscape(meta.FileID)
		results = append(results, UploadedAttachment{
			FileID:      meta.FileID,
			SessionID:   session.ID,
			Name:        meta.Name,
			Size:        meta.Size,
			SHA256:      meta.SHA256,
			MediaType:   meta.MediaType,
			CreatedAt:   meta.CreatedAt,
			DownloadURL: downloadURL,
			OpenURL:     downloadURL + "/open",
		})
	}
	httpx.OK(w, map[string]any{"attachments": results, "serverTime": timestampNow()})
}

// handleDownloadAttachment returns the blob inline as base64 for clients that
// cannot follow the redirect.
func (a *App) handleDownloadAttachment(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	meta, data, err := store.ReadAttachment(a.cfg.UploadDir, session.ID, r.PathValue("file_id"))
	if err != nil {
		writeAttachmentError(w, err)
		return
	}
	httpx.OK(w, map[string]any{
		"fileId":        meta.FileID,
		"sessionId":     meta.SessionID,
		"path":          meta.Path,
		"name":          meta.Name,
		"size":          meta.Size,
		"sha256":        meta.SHA256,
		"contentBase64": base64.StdEncoding.EncodeToString(data),
		"createdAt":     meta.CreatedAt,
		"serverTime":    timestampNow(),
	})
}

// handleOpenAttachment redirects to a short-lived signed URL, so an <img> tag
// can fetch the bytes without carrying the user's bearer.
func (a *App) handleOpenAttachment(w http.ResponseWriter, r *http.Request, user store.User) {
	session, ok := a.ownedSession(w, r, user)
	if !ok {
		return
	}
	fileID := r.PathValue("file_id")
	if _, err := store.ReadAttachmentMeta(a.cfg.UploadDir, session.ID, fileID); err != nil {
		writeAttachmentError(w, err)
		return
	}
	token, err := auth.CreateSignedToken(attachmentTokenKind, map[string]any{
		"sessionId": session.ID,
		"fileId":    fileID,
	}, attachmentTokenTTL)
	if err != nil {
		httpx.Fail(w, err)
		return
	}
	target := "/api/v2/sessions/local/" + url.PathEscape(session.ID) + "/" +
		url.PathEscape(fileID) + "?token=" + url.QueryEscape(token)
	w.Header().Set("Location", target)
	w.WriteHeader(http.StatusFound)
}

// handleLocalAttachment serves the blob behind a signed token. The token binds
// both ids, so a token minted for one file cannot read another.
func (a *App) handleLocalAttachment(w http.ResponseWriter, r *http.Request, sessionID, fileID string) {
	payload, err := auth.VerifySignedToken(attachmentTokenKind, r.URL.Query().Get("token"))
	if err != nil || payload["sessionId"] != sessionID || payload["fileId"] != fileID {
		httpx.Fail(w, httpx.Unauthorized("invalid file token"))
		return
	}
	meta, data, err := store.ReadAttachment(a.cfg.UploadDir, sessionID, fileID)
	if err != nil {
		writeAttachmentError(w, err)
		return
	}
	mediaType := meta.MediaType
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	// Anything that can run script is served as a download, never inline: the
	// app and the file share an origin, so an inline HTML or SVG attachment
	// would execute with the viewer's bearer in reach.
	disposition := "attachment"
	if inlineSafeType(mediaType) {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Disposition", contentDisposition(disposition, meta.Name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if disposition == "attachment" {
		w.Header().Set("Content-Security-Policy", "sandbox; default-src 'none'")
	}
	w.Header().Set("X-File-Name", safeHeaderValue(meta.Name))
	w.Header().Set("X-File-Sha256", meta.SHA256)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

// writeAttachmentError maps storage failures onto the API's error shape.
func writeAttachmentError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		httpx.Fail(w, httpx.NotFound("file not found"))
	case errors.Is(err, store.ErrAttachmentCorrupt):
		httpx.Fail(w, httpx.NewError(http.StatusInternalServerError, err.Error()))
	default:
		httpx.Fail(w, err)
	}
}

func overBodyLimit(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// mediaTypeForName mirrors Python's mimetypes.guess_type fallback. Go returns
// parameters for some extensions ("text/plain; charset=utf-8"), so only the
// base type is kept for a stable header value.
func mediaTypeForName(name string) string {
	guessed := mime.TypeByExtension(filepath.Ext(name))
	if guessed == "" {
		return ""
	}
	if parsed, _, err := mime.ParseMediaType(guessed); err == nil {
		return parsed
	}
	return guessed
}

// contentDisposition carries the name twice: an ASCII fallback for old clients
// and the RFC 5987 UTF-8 form, so non-ASCII names survive intact.
func contentDisposition(disposition, name string) string {
	fallback := strings.ReplaceAll(asciiFilename(name), `\`, `\\`)
	fallback = strings.ReplaceAll(fallback, `"`, `\"`)
	return disposition + `; filename="` + fallback + `"; filename*=UTF-8''` + rfc5987Encode(name)
}

// rfc5987Encode percent-encodes every byte outside the attr-char set. Go's
// url.PathEscape is more permissive (it leaves "=", "@" and ":" alone), which
// RFC 5987 does not allow in a filename* value.
func rfc5987Encode(value string) string {
	const upperHex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') ||
			c == '!' || c == '#' || c == '$' || c == '&' || c == '+' || c == '-' ||
			c == '.' || c == '^' || c == '_' || c == '`' || c == '|' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(upperHex[c>>4])
		b.WriteByte(upperHex[c&0x0f])
	}
	return b.String()
}

func asciiFilename(name string) string {
	var b strings.Builder
	for _, r := range name {
		if r < 0x80 {
			b.WriteRune(r)
		}
	}
	if trimmed := strings.TrimSpace(b.String()); trimmed != "" {
		return trimmed
	}
	return "attachment"
}

// safeHeaderValue keeps the X-File-Name header on one line and in Latin-1,
// matching the Python implementation; the exact name is still available
// percent-encoded in Content-Disposition.
func safeHeaderValue(value string) string {
	var b strings.Builder
	for _, r := range value {
		if r == '\t' || (r >= 0x20 && r != 0x7f && r <= 0xff) {
			b.WriteByte(byte(r))
			continue
		}
		b.WriteByte('?')
	}
	return b.String()
}

// inlineSafeType reports whether a browser may render the type in place.
//
// The list is deliberately narrow: raster images only. SVG is XML that can
// carry script, and every text/* type can be sniffed into HTML.
func inlineSafeType(mediaType string) bool {
	base := mediaType
	if index := strings.IndexByte(base, ';'); index >= 0 {
		base = base[:index]
	}
	switch strings.ToLower(strings.TrimSpace(base)) {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/avif", "image/bmp":
		return true
	default:
		return false
	}
}
