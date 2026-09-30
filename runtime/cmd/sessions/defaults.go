package main

import (
	"fmt"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func normalizeClaudePermissionDefault(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "inherit", "settings", "provider":
		return state.ClaudeChoiceInherit, true
	case "manual", "ask":
		return state.ClaudePermissionManual, true
	case "acceptedits", "accept-edits", "edits":
		return state.ClaudePermissionAcceptEdits, true
	case "auto":
		return state.ClaudePermissionAuto, true
	case "plan", "plan-only":
		return state.ClaudePermissionPlan, true
	case "dontask", "dont-ask":
		return state.ClaudePermissionDontAsk, true
	case "bypasspermissions", "bypass", "full", "skip", "skip-permissions":
		return state.ClaudePermissionBypass, true
	default:
		return "", false
	}
}

func claudePermissionDefaultLabel(value string) string {
	switch value {
	case state.ClaudeChoiceInherit:
		return "Claude settings"
	case state.ClaudePermissionManual:
		return "Ask every time"
	case state.ClaudePermissionAcceptEdits:
		return "Accept edits"
	case state.ClaudePermissionAuto:
		return "Auto"
	case state.ClaudePermissionPlan:
		return "Plan only"
	case state.ClaudePermissionDontAsk:
		return "Don't ask"
	case state.ClaudePermissionBypass:
		return "Full access (skip permissions)"
	default:
		return value
	}
}

// cmdDefaults gives agents the same durable launch-default control as the
// native Settings view. It changes only future Claude launches; a live
// provider owns its current permission mode.
func (a *app) cmdDefaults(args []string) error {
	var current state.ClaudeSettings
	if err := a.getJSON("/api/claude/settings", &current); err != nil {
		return err
	}

	if value, present := pluck(&args, "--permissions"); present {
		normalized, ok := normalizeClaudePermissionDefault(value)
		if !ok {
			return fail(1, "--permissions must be settings, ask, accept-edits, auto, plan, dont-ask, or full")
		}
		current.PermissionMode = normalized
		if err := a.putJSON("/api/claude/settings", current, &current, 2); err != nil {
			return err
		}
	}
	if len(args) != 0 {
		return fail(1, "usage: sessions defaults [--permissions settings|ask|accept-edits|auto|plan|dont-ask|full]")
	}

	if a.wantJSON {
		return writeJSON(a.stdout, current, true)
	}
	_, err := fmt.Fprintf(a.stdout, "Claude permissions for new sessions: %s\n", claudePermissionDefaultLabel(current.PermissionMode))
	return err
}
