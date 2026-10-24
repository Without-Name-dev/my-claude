// Package logx configures the process logger.
package logx

import (
	"log/slog"
	"os"
	"strings"
)

// Setup installs a JSON handler at the requested level. Levels are spelled the
// way the rest of the stack spells them: debug, info, warn, error.
func Setup(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
