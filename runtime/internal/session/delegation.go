package session

import (
	"context"
	"errors"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func (m *Manager) resolveDelegatedExecution(
	_ context.Context,
	request state.CreateSessionRequest,
	creatorKind ledger.CreatorKind,
	creatorID string,
) (state.CreateSessionRequest, error) {
	requestedPermissions := strings.ToLower(strings.TrimSpace(request.Permissions))
	requestedLifecycle := strings.ToLower(strings.TrimSpace(request.Lifecycle))
	if requestedPermissions != "" && requestedPermissions != state.PermissionsInherit && requestedPermissions != state.PermissionsConstrained && requestedPermissions != state.PermissionsFull {
		return request, errors.New("permissions must be inherit, constrained, or full")
	}
	if requestedLifecycle != "" && requestedLifecycle != state.LifecycleTask && requestedLifecycle != state.LifecycleSession {
		return request, errors.New("lifecycle must be task or session")
	}

	isChild := creatorKind == ledger.CreatorSession
	parentPermissions := state.PermissionsConstrained
	var parentArgs []string
	if isChild {
		if parent, ok := m.registry.Get(creatorID); ok {
			parentInfo := parent.Info()
			parentArgs = append([]string(nil), parentInfo.Args...)
			parentPermissions = parentInfo.Permissions
			if parentPermissions == "" {
				parentPermissions = inferPermissions(parentInfo.Args)
			}
		}
	}
	autonomous := false
	if settings, err := state.LoadSettings(m.config.SettingsPath); err == nil {
		autonomous = settings.EffectiveDelegation().Access == state.DelegatedAccessConsentAutonomous
	}

	inheritExact := false
	switch requestedPermissions {
	case state.PermissionsInherit:
		if !isChild {
			return request, errors.New("permissions inherit requires a parent session")
		}
		request.Permissions = parentPermissions
		inheritExact = true
	case state.PermissionsFull:
		if isChild && parentPermissions != state.PermissionsFull && !autonomous {
			return request, errors.New("a child session cannot exceed its parent's permissions; enable autonomous delegated work in Sessions onboarding or Settings")
		}
		request.Permissions = state.PermissionsFull
	case state.PermissionsConstrained:
		request.Permissions = state.PermissionsConstrained
	default:
		if isChild && request.DelegationKind == "agent" {
			if autonomous {
				request.Permissions = state.PermissionsFull
			} else {
				request.Permissions = parentPermissions
				inheritExact = true
			}
		} else {
			request.Permissions = inferPermissions(request.Args)
		}
	}
	tool := state.CommandTool(request.Cmd)
	if inheritExact && len(parentArgs) > 0 {
		request.Args = applyInheritedPermissions(tool, request.Args, parentArgs, request.Permissions)
	} else {
		request.Args = applyResolvedPermissions(tool, request.Args, request.Permissions)
	}

	if requestedLifecycle != "" {
		request.Lifecycle = requestedLifecycle
	} else if isChild && request.DelegationKind == "agent" {
		request.Lifecycle = state.LifecycleTask
	} else {
		request.Lifecycle = state.LifecycleSession
	}
	return request, nil
}

func inferPermissions(args []string) string {
	for _, argument := range args {
		switch argument {
		case "--dangerously-bypass-approvals-and-sandbox", "--dangerously-skip-permissions", "--full-auto":
			return state.PermissionsFull
		}
	}
	return state.PermissionsConstrained
}

func applyResolvedPermissions(tool state.SessionTool, args []string, permissions string) []string {
	cleaned := stripPermissionArgs(tool, args, permissions == state.PermissionsFull)
	switch tool {
	case state.ToolClaude:
		if permissions == state.PermissionsFull {
			cleaned = append([]string{"--dangerously-skip-permissions"}, cleaned...)
		}
	case state.ToolCodex:
		if permissions == state.PermissionsFull {
			cleaned = append([]string{"--dangerously-bypass-approvals-and-sandbox"}, cleaned...)
		} else if len(extractPermissionArgs(tool, cleaned)) == 0 {
			cleaned = append([]string{"--sandbox", "workspace-write", "--ask-for-approval", "on-request"}, cleaned...)
		}
	}
	return cleaned
}

func applyInheritedPermissions(tool state.SessionTool, childArgs, parentArgs []string, permissions string) []string {
	cleaned := stripAllPermissionArgs(tool, childArgs)
	if permissions == state.PermissionsFull {
		return applyResolvedPermissions(tool, cleaned, permissions)
	}
	inherited := extractPermissionArgs(tool, parentArgs)
	if len(inherited) == 0 {
		return applyResolvedPermissions(tool, cleaned, permissions)
	}
	return append(inherited, cleaned...)
}

func stripPermissionArgs(tool state.SessionTool, args []string, stripConstrained bool) []string {
	if stripConstrained {
		return stripAllPermissionArgs(tool, args)
	}
	cleaned := make([]string, 0, len(args))
	for _, argument := range args {
		if argument == "--dangerously-bypass-approvals-and-sandbox" || argument == "--dangerously-skip-permissions" || argument == "--full-auto" {
			continue
		}
		cleaned = append(cleaned, argument)
	}
	return cleaned
}

func stripAllPermissionArgs(tool state.SessionTool, args []string) []string {
	cleaned := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--dangerously-bypass-approvals-and-sandbox" || argument == "--dangerously-skip-permissions" || argument == "--full-auto" {
			continue
		}
		if (tool == state.ToolCodex && (argument == "--sandbox" || argument == "--ask-for-approval")) ||
			(tool == state.ToolClaude && argument == "--permission-mode") {
			if index+1 < len(args) {
				index++
			}
			continue
		}
		cleaned = append(cleaned, argument)
	}
	return cleaned
}

func extractPermissionArgs(tool state.SessionTool, args []string) []string {
	result := make([]string, 0, 4)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--dangerously-bypass-approvals-and-sandbox" || argument == "--dangerously-skip-permissions" || argument == "--full-auto" {
			result = append(result, argument)
			continue
		}
		if (tool == state.ToolCodex && (argument == "--sandbox" || argument == "--ask-for-approval")) ||
			(tool == state.ToolClaude && argument == "--permission-mode") {
			if index+1 < len(args) {
				result = append(result, argument, args[index+1])
				index++
			}
		}
	}
	return result
}
