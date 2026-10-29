package connector

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func withGatewayDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	SetGatewayStateDir(dir)
	ApplyGatewayConfig(nil) // start from a clean slate
	t.Cleanup(func() { ApplyGatewayConfig(nil) })
}

func TestGatewayConfigRoundTrip(t *testing.T) {
	withGatewayDir(t)
	if env := gatewayEnvironment(); len(env) != 0 {
		t.Fatalf("expected no gateway env before configuration, got %v", env)
	}
	cfg := &GatewayConfig{BaseURL: "https://server.example/api/v2/gateway", Token: "gwc_secret"}
	if err := ApplyGatewayConfig(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	env := gatewayEnvironment()
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "ANTHROPIC_BASE_URL=https://server.example/api/v2/gateway") {
		t.Fatalf("base url missing from spawn env: %v", env)
	}
	if !strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN=gwc_secret") {
		t.Fatalf("token missing from spawn env: %v", env)
	}
	// The stored file must exist where the loader looks for it. Owner-only
	// permissions are a Unix property; Windows ignores the permission bits.
	path := gatewayConfigPath()
	info, statErr := os.Stat(path)
	if statErr != nil {
		t.Fatalf("gateway config missing: %v", statErr)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("gateway config is not owner-only: %v", info.Mode().Perm())
	}
	// A restart loads the stored wiring without wiping it.
	gatewayEnv.Store(nil)
	LoadStoredGatewayConfig()
	if len(gatewayEnvironment()) != 2 {
		t.Fatal("stored config was not loaded at startup")
	}
	// Clearing removes both the file and the env additions.
	if err := ApplyGatewayConfig(nil); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("gateway config file survived a clear")
	}
	if len(gatewayEnvironment()) != 0 {
		t.Fatal("gateway env survived a clear")
	}
}

func TestChildEnvironmentGatewayVars(t *testing.T) {
	withGatewayDir(t)
	t.Setenv("ANTHROPIC_BASE_URL", "https://own.example")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "own-token")
	t.Setenv("ANTHROPIC_API_KEY", "own-key")
	// Without the gateway the person's own Anthropic configuration is left
	// alone: enabling the gateway must not silently rewrite their setup.
	joined := strings.Join(childEnvironment(), "\n")
	if !strings.Contains(joined, "ANTHROPIC_BASE_URL=https://own.example") ||
		!strings.Contains(joined, "ANTHROPIC_AUTH_TOKEN=own-token") ||
		!strings.Contains(joined, "ANTHROPIC_API_KEY=own-key") {
		t.Fatal("gateway-off child env dropped the user's own Anthropic variables")
	}
	// With wiring installed, the stale values are replaced by the gateway's
	// and the inherited key is removed, exactly once each.
	_ = ApplyGatewayConfig(&GatewayConfig{BaseURL: "https://gw.example", Token: "tok"})
	child := childEnvironment()
	joined = strings.Join(child, "\n")
	if strings.Contains(joined, "https://own.example") ||
		strings.Contains(joined, "own-token") || strings.Contains(joined, "own-key=") {
		t.Fatalf("stale Anthropic variables survived gateway wiring: %s", joined)
	}
	var base, token int
	for _, entry := range childEnvironment() {
		if entry == "ANTHROPIC_BASE_URL=https://gw.example" {
			base++
		}
		if entry == "ANTHROPIC_AUTH_TOKEN=tok" {
			token++
		}
	}
	if base != 1 || token != 1 {
		t.Fatalf("gateway env not injected exactly once: base=%d token=%d", base, token)
	}
}

// ensure the state dir helper is exercised with a file path, not a dir.
func TestGatewayConfigPathEmptyDir(t *testing.T) {
	SetGatewayStateDir("")
	if p := gatewayConfigPath(); p != "" {
		t.Fatalf("expected empty path, got %q", p)
	}
	SetGatewayStateDir(filepath.Join(t.TempDir(), "state"))
}
