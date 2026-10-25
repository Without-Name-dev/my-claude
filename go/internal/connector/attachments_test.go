package connector

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agents-anywhere/agents-anywhere/internal/bridge"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
)

// attachmentServer serves one file on the connector route and records the
// bearer it was fetched with.
func attachmentServer(t *testing.T, body []byte) (*httptest.Server, *string) {
	t.Helper()
	seen := new(string)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v2/connector/sessions/ses_1/attachments/file_1/content" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)
	return server, seen
}

func newAttachmentClient(t *testing.T, serverURL string) *Client {
	t.Helper()
	client := NewClient("ws://unused/api/v2/connector/ws", "connector-token", "test",
		bridge.New(t.TempDir()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	client.ServerURL = serverURL
	return client
}

func TestMaterializeAttachmentsWritesTheFileAndVerifiesItsChecksum(t *testing.T) {
	body := []byte("\x89PNG\r\n\x1a\n fake image")
	sum := sha256.Sum256(body)
	server, seen := attachmentServer(t, body)
	client := newAttachmentClient(t, server.URL)

	files, failures := client.materializeAttachments(context.Background(), protocol.Submit{
		SessionID:         "ses_1",
		ExternalSessionID: "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6",
		Attachments: []protocol.SubmitAttachment{{
			FileID:      "file_1",
			Name:        "shot.png",
			MediaType:   "image/png",
			Size:        int64(len(body)),
			SHA256:      hex.EncodeToString(sum[:]),
			DownloadURL: "/api/v2/connector/sessions/ses_1/attachments/file_1/content",
		}},
	})
	if len(failures) != 0 {
		t.Fatalf("failures = %v, want none", failures)
	}
	if len(files) != 1 {
		t.Fatalf("files = %+v, want one", files)
	}
	if *seen != "Bearer connector-token" {
		t.Fatalf("authorization = %q, want the connector's own token", *seen)
	}
	stored, err := os.ReadFile(files[0].Path)
	if err != nil {
		t.Fatalf("read the downloaded file: %v", err)
	}
	if string(stored) != string(body) {
		t.Fatalf("stored %q, want the served bytes", stored)
	}
	if filepath.Base(files[0].Path) != "file_1-shot.png" {
		t.Fatalf("file name = %q, want the id and display name", filepath.Base(files[0].Path))
	}
	dir := filepath.Dir(files[0].Path)
	if filepath.Base(dir) != "6f1f0d9c-9a4b-4d15-8c39-a1b2c3d4e5f6" {
		t.Fatalf("directory = %q, want the runtime session id", dir)
	}
}

func TestMaterializeAttachmentsReportsAChecksumMismatchInsteadOfWriting(t *testing.T) {
	server, _ := attachmentServer(t, []byte("different bytes"))
	client := newAttachmentClient(t, server.URL)

	files, failures := client.materializeAttachments(context.Background(), protocol.Submit{
		SessionID: "ses_1",
		Attachments: []protocol.SubmitAttachment{{
			FileID:      "file_1",
			Name:        "shot.png",
			SHA256:      strings.Repeat("0", 64),
			DownloadURL: "/api/v2/connector/sessions/ses_1/attachments/file_1/content",
		}},
	})
	if len(files) != 0 {
		t.Fatalf("files = %+v, want none", files)
	}
	if len(failures) != 1 || !strings.Contains(failures[0], "checksum") {
		t.Fatalf("failures = %v, want one checksum failure", failures)
	}
}

func TestMaterializeAttachmentsRefusesAForeignURL(t *testing.T) {
	client := newAttachmentClient(t, "http://example.test")
	files, failures := client.materializeAttachments(context.Background(), protocol.Submit{
		Attachments: []protocol.SubmitAttachment{{
			FileID:      "file_1",
			Name:        "shot.png",
			DownloadURL: "http://elsewhere.test/steal",
		}},
	})
	if len(files) != 0 || len(failures) != 1 {
		t.Fatalf("files = %v, failures = %v, want one refusal", files, failures)
	}
}

func TestPromptWithAttachmentsNamesEveryLocalPath(t *testing.T) {
	text := promptWithAttachments("看这张图", []localAttachment{
		{Path: "/home/me/.claude/phone-bridge/attachments/s1/file_1-shot.png", MediaType: "image/png"},
	}, nil)
	if !strings.Contains(text, "Read tool") {
		t.Fatalf("prompt = %q, want the Read tool instruction", text)
	}
	if !strings.Contains(text, "/home/me/.claude/phone-bridge/attachments/s1/file_1-shot.png") {
		t.Fatalf("prompt = %q, want the local path", text)
	}
	if !strings.HasSuffix(text, "看这张图") {
		t.Fatalf("prompt = %q, want the person's words last", text)
	}
}

func TestPromptWithAttachmentsSurvivesAnEmptyMessage(t *testing.T) {
	text := promptWithAttachments("", []localAttachment{{Path: "/tmp/a.png"}}, nil)
	if strings.TrimSpace(text) == "" {
		t.Fatal("an image-only message produced empty prompt text, which the bridge rejects")
	}
}

func TestHTTPBaseTurnsTheWebsocketURLIntoAnOrigin(t *testing.T) {
	cases := map[string]string{
		"wss://example.test/api/v2/connector/ws":  "https://example.test",
		"ws://127.0.0.1:8080/api/v2/connector/ws": "http://127.0.0.1:8080",
		"http://example.test/":                    "http://example.test",
	}
	for input, want := range cases {
		client := &Client{URL: input}
		if got := client.httpBase(); got != want {
			t.Fatalf("httpBase(%q) = %q, want %q", input, got, want)
		}
	}
}
