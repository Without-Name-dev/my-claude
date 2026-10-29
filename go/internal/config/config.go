// Package config reads process configuration from the environment.
//
// Every value has a working default so a fresh checkout runs with no setup,
// and the environment names are the ones the Python deployment already uses
// where they overlap (AGENT_SERVER_SECRET).
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Server is everything the HTTP service needs to start.
type Server struct {
	Listen          string
	DBPath          string
	UploadDir       string
	PublicURL       string
	CORSOrigins     []string
	LogLevel        string
	SessionTTL      time.Duration
	WSOriginPattern []string
	// MaxUploadBytes caps a single attachment upload.
	MaxUploadBytes int64
	// WebDir, when set, is a directory of pre-built client files served from
	// the same origin as the API. Empty means API only.
	WebDir string
	// GatewayMaxBodyBytes caps one model gateway request body. Gateway
	// requests carry images and long contexts, so the JSON cap that protects
	// the management API does not apply to them.
	GatewayMaxBodyBytes int64
	// GatewayUpstreamHeaderTimeout bounds how long the gateway waits for the
	// upstream's response headers. The body itself may stream for as long as
	// both sides stay connected.
	GatewayUpstreamHeaderTimeout time.Duration
}

// LoadServer builds the server configuration from the environment.
func LoadServer() Server {
	root := env("AA_DATA_DIR", "data")
	return Server{
		Listen:                       env("AA_LISTEN", ":8080"),
		DBPath:                       env("AA_DB_PATH", filepath.Join(root, "server.db")),
		UploadDir:                    env("AA_UPLOAD_DIR", filepath.Join(root, "uploads")),
		PublicURL:                    strings.TrimRight(env("AA_PUBLIC_URL", "http://127.0.0.1:8080"), "/"),
		CORSOrigins:                  splitList(env("AA_CORS_ORIGINS", "")),
		LogLevel:                     env("AA_LOG_LEVEL", "info"),
		SessionTTL:                   durationEnv("AA_SESSION_TTL", 7*24*time.Hour),
		WSOriginPattern:              splitList(env("AA_WS_ORIGINS", "*")),
		MaxUploadBytes:               int64Env("AA_MAX_UPLOAD_BYTES", 256<<20),
		WebDir:                       env("AA_WEB_DIR", ""),
		GatewayMaxBodyBytes:          int64Env("AA_GATEWAY_MAX_BODY_BYTES", 64<<20),
		GatewayUpstreamHeaderTimeout: durationEnv("AA_GATEWAY_UPSTREAM_HEADER_TIMEOUT", 5*time.Minute),
	}
}

// Connector is what the desktop-side process needs.
type Connector struct {
	ServerURL   string
	ConfigPath  string
	DataDir     string
	BridgeDir   string
	LogLevel    string
	RuntimeName string
	// TokenOverride lets a token from the command line stand in for a pairing
	// file, which is how the web app's "command line" instructions work.
	TokenOverride string
}

// LoadConnector builds the connector configuration from the environment.
func LoadConnector() Connector {
	return Connector{
		ServerURL:   strings.TrimRight(env("AA_SERVER_URL", "http://127.0.0.1:8080"), "/"),
		ConfigPath:  env("AA_CONNECTOR_CONFIG", ""),
		DataDir:     env("AA_CONNECTOR_DATA_DIR", filepath.Join(homeDir(), ".agents-anywhere")),
		BridgeDir:   env("AA_BRIDGE_DIR", filepath.Join(homeDir(), ".claude", "phone-bridge")),
		LogLevel:    env("AA_LOG_LEVEL", "info"),
		RuntimeName: env("AA_RUNTIME_NAME", "claude"),
	}
}

func env(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return fallback
}

func splitList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func durationEnv(key string, fallback time.Duration) time.Duration {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil {
		return time.Duration(n) * time.Second
	}
	return fallback
}

func int64Env(key string, fallback int64) int64 {
	if n, err := strconv.ParseInt(os.Getenv(key), 10, 64); err == nil && n > 0 {
		return n
	}
	return fallback
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return "."
}
