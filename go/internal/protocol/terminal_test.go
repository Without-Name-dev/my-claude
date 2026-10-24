package protocol

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestTerminalCommandJSONShape(t *testing.T) {
	raw, err := json.Marshal(TerminalCommand{
		Op:         TerminalOpOpen,
		TerminalID: "trm_abc",
		SessionID:  "browse_con_1",
		CWD:        `C:\work`,
		Shell:      "powershell.exe",
		Cols:       120,
		Rows:       40,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded["op"] != TerminalOpOpen {
		t.Fatalf("op = %v", decoded["op"])
	}
	if decoded["terminalId"] != "trm_abc" {
		t.Fatalf("terminalId = %v", decoded["terminalId"])
	}
	if decoded["cols"] != float64(120) || decoded["rows"] != float64(40) {
		t.Fatalf("size = %v x %v", decoded["cols"], decoded["rows"])
	}
	if _, present := decoded["data"]; present {
		t.Fatalf("empty data should be omitted: %s", raw)
	}

	var roundTrip TerminalCommand
	if err := json.Unmarshal(raw, &roundTrip); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if roundTrip.CWD != `C:\work` || roundTrip.Shell != "powershell.exe" {
		t.Fatalf("round trip lost fields: %+v", roundTrip)
	}
}

func TestTerminalOutputRoundTrip(t *testing.T) {
	payload := []byte("hello\r\n\x1b[32mgreen\x1b[0m")
	message := TerminalOutput{
		TerminalID: "trm_1",
		Seq:        1 << 40, // larger than a 32-bit counter
		Data:       base64.StdEncoding.EncodeToString(payload),
	}
	raw, err := json.Marshal(message)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded TerminalOutput
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Seq != message.Seq {
		t.Fatalf("seq = %d, want %d", decoded.Seq, message.Seq)
	}
	bytes, err := base64.StdEncoding.DecodeString(decoded.Data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(bytes) != string(payload) {
		t.Fatalf("payload = %q", bytes)
	}
}

func TestTerminalExitKeepsNullCode(t *testing.T) {
	raw, err := json.Marshal(TerminalExit{TerminalID: "trm_1", Reason: "closed"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"exitCode":null`) {
		t.Fatalf("a nil exit code must be serialized as null: %s", raw)
	}

	code := 7
	raw, err = json.Marshal(TerminalExit{TerminalID: "trm_1", ExitCode: &code, Reason: "exit"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"exitCode":7`) {
		t.Fatalf("exit code missing: %s", raw)
	}
}

func TestTerminalFrameTypesAreDistinct(t *testing.T) {
	types := map[string]bool{
		TypeTerminal:       true,
		TypeTerminalOpened: true,
		TypeTerminalOutput: true,
		TypeTerminalExit:   true,
		TypeTerminalError:  true,
	}
	if len(types) != 5 {
		t.Fatalf("frame type constants collide: %v", types)
	}
}
