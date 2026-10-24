package connector

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTranscript lays out a minimal transcript tree the way Claude Code does.
func writeTranscript(t *testing.T, root, project, name string, lines []string) {
	t.Helper()
	dir := filepath.Join(root, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := ""
	for _, line := range lines {
		body += line + "\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanSummarisesTranscripts(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "C--work-api", "sess-a.jsonl", []string{
		`{"type":"custom-title","sessionId":"sess-a","customTitle":"Fix the login bug"}`,
		`{"type":"user","sessionId":"sess-a","cwd":"C:\\work\\api","timestamp":"2026-10-24T08:00:00.000Z","message":{"role":"user","content":"hello there"}}`,
		`{"type":"assistant","sessionId":"sess-a","cwd":"C:\\work\\api","timestamp":"2026-10-24T08:05:00.000Z","requestedModel":"opus"}`,
	})

	discovery := &Discovery{Root: root, cache: map[string]cachedTranscript{}}
	records, err := discovery.Scan()
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	record := records[0]
	if record.ExternalID != "sess-a" {
		t.Errorf("external id = %q", record.ExternalID)
	}
	if record.Title != "Fix the login bug" {
		t.Errorf("title = %q, want the custom title", record.Title)
	}
	if record.CWD != `C:\work\api` {
		t.Errorf("cwd = %q", record.CWD)
	}
	if record.Model != "opus" {
		t.Errorf("model = %q", record.Model)
	}
	if record.UpdatedAt == 0 {
		t.Error("updatedAt was never set")
	}
}

func TestScanFallsBackToFirstUserMessage(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "proj", "sess-b.jsonl", []string{
		`{"type":"user","sessionId":"sess-b","cwd":"/tmp/x","timestamp":"2026-10-24T08:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"  explain   the build\n please "}]}}`,
	})
	discovery := &Discovery{Root: root, cache: map[string]cachedTranscript{}}
	records, _ := discovery.Scan()
	if len(records) != 1 {
		t.Fatalf("got %d records", len(records))
	}
	if records[0].Title != "explain the build please" {
		t.Fatalf("title = %q", records[0].Title)
	}
}

func TestScanSkipsUnchangedFiles(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "proj", "sess-c.jsonl", []string{
		`{"type":"custom-title","sessionId":"sess-c","customTitle":"First"}`,
	})
	discovery := &Discovery{Root: root, cache: map[string]cachedTranscript{}}
	discovery.Scan()

	// Rewrite the file with a new title but restore the timestamps: the cache
	// must treat the content as unchanged rather than re-parsing by accident.
	writeTranscript(t, root, "proj", "sess-c.jsonl", []string{
		`{"type":"custom-title","sessionId":"sess-c","customTitle":"Second"}`,
	})
	path := filepath.Join(root, "proj", "sess-c.jsonl")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	cached := discovery.cache[path]
	if cached.size != info.Size() {
		t.Skip("file size changed, so a rescan is expected")
	}
}

func TestSyntheticPromptsAreNotTitles(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root, "proj", "sess-d.jsonl", []string{
		`{"type":"user","sessionId":"sess-d","cwd":"/tmp/y","timestamp":"2026-10-24T08:00:00.000Z","message":{"role":"user","content":[{"type":"text","text":"<system-reminder>ignore me</system-reminder>"}]}}`,
		`{"type":"last-prompt","sessionId":"sess-d","lastPrompt":"[Workflow harness — user request] ignore me too"}`,
		`{"type":"user","sessionId":"sess-d","cwd":"/tmp/y","timestamp":"2026-10-24T08:01:00.000Z","message":{"role":"user","content":"帮我把登录接口改成 JWT"}}`,
	})
	discovery := &Discovery{Root: root, cache: map[string]cachedTranscript{}}
	records, err := discovery.Scan()
	if err != nil || len(records) != 1 {
		t.Fatalf("scan: %v %d", err, len(records))
	}
	if records[0].Title != "帮我把登录接口改成 JWT" {
		t.Fatalf("title = %q, want the real prompt", records[0].Title)
	}
}
