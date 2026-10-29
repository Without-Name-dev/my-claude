package connector

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadHistoryWhileTranscriptIsChanging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.jsonl")
	transcript := `{"type":"user","uuid":"u1","message":{"role":"user","content":"first"}}
{"type":"assistant","uuid":"a1","message":{"id":"m1","role":"assistant","content":[{"type":"text","text":"completed"}]}}
{"type":"user","uuid":"u2","message":{"role":"user","content":"second"}}
{"type":"assistant","uuid":"a2","message":{"id":"m2","role":"assistant","content":[{"type":"text","text":"still writing"}]}}
{"type":"assistant","uuid":"incomplete"`
	if err := os.WriteFile(path, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}

	settled, err := readHistory(path, 80, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(settled) != 3 || settled[0].Text != "first" || settled[1].Text != "completed" || settled[2].Text != "second" {
		t.Fatalf("changing transcript should expose complete records but hold the last assistant: %#v", settled)
	}
	full, err := ReadHistory(path, 80)
	if err != nil {
		t.Fatal(err)
	}
	if len(full) != 4 || full[3].Text != "still writing" || full[3].ID != "hist_a2" {
		t.Fatalf("settled pass should expose the final assistant exactly once: %#v", full)
	}
}
