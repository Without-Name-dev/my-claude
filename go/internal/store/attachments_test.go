package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAttachmentsMigrationCreatesTable(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "attachments.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	var count int
	if err := st.Read().QueryRowContext(ctx, `SELECT count(*) FROM attachments`).Scan(&count); err != nil {
		t.Fatalf("attachments table is not queryable: %v", err)
	}
}

func TestSaveAttachmentRecordsSizeAndSHA256(t *testing.T) {
	root := t.TempDir()
	meta := AttachmentMeta{
		FileID:    "file_abc123",
		SessionID: "ses_abc123",
		Name:      "notes.txt",
		MediaType: "text/plain",
	}
	data := []byte("hello attachment")
	if err := SaveAttachment(root, &meta, data); err != nil {
		t.Fatalf("SaveAttachment: %v", err)
	}

	sum := sha256.Sum256(data)
	if want := hex.EncodeToString(sum[:]); meta.SHA256 != want {
		t.Fatalf("sha256 = %q, want %q", meta.SHA256, want)
	}
	if meta.Size != int64(len(data)) {
		t.Fatalf("size = %d, want %d", meta.Size, len(data))
	}
	if meta.CreatedAt == "" {
		t.Fatal("createdAt was not filled in")
	}

	read, blob, err := ReadAttachment(root, meta.SessionID, meta.FileID)
	if err != nil {
		t.Fatalf("ReadAttachment: %v", err)
	}
	if string(blob) != string(data) {
		t.Fatalf("blob = %q, want %q", blob, data)
	}
	if read.SHA256 != meta.SHA256 || read.Size != meta.Size {
		t.Fatalf("read metadata = %+v, want sha256 %q size %d", read, meta.SHA256, meta.Size)
	}

	// The atomic write must leave exactly the blob and its sidecar behind.
	entries, err := os.ReadDir(filepath.Join(root, meta.SessionID))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 2 {
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Fatalf("stored files = %v, want the .bin and .json only", names)
	}
}

func TestReadAttachmentDetectsCorruptBlob(t *testing.T) {
	root := t.TempDir()
	meta := AttachmentMeta{FileID: "file_corrupt", SessionID: "ses_corrupt", Name: "x"}
	if err := SaveAttachment(root, &meta, []byte("original")); err != nil {
		t.Fatalf("SaveAttachment: %v", err)
	}
	blobPath := filepath.Join(root, meta.SessionID, meta.FileID+".bin")
	if err := os.WriteFile(blobPath, []byte("tampered"), 0o644); err != nil {
		t.Fatalf("tamper with blob: %v", err)
	}
	if _, _, err := ReadAttachment(root, meta.SessionID, meta.FileID); !errors.Is(err, ErrAttachmentCorrupt) {
		t.Fatalf("error = %v, want ErrAttachmentCorrupt", err)
	}
}

func TestReadAttachmentDetectsWrongSidecarSize(t *testing.T) {
	root := t.TempDir()
	sessionID, fileID := "ses_size", "file_size"
	dir := filepath.Join(root, sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data := []byte("abc")
	sum := sha256.Sum256(data)
	if err := os.WriteFile(filepath.Join(dir, fileID+".bin"), data, 0o644); err != nil {
		t.Fatalf("write blob: %v", err)
	}
	sidecar, err := json.Marshal(AttachmentMeta{
		FileID:    fileID,
		SessionID: sessionID,
		Name:      "abc",
		Size:      99, // deliberately not len(data)
		SHA256:    hex.EncodeToString(sum[:]),
		CreatedAt: "2026-10-24T12:00:00.000000Z",
	})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fileID+".json"), sidecar, 0o644); err != nil {
		t.Fatalf("write sidecar: %v", err)
	}
	if _, _, err := ReadAttachment(root, sessionID, fileID); !errors.Is(err, ErrAttachmentCorrupt) {
		t.Fatalf("error = %v, want ErrAttachmentCorrupt", err)
	}
}

func TestSaveAttachmentEmptyFile(t *testing.T) {
	root := t.TempDir()
	meta := AttachmentMeta{FileID: "file_empty", SessionID: "ses_empty"}
	if err := SaveAttachment(root, &meta, nil); err != nil {
		t.Fatalf("SaveAttachment: %v", err)
	}
	sum := sha256.Sum256(nil)
	if meta.Size != 0 {
		t.Fatalf("size = %d, want 0", meta.Size)
	}
	if want := hex.EncodeToString(sum[:]); meta.SHA256 != want {
		t.Fatalf("sha256 = %q, want %q", meta.SHA256, want)
	}
	if meta.Name != meta.FileID {
		t.Fatalf("name = %q, want the file id fallback %q", meta.Name, meta.FileID)
	}
}

func TestAttachmentPathsRejectTraversal(t *testing.T) {
	root := t.TempDir()
	cases := []struct{ sessionID, fileID string }{
		{"..", "file_a"},
		{"", "file_a"},
		{"ses_a", "../file_a"},
		{"ses_a", "file_a/../../x"},
		{"ses_a", "file_a\\..\\x"},
		{"ses_a", "not-a-file"},
	}
	for _, tc := range cases {
		if _, err := ReadAttachmentMeta(root, tc.sessionID, tc.fileID); !errors.Is(err, ErrNotFound) {
			t.Errorf("ReadAttachmentMeta(%q, %q) error = %v, want ErrNotFound",
				tc.sessionID, tc.fileID, err)
		}
	}
}
