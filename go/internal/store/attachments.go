package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// AttachmentMeta is the sidecar record stored next to a blob.
//
// The sidecar, not the database row, is the source of truth for the download
// endpoints: an attachment has to remain readable even if the index row was
// lost, and a copied upload directory stays self-describing.
type AttachmentMeta struct {
	FileID    string `json:"fileId"`
	SessionID string `json:"sessionId"`
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	MediaType string `json:"mediaType"`
	Origin    string `json:"origin"`
	CreatedAt string `json:"createdAt"`
}

// ErrAttachmentCorrupt reports a blob whose bytes no longer match its sidecar.
// Callers surface it as a 500: the upload succeeded once, so this is storage
// damage, not a missing file.
var ErrAttachmentCorrupt = errors.New("stored file metadata does not match content")

// SaveAttachment writes data and its sidecar, filling Size and SHA256 from the
// bytes actually written. Both files go through a temporary file and a rename,
// so an interrupted request cannot leave a truncated blob behind.
func SaveAttachment(root string, meta *AttachmentMeta, data []byte) error {
	if !validAttachmentSegment(meta.SessionID) || !validFileID(meta.FileID) {
		return ErrNotFound
	}
	if meta.Name == "" {
		meta.Name = meta.FileID
	}
	if meta.Origin == "" {
		meta.Origin = "user"
	}
	if meta.CreatedAt == "" {
		meta.CreatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000000Z")
	}
	sum := sha256.Sum256(data)
	meta.Size = int64(len(data))
	meta.SHA256 = hex.EncodeToString(sum[:])

	dir := filepath.Join(root, meta.SessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create attachment directory: %w", err)
	}
	blobPath := filepath.Join(dir, meta.FileID+".bin")
	if err := writeFileAtomic(blobPath, data); err != nil {
		return fmt.Errorf("write attachment: %w", err)
	}
	sidecar, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		_ = os.Remove(blobPath)
		return fmt.Errorf("encode attachment metadata: %w", err)
	}
	sidecar = append(sidecar, '\n')
	if err := writeFileAtomic(filepath.Join(dir, meta.FileID+".json"), sidecar); err != nil {
		// A blob without a sidecar can never be served, so it must not linger.
		_ = os.Remove(blobPath)
		return fmt.Errorf("write attachment metadata: %w", err)
	}
	return nil
}

// ReadAttachmentMeta loads the sidecar without the blob, which is all the
// "open" redirect needs.
func ReadAttachmentMeta(root, sessionID, fileID string) (AttachmentMeta, error) {
	if !validAttachmentSegment(sessionID) || !validFileID(fileID) {
		return AttachmentMeta{}, ErrNotFound
	}
	raw, err := os.ReadFile(filepath.Join(root, sessionID, fileID+".json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AttachmentMeta{}, ErrNotFound
		}
		return AttachmentMeta{}, fmt.Errorf("read attachment metadata: %w", err)
	}
	var meta AttachmentMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return AttachmentMeta{}, fmt.Errorf("decode attachment metadata: %w", err)
	}
	// The sidecar must agree with the path it was found at; otherwise a copied
	// directory could be served under the wrong session.
	if meta.FileID != fileID || meta.SessionID != sessionID {
		return AttachmentMeta{}, ErrNotFound
	}
	return meta, nil
}

// ReadAttachment loads both files and checks them against each other, so a
// corrupted blob is reported instead of being handed to a client under a
// sha256 that no longer describes it.
func ReadAttachment(root, sessionID, fileID string) (AttachmentMeta, []byte, error) {
	meta, err := ReadAttachmentMeta(root, sessionID, fileID)
	if err != nil {
		return AttachmentMeta{}, nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, sessionID, fileID+".bin"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return AttachmentMeta{}, nil, ErrNotFound
		}
		return AttachmentMeta{}, nil, fmt.Errorf("read attachment: %w", err)
	}
	sum := sha256.Sum256(data)
	if int64(len(data)) != meta.Size || hex.EncodeToString(sum[:]) != meta.SHA256 {
		return AttachmentMeta{}, nil, ErrAttachmentCorrupt
	}
	return meta, data, nil
}

// DeleteAttachment removes a blob and its sidecar. It is the compensation path
// for a failed index insert, so a missing file is not an error.
func DeleteAttachment(root, sessionID, fileID string) error {
	if !validAttachmentSegment(sessionID) || !validFileID(fileID) {
		return nil
	}
	dir := filepath.Join(root, sessionID)
	for _, suffix := range []string{".bin", ".json"} {
		if err := os.Remove(filepath.Join(dir, fileID+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// InsertAttachment records the metadata index row for a blob already on disk.
// The caller deletes the files when this fails, because the two writes cannot
// be committed together.
func (s *Store) InsertAttachment(ctx context.Context, meta AttachmentMeta, createdAtMS int64) error {
	storageKey := meta.SessionID + "/" + meta.FileID + ".bin"
	return s.Write(ctx, func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO attachments
				(id, session_id, name, media_type, size, sha256, origin, storage_key, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			meta.FileID, meta.SessionID, meta.Name, meta.MediaType, meta.Size,
			meta.SHA256, meta.Origin, storageKey, createdAtMS)
		return err
	})
}

// AttachmentFileRef identifies one stored attachment by the two path segments
// its blob lives under. It is what callers need to delete the files after the
// session rows are gone.
type AttachmentFileRef struct {
	SessionID string
	FileID    string
}

// AttachmentFilesForConnector lists the attachments of every session on one
// connector. It must be called before those sessions are deleted, because the
// attachment rows cascade away with them.
func (s *Store) AttachmentFilesForConnector(ctx context.Context, q Querier, connectorID string) ([]AttachmentFileRef, error) {
	return attachmentFiles(ctx, q, `
		SELECT a.session_id, a.id FROM attachments a
		JOIN sessions s ON s.id = a.session_id
		WHERE s.connector_id = ?`, connectorID)
}

// AttachmentFilesForUser lists the attachments of every session a user owns.
func (s *Store) AttachmentFilesForUser(ctx context.Context, q Querier, userID string) ([]AttachmentFileRef, error) {
	return attachmentFiles(ctx, q, `
		SELECT a.session_id, a.id FROM attachments a
		JOIN sessions s ON s.id = a.session_id
		JOIN connectors c ON c.id = s.connector_id
		WHERE c.user_id = ?`, userID)
}

func attachmentFiles(ctx context.Context, q Querier, query string, arg any) ([]AttachmentFileRef, error) {
	rows, err := q.QueryContext(ctx, query, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AttachmentFileRef
	for rows.Next() {
		var ref AttachmentFileRef
		if err := rows.Scan(&ref.SessionID, &ref.FileID); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// writeFileAtomic stages data in the destination directory, then renames it
// into place. The rename is atomic on one filesystem, so readers never observe
// a partially written file.
func writeFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	fail := func(err error) error {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return err
	}
	if _, err := file.Write(data); err != nil {
		return fail(err)
	}
	// Match the mode a plain os.WriteFile would leave, so other processes in
	// the deployment can read the uploads.
	if err := file.Chmod(0o644); err != nil {
		return fail(err)
	}
	if err := file.Sync(); err != nil {
		return fail(err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return err
	}
	return nil
}

// validAttachmentSegment guards every value that becomes a path segment. The
// IDs are generated internally, but importers and a future API must not be able
// to escape the upload root.
func validAttachmentSegment(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	return !strings.ContainsAny(value, "/\\\x00")
}

// validFileID keeps the "file_" namespace the Python implementation used, so a
// stray path value cannot address a sidecar that is not an attachment.
func validFileID(fileID string) bool {
	return strings.HasPrefix(fileID, "file_") && validAttachmentSegment(fileID)
}
