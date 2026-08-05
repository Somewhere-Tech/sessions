package ledger

import (
	"path/filepath"
	"regexp"
	"strings"
)

// canonicalUUID is the shape of both a Sessions lane UUID and a Claude/Codex
// conversation id.
const canonicalUUID = `(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

var (
	// Claude and Codex both identify a conversation with a canonical UUID, and
	// recovery already requires that exact shape before it will adopt one
	// (recovery/adopt.go strictProviderPattern). The former unbounded
	// `[0-9a-f-]{8,}` accepted values that are not conversation ids at all —
	// "--------" among them — and durably recorded them as resume recipes.
	providerIDPattern  = regexp.MustCompile(canonicalUUID)
	sessionIDPattern   = regexp.MustCompile(canonicalUUID)
	userCreatorPattern = regexp.MustCompile(`^(?:uid:[0-9]+|sid:S-[0-9]+(?:-[0-9]+)+)$`)

	// Claude's real spellings for reopening an existing conversation. The
	// Sessions CLI already treats all three as resume flags
	// (cmd/sessions/commands.go) and the runner reads the same set
	// (cmd/sessions-runner/claude_p.go).
	claudeResumeFlags  = []string{"--resume", "-r"}
	claudeIdentityFlag = "--session-id"
	codexResumeFlags   = []string{"resume", "--resume"}
)

// flagValues returns every value args associates with one of names, in
// argument order. Long flags are matched in both `--flag value` and
// `--flag=value` form; short flags and the Codex `resume` subcommand only take
// a separated value, which is what those CLIs actually accept.
func flagValues(args []string, names ...string) []string {
	values := make([]string, 0, 2)
	for index, argument := range args {
		for _, name := range names {
			if argument == name {
				if index+1 < len(args) {
					values = append(values, args[index+1])
				}
				continue
			}
			if strings.HasPrefix(name, "--") {
				if value, ok := strings.CutPrefix(argument, name+"="); ok {
					values = append(values, value)
				}
			}
		}
	}
	return values
}

// SafeResumeRecipe follows the normative TypeScript argument forms while
// intentionally discarding every unrelated argument. The result can contain
// a provider identity and mode switch, but never a prompt, environment value,
// or arbitrary positional argument.
func SafeResumeRecipe(tool, cmd string, args []string) (providerUUID string, argv []string) {
	base := strings.ToLower(filepath.Base(cmd))
	if tool == "claude-code" || base == "claude" {
		names := append(append([]string{}, claudeResumeFlags...), claudeIdentityFlag)
		for _, candidate := range flagValues(args, names...) {
			if providerIDPattern.MatchString(candidate) {
				return candidate, []string{cmd, "--resume", candidate}
			}
		}
		return "", nil
	}
	if tool == "codex" || base == "codex" {
		for _, candidate := range flagValues(args, codexResumeFlags...) {
			if providerIDPattern.MatchString(candidate) {
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
		for _, candidate := range flagValues(args, claudeResumeFlags...) {
			if providerIDPattern.MatchString(candidate) {
				return candidate, []string{cmd, "--resume", candidate}
			}
		}
		return "", nil
	}
	if base == "codex" {
		for _, candidate := range flagValues(args, codexResumeFlags...) {
			if providerIDPattern.MatchString(candidate) {
				return candidate, []string{cmd, "resume", candidate}
			}
		}
	}
	return "", nil
}

// ResumeRecipeForProvider builds the minimal recipe used after a provider is
// bound asynchronously (notably a fresh Codex rollout).
func ResumeRecipeForProvider(tool, cmd, providerUUID string) []string {
	if !providerIDPattern.MatchString(providerUUID) {
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
