package server

import (
	"fmt"
	"strings"
)

// Command represents a parsed slash command
type Command struct {
	Name string
	Args []string
	Raw  string
}

// CommandType represents the type of command
type CommandType string

const (
	// Session configuration commands
	CommandEffort    CommandType = "effort"
	CommandModel     CommandType = "model"
	CommandFast      CommandType = "fast"
	CommandAutoMode  CommandType = "auto"

	// Session management commands
	CommandCompact   CommandType = "compact"
	CommandClear     CommandType = "clear"

	// Information commands
	CommandHelp      CommandType = "help"
	CommandConfig    CommandType = "config"
	CommandWorkflows CommandType = "workflows"

	// Unknown command
	CommandUnknown   CommandType = "unknown"
)

// ParseCommand parses a slash command string
func ParseCommand(input string) (*Command, error) {
	input = strings.TrimSpace(input)

	if !strings.HasPrefix(input, "/") {
		return nil, fmt.Errorf("command must start with /")
	}

	// Remove leading /
	input = strings.TrimPrefix(input, "/")

	// Split into parts
	parts := strings.Fields(input)
	if len(parts) == 0 {
		return nil, fmt.Errorf("empty command")
	}

	cmd := &Command{
		Name: strings.ToLower(parts[0]),
		Raw:  input,
	}

	if len(parts) > 1 {
		cmd.Args = parts[1:]
	}

	return cmd, nil
}

// Type returns the command type
func (c *Command) Type() CommandType {
	switch c.Name {
	case "effort":
		return CommandEffort
	case "model":
		return CommandModel
	case "fast":
		return CommandFast
	case "auto":
		return CommandAutoMode
	case "compact":
		return CommandCompact
	case "clear":
		return CommandClear
	case "help":
		return CommandHelp
	case "config":
		return CommandConfig
	case "workflows":
		return CommandWorkflows
	default:
		return CommandUnknown
	}
}

// ValidateEffort validates effort level argument
func ValidateEffort(level string) bool {
	level = strings.ToLower(level)
	validLevels := []string{"low", "medium", "high", "xhigh", "max", "auto"}
	for _, valid := range validLevels {
		if level == valid {
			return true
		}
	}
	return false
}

// ValidateModel validates model name argument
func ValidateModel(model string) bool {
	model = strings.ToLower(model)
	validModels := []string{
		"opus-5.5", "sonnet-5.5", "haiku-5.5",
		"opus", "sonnet", "haiku",
		"claude-opus-5-5", "claude-sonnet-5-5", "claude-haiku-5-5",
	}
	for _, valid := range validModels {
		if model == valid {
			return true
		}
	}
	return false
}

// NormalizeModel converts various model names to canonical form
func NormalizeModel(model string) string {
	model = strings.ToLower(model)
	switch model {
	case "opus", "opus-5.5", "claude-opus-5-5":
		return "claude-opus-5-5"
	case "sonnet", "sonnet-5.5", "claude-sonnet-5-5":
		return "claude-sonnet-5-5"
	case "haiku", "haiku-5.5", "claude-haiku-5-5":
		return "claude-haiku-5-5"
	default:
		return model
	}
}

// GetCommandHelp returns help text for commands
func GetCommandHelp() map[string]string {
	return map[string]string{
		"effort":    "Set reasoning effort level: /effort <low|medium|high|xhigh|max|auto>",
		"model":     "Switch AI model: /model <opus|sonnet|haiku>",
		"fast":      "Toggle fast mode: /fast [on|off]",
		"auto":      "Toggle auto mode: /auto mode <on|off>",
		"compact":   "Compact conversation history: /compact",
		"clear":     "Clear current session: /clear",
		"help":      "Show this help message: /help",
		"config":    "Show configuration panel: /config",
		"workflows": "Show workflows panel: /workflows",
	}
}
