package connector

// Model gateway wiring. The server can push a gateway endpoint plus a narrow,
// revocable credential to this connector; every Claude terminal the connector
// opens then points at the gateway, and the server decides which of the
// user's providers serves each request.
//
// The credential is stored in the connector's state directory, never in the
// bridge (the plugin shares that directory and never needs the token), never
// on a command line, and never in a log line.

import (
	"encoding/json"
	"fmt"
	"github.com/agents-anywhere/agents-anywhere/internal/protocol"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// GatewayConfig is the pushed gateway wiring.
type GatewayConfig struct {
	BaseURL   string `json:"baseUrl"`
	Token     string `json:"token"`
	UpdatedAt int64  `json:"updatedAt"`
}

// gatewayEnv holds the environment additions spawned terminals receive. It is
// package state because the spawn path (startSessionTerminal and its platform
// helpers) is deliberately independent of the Client.
var gatewayEnv atomic.Pointer[[]string]

// gatewayStateDir is where gateway.json lives; set from main so the file is
// stable regardless of whether the plugin bridge is enabled.
var gatewayStateDir atomic.Pointer[string]

// gatewayWriteMu serializes writers of gateway.json. RPC handlers run in their
// own goroutines, so an enable push and a disable/resync clear can otherwise
// interleave their writes.
var gatewayWriteMu sync.Mutex

// SetGatewayStateDir records where the gateway config file lives.
func SetGatewayStateDir(dir string) {
	gatewayStateDir.Store(&dir)
}

func gatewayConfigPath() string {
	dir := gatewayStateDir.Load()
	if dir == nil || *dir == "" {
		return ""
	}
	return filepath.Join(*dir, "gateway.json")
}

// LoadGatewayConfig reads the stored wiring, if any. The spawn path consults
// it through gatewayEnvironment on every terminal open.
func LoadGatewayConfig() *GatewayConfig {
	path := gatewayConfigPath()
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg GatewayConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	if cfg.BaseURL == "" || cfg.Token == "" {
		return nil
	}
	return &cfg
}

// saveGatewayConfig stores the wiring atomically with owner-only permissions.
// The temporary file is unique per call so concurrent writers cannot share an
// offset, and the rename is atomic on every supported platform.
func saveGatewayConfig(cfg *GatewayConfig) error {
	path := gatewayConfigPath()
	if path == "" {
		return fmt.Errorf("gateway state directory is not configured")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, "gateway-*.tmp")
	if err != nil {
		return err
	}
	tmp := file.Name()
	if _, err := file.Write(raw); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chmod(tmp, 0o600)
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func clearGatewayConfig() error {
	path := gatewayConfigPath()
	if path == "" {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ApplyGatewayConfig installs the wiring (or removes it when enabled is
// false) and refreshes the environment additions terminals receive.
func ApplyGatewayConfig(cfg *GatewayConfig) error {
	gatewayWriteMu.Lock()
	defer gatewayWriteMu.Unlock()
	var err error
	switch {
	case cfg != nil && cfg.BaseURL != "" && cfg.Token != "":
		err = saveGatewayConfig(cfg)
	default:
		err = clearGatewayConfig()
	}
	refreshGatewayEnvironment()
	return err
}

// LoadStoredGatewayConfig installs whatever wiring was persisted by a
// previous run into the spawn environment. Startup calls this instead of
// ApplyGatewayConfig, whose nil argument means "clear".
func LoadStoredGatewayConfig() { refreshGatewayEnvironment() }

// refreshGatewayEnvironment recomputes the env additions from the stored
// config. Splitting it from Apply keeps a config read at startup cheap.
func refreshGatewayEnvironment() {
	additions := []string(nil)
	if cfg := LoadGatewayConfig(); cfg != nil {
		additions = []string{
			"ANTHROPIC_BASE_URL=" + cfg.BaseURL,
			"ANTHROPIC_AUTH_TOKEN=" + cfg.Token,
		}
	}
	gatewayEnv.Store(&additions)
}

// gatewayEnvironment returns the env entries a spawned terminal needs for the
// model gateway. Empty when the gateway is not wired.
func gatewayEnvironment() []string {
	if additions := gatewayEnv.Load(); additions != nil {
		return *additions
	}
	refreshGatewayEnvironment()
	if additions := gatewayEnv.Load(); additions != nil {
		return *additions
	}
	return nil
}

// handleGatewayConfig answers the server's gateway.config RPC: install or
// clear the wiring. The result echoes the live state so the server can tell
// an accepted config from one that never arrived.
func (c *Client) handleGatewayConfig(raw json.RawMessage) (json.RawMessage, *protocol.RPCError) {
	var params struct {
		BaseURL string `json:"baseUrl"`
		Token   string `json:"token"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, &protocol.RPCError{Code: "bad_params", Message: "gateway.config params are unreadable"}
	}
	var cfg *GatewayConfig
	if params.Enabled && params.Token != "" {
		cfg = &GatewayConfig{BaseURL: params.BaseURL, Token: params.Token, UpdatedAt: time.Now().UnixMilli()}
	}
	if err := ApplyGatewayConfig(cfg); err != nil {
		c.Log.Warn("gateway config not applied", "error", err) // never the token
		return nil, &protocol.RPCError{Code: "apply_failed", Message: "could not store the gateway config"}
	}
	live := LoadGatewayConfig() != nil
	c.Log.Info("gateway config updated", "enabled", live, "baseUrl", params.BaseURL)
	result, _ := json.Marshal(map[string]any{"ok": true, "enabled": live})
	return result, nil
}
