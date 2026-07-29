package state

import (
	"fmt"
	"regexp"
)

var profileNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidateProfileName enforces the stable CLI/API profile name contract.
func ValidateProfileName(name string) error {
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("invalid profile %q: use 1-32 lowercase letters, digits, or hyphens", name)
	}
	return nil
}

// ProfileToolName returns the on-disk profile tool component.
func ProfileToolName(tool SessionTool) (string, bool) {
	switch tool {
	case ToolClaude:
		return "claude", true
	case ToolCodex:
		return "codex", true
	default:
		return "", false
	}
}

// CommandTool exposes the registry's command classification to the manager's
// pre-launch profile preparation without duplicating the classification rules.
func CommandTool(command string) SessionTool { return classifyTool(command) }
