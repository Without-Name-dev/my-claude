// Command aa-connector links a desktop running Claude Code to the server.
//
// It has nothing to drive on its own: the phone-pane plugin inside the Claude
// session does the work, and this process carries messages to and from it
// through the bridge directory.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/agents-anywhere/agents-anywhere/internal/bridge"
	"github.com/agents-anywhere/agents-anywhere/internal/config"
	"github.com/agents-anywhere/agents-anywhere/internal/connector"
	"github.com/agents-anywhere/agents-anywhere/internal/logx"
)

// connectorFile is what a pairing session writes for this process to use.
type connectorFile struct {
	ServerURL      string `json:"serverUrl"`
	ConnectorID    string `json:"connectorId"`
	ConnectorToken string `json:"connectorToken"`
	Name           string `json:"name,omitempty"`
}

func main() {
	configPath := flag.String("config", "", "path to connector.json written by pairing")
	pair := flag.Bool("pair", false, "ask for a pairing code, wait for it to be entered, then exit")
	pairServer := flag.String("server", "", "server URL to pair with (with --pair)")
	tokenFlag := flag.String("token", "", "connector token, instead of a pairing file")
	flag.Parse()

	cfg := config.LoadConnector()
	if *configPath != "" {
		cfg.ConfigPath = *configPath
	}
	log := logx.Setup(cfg.LogLevel)

	if *pair {
		serverURL := *pairServer
		if serverURL == "" {
			serverURL = cfg.ServerURL
		}
		if err := runPair(serverURL, cfg.DataDir); err != nil {
			log.Error("pairing failed", "error", err)
			os.Exit(1)
		}
		return
	}

	if *pairServer != "" {
		cfg.ServerURL = trimRightSlash(*pairServer)
	}
	if *tokenFlag != "" {
		cfg.TokenOverride = *tokenFlag
	}
	profile, err := loadProfile(cfg)
	if err != nil {
		log.Error("connector is not paired", "error", err,
			"hint", "run 'aa-connector --config <connector.json>' or set AA_CONNECTOR_TOKEN")
		os.Exit(1)
	}

	b := bridge.New(cfg.BridgeDir)
	if !bridge.Enabled() {
		log.Warn("the plugin bridge is off; set AGENT_CONNECTOR_CLAUDE_PLUGIN_BRIDGE=1 to drive sessions",
			"dir", cfg.BridgeDir)
		// A nil bridge makes the switch real: hello and beat advertise no
		// delivery capability, so the server never sends a submit here.
		b = nil
	}

	client := connector.NewClient(wsURL(profile.ServerURL), profile.ConnectorToken,
		profile.displayName(), b, log)
	// Attachment downloads go over HTTP to the same server that was paired.
	client.ServerURL = profile.ServerURL

	client.Discovery = connector.NewDiscovery(log)

	if b != nil {
		approvals := connector.NewApprovalServer(b.ApprovalPath(), approvalToken(b), client.Ask, log)
		if err := approvals.Start(); err != nil {
			log.Error("could not start the approval endpoint", "error", err)
			os.Exit(1)
		}
		defer approvals.Stop()
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log.Info("connector starting",
		"server", profile.ServerURL,
		"connector", profile.ConnectorID,
		"bridge", cfg.BridgeDir)

	client.Run(ctx)
	log.Info("connector stopped")
}

// loadProfile reads the pairing file or falls back to the environment.
func loadProfile(cfg config.Connector) (connectorFile, error) {
	path := cfg.ConfigPath
	if path == "" {
		path = filepath.Join(cfg.DataDir, "connector.json")
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		var profile connectorFile
		if err := json.Unmarshal(raw, &profile); err != nil {
			return connectorFile{}, fmt.Errorf("parse %s: %w", path, err)
		}
		if profile.ConnectorToken == "" {
			return connectorFile{}, errors.New("connector token is empty in " + path)
		}
		if profile.ServerURL == "" {
			profile.ServerURL = cfg.ServerURL
		}
		return profile, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return connectorFile{}, err
	}

	token := cfg.TokenOverride
	if token == "" {
		token = os.Getenv("AA_CONNECTOR_TOKEN")
	}
	if token == "" {
		return connectorFile{}, fmt.Errorf("%s does not exist and AA_CONNECTOR_TOKEN is unset", path)
	}
	return connectorFile{
		ServerURL:      cfg.ServerURL,
		ConnectorID:    os.Getenv("AA_CONNECTOR_ID"),
		ConnectorToken: token,
	}, nil
}

// displayName is what the dashboard shows for this machine.
func (p connectorFile) displayName() string {
	if p.Name != "" {
		return p.Name
	}
	if host, err := os.Hostname(); err == nil {
		return host
	}
	return "Agents Anywhere Connector"
}

// wsURL turns the configured http base into the websocket endpoint.
func wsURL(base string) string {
	scheme := "ws"
	rest := base
	if len(base) >= 8 && base[:8] == "https://" {
		scheme, rest = "wss", base[8:]
	} else if len(base) >= 7 && base[:7] == "http://" {
		rest = base[7:]
	}
	rest = trimRightSlash(rest)
	return scheme + "://" + rest + "/api/v2/connector/ws"
}

func trimRightSlash(value string) string {
	for len(value) > 0 && value[len(value)-1] == '/' {
		value = value[:len(value)-1]
	}
	return value
}

// approvalToken is the secret the plugin must present. It is regenerated per
// process and published next to the endpoint, so a stale plugin copy cannot
// answer for this one.
func approvalToken(b *bridge.Bridge) string {
	token, err := b.Token()
	if err != nil {
		// A missing token would mean every approval is refused; fall back to a
		// value the plugin cannot read, which fails closed.
		return fmt.Sprintf("unavailable-%d", time.Now().UnixNano())
	}
	return token
}
