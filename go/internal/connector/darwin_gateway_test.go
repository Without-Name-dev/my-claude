//go:build darwin

package connector

import (
	"strings"
	"testing"
)

func TestDarwinCommandLineCarriesGateway(t *testing.T) {
	SetGatewayStateDir(t.TempDir())
	_ = ApplyGatewayConfig(nil)
	defer ApplyGatewayConfig(nil)

	plain := darwinSessionCommandLine("/tmp/project", "sess-1", true)
	if strings.Contains(plain, "ANTHROPIC_BASE_URL") {
		t.Fatalf("gateway variables leaked into an unwired command line: %s", plain)
	}
	if !strings.Contains(plain, "claude --resume 'sess-1'") {
		t.Fatalf("resume flag missing: %s", plain)
	}

	_ = ApplyGatewayConfig(&GatewayConfig{BaseURL: "https://gw.example", Token: "tok'quote"})
	wired := darwinSessionCommandLine("/tmp/project", "sess-1", false)
	for _, want := range []string{
		"unset CLAUDE_CODE_CHILD_SESSION",
		"ANTHROPIC_BASE_URL",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_USE_BEDROCK",
		"env ",
		"claude --session-id 'sess-1'",
	} {
		if !strings.Contains(wired, want) {
			t.Fatalf("missing %q in %s", want, wired)
		}
	}
	// A token containing a single quote must stay one shell word.
	if !strings.Contains(wired, `'\''`) {
		t.Fatalf("token was not shell-quoted: %s", wired)
	}
}
