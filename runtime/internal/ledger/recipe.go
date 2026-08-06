package ledger

import (
	"path/filepath"
	"regexp"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
)

// Claude and Codex both identify a conversation with a canonical UUID, and
// recovery already requires that exact shape before it will adopt one
// (recovery/adopt.go strictProviderPattern). The former unbounded
// `[0-9a-f-]{8,}` accepted values that are not conversation ids at all —
// "--------" among them — and durably recorded them as resume recipes. Both the
// shape and the argv spellings it validates now live in internal/providerargs.
var userCreatorPattern = regexp.MustCompile(`^(?:uid:[0-9]+|sid:S-[0-9]+(?:-[0-9]+)+)$`)

// SafeResumeRecipe follows the normative TypeScript argument forms while
// intentionally discarding every unrelated argument. The result can contain
// a provider identity and mode switch, but never a prompt, environment value,
// or arbitrary positional argument.
func SafeResumeRecipe(tool, cmd string, args []string) (providerUUID string, argv []string) {
	base := strings.ToLower(filepath.Base(cmd))
	if tool == "claude-code" || base == "claude" {
		for _, candidate := range providerargs.Values(args, providerargs.ClaudeIdentityFlags()...) {
			if providerargs.IsConversationUUID(candidate) {
				return candidate, []string{cmd, "--resume", candidate}
			}
		}
		return "", nil
	}
	if tool == "codex" || base == "codex" {
		for _, candidate := range providerargs.Values(args, providerargs.CodexResumeFlags()...) {
			if providerargs.IsConversationUUID(candidate) {
				return candidate, []string{cmd, "resume", candidate}
			}
		}
	}
	return "", nil
}

// ExistingProviderResume recognizes only commands which reopen an existing
// conversation. In particular, Claude --session-id is deliberately excluded:
// Sessions generates that UUID for a fresh session and it is not a reattach.
func ExistingProviderResume(cmd string, args []string) (providerUUID string, argv []string) {
	base := strings.ToLower(filepath.Base(cmd))
	if base == "claude" {
		for _, candidate := range providerargs.Values(args, providerargs.ClaudeResumeFlags()...) {
			if providerargs.IsConversationUUID(candidate) {
				return candidate, []string{cmd, "--resume", candidate}
			}
		}
		return "", nil
	}
	if base == "codex" {
		for _, candidate := range providerargs.Values(args, providerargs.CodexResumeFlags()...) {
			if providerargs.IsConversationUUID(candidate) {
				return candidate, []string{cmd, "resume", candidate}
			}
		}
	}
	return "", nil
}

// ResumeRecipeForProvider builds the minimal recipe used after a provider is
// bound asynchronously (notably a fresh Codex rollout).
func ResumeRecipeForProvider(tool, cmd, providerUUID string) []string {
	if !providerargs.IsConversationUUID(providerUUID) {
		return nil
	}
	base := strings.ToLower(filepath.Base(cmd))
	switch {
	case tool == "claude-code" || base == "claude":
		return []string{cmd, "--resume", providerUUID}
	case tool == "codex" || base == "codex":
		return []string{cmd, "resume", providerUUID}
	default:
		return nil
	}
}
