// Package providerargs is the single place that knows how the Claude and Codex
// command lines spell the things Sessions has to read back out of a spawn argv:
// which conversation the session was launched against, and which model and
// effort controls it carries.
//
// That knowledge used to live in a copy per caller — usage, history, backup,
// watch, session, state, ledger, the CLI and the runner each had their own —
// and the copies drifted. The drift was not cosmetic: a Claude session started
// with `-r <uuid>` was invisible to most of them, so Sessions appended a second
// `--session-id` and produced a duplicate conversation, and a Codex session
// started with `codex resume <uuid>` bound for billing but not for recovery.
//
// The rules encoded here are the providers' own:
//
//   - Claude names a conversation with --session-id, --resume, or -r. Long
//     flags accept both `--flag value` and `--flag=value`; -r takes only a
//     separated value.
//   - Codex names a conversation with the `resume` subcommand or --resume. It
//     has no -r resume shorthand: -r means something else there, so reading it
//     as an identity would bind the wrong conversation.
//   - Codex carries effort and service tier as `-c key=value` (or --config),
//     with the value optionally quoted.
//
// Validation is deliberately not baked in. Callers disagree about how strict to
// be for good reasons — the ledger will only durably record a canonical UUID,
// while the rollout resolver matches id prefixes against filenames — so this
// package reports what the argv says and exposes IsConversationUUID for the
// callers that require the canonical shape.
package providerargs

import (
	"regexp"
	"strings"
)

// ClaudeSessionIDFlag pins a fresh Claude conversation to an id Sessions chose.
// It is not a resume flag: Sessions generates that UUID, so a caller asking
// "was this a reattach" must not treat it as one.
const ClaudeSessionIDFlag = "--session-id"

// Codex `-c key=value` keys Sessions reads and writes.
const (
	CodexEffortKey      = "model_reasoning_effort"
	CodexServiceTierKey = "service_tier"
)

var (
	claudeResumeFlags   = []string{"--resume", "-r"}
	claudeIdentityFlags = []string{ClaudeSessionIDFlag, "--resume", "-r"}
	codexResumeFlags    = []string{"resume", "--resume"}
	modelFlags          = []string{"--model", "-m"}
	claudeEffortFlags   = []string{"--effort"}
	configFlags         = []string{"-c", "--config"}

	// conversationUUID is the shape both providers use for a conversation id,
	// and the shape recovery requires before it will adopt one. An unbounded
	// hex-and-dash pattern accepts values that are not ids at all — "--------"
	// among them — which is how non-ids once reached durable resume recipes.
	conversationUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// ClaudeResumeFlags returns the spellings that reopen an existing Claude
// conversation, excluding --session-id.
func ClaudeResumeFlags() []string { return append([]string(nil), claudeResumeFlags...) }

// ClaudeIdentityFlags returns every spelling that names a Claude conversation,
// including --session-id.
func ClaudeIdentityFlags() []string { return append([]string(nil), claudeIdentityFlags...) }

// CodexResumeFlags returns the spellings that reopen an existing Codex
// conversation.
func CodexResumeFlags() []string { return append([]string(nil), codexResumeFlags...) }

// ModelFlags returns the spellings both providers accept for a model choice.
func ModelFlags() []string { return append([]string(nil), modelFlags...) }

// ClaudeEffortFlags returns the spellings Claude accepts for a reasoning
// effort. Codex carries effort as a -c config value instead.
func ClaudeEffortFlags() []string { return append([]string(nil), claudeEffortFlags...) }

// ConfigFlags returns the spellings Codex accepts for `-c key=value`.
func ConfigFlags() []string { return append([]string(nil), configFlags...) }

// IsConversationUUID reports whether value is the canonical UUID shape both
// providers use for a conversation id.
func IsConversationUUID(value string) bool {
	return conversationUUID.MatchString(strings.TrimSpace(value))
}

// Values returns every value args associates with one of names, in argument
// order. A long flag matches in both `--flag value` and `--flag=value` form; a
// short flag or a bare subcommand only takes a separated value, which is what
// those CLIs actually accept. Values are trimmed; a flag with no value at all
// contributes nothing.
func Values(args []string, names ...string) []string {
	values := make([]string, 0, 2)
	for index, argument := range args {
		for _, name := range names {
			if argument == name {
				if index+1 < len(args) {
					values = append(values, strings.TrimSpace(args[index+1]))
				}
				continue
			}
			if strings.HasPrefix(name, "--") {
				if value, ok := strings.CutPrefix(argument, name+"="); ok {
					values = append(values, strings.TrimSpace(value))
				}
			}
		}
	}
	return values
}

// Value returns the first value args associates with one of names, or "".
func Value(args []string, names ...string) string {
	for _, value := range Values(args, names...) {
		return value
	}
	return ""
}

// Has reports whether args mentions any of names at all, in either form. A
// caller adding a default uses this: the flag being present is enough to mean
// "the user already decided", even when they spelled it `--flag=value`.
func Has(args []string, names ...string) bool {
	for _, argument := range args {
		for _, name := range names {
			if argument == name {
				return true
			}
			if strings.HasPrefix(name, "--") && strings.HasPrefix(argument, name+"=") {
				return true
			}
		}
	}
	return false
}

// HasValue reports whether args associates a non-empty value with any of names.
// This is the check to make before appending a default value, and it must count
// the joined form: treating `--model=opus` as "no model" appends a second
// --model and hands the provider two conflicting answers.
func HasValue(args []string, names ...string) bool {
	return len(Values(args, names...)) > 0
}

// ConfigValue returns the value Codex's `-c key=value` associates with key,
// with surrounding quotes removed.
func ConfigValue(args []string, key string) string {
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-c" && args[index] != "--config" {
			continue
		}
		if value, ok := strings.CutPrefix(args[index+1], key+"="); ok {
			return strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return ""
}

// HasConfigValue reports whether args already sets key via `-c key=value`.
func HasConfigValue(args []string, key string) bool {
	for index := 0; index+1 < len(args); index++ {
		if args[index] != "-c" && args[index] != "--config" {
			continue
		}
		if strings.HasPrefix(args[index+1], key+"=") {
			return true
		}
	}
	return false
}

// firstIdentity returns the first candidate that could be a conversation id. A
// value starting with "-" is the next flag, not an id: `codex resume --resume
// <uuid>` and `claude --resume -r <uuid>` both name a conversation, and reading
// the flag itself as the answer binds nothing.
func firstIdentity(candidates []string) string {
	for _, candidate := range candidates {
		if candidate == "" || strings.HasPrefix(candidate, "-") {
			continue
		}
		return candidate
	}
	return ""
}

// ClaudeSessionID returns the conversation a Claude argv names, from any of
// --session-id, --resume or -r, in separated or joined form.
func ClaudeSessionID(args []string) string {
	return firstIdentity(Values(args, claudeIdentityFlags...))
}

// ClaudeResumeID returns the conversation a Claude argv reopens. Unlike
// ClaudeSessionID it ignores --session-id, which Sessions generates for a fresh
// session and which therefore is not a reattach.
func ClaudeResumeID(args []string) string {
	return firstIdentity(Values(args, claudeResumeFlags...))
}

// CodexConversationID returns the conversation a Codex argv names, from the
// `resume` subcommand or --resume in either form.
func CodexConversationID(args []string) string {
	return firstIdentity(Values(args, codexResumeFlags...))
}

// HasClaudeIdentity reports whether a Claude argv already names a conversation,
// so a caller knows not to pin a fresh id over the top of it.
func HasClaudeIdentity(args []string) bool { return Has(args, claudeIdentityFlags...) }

// WithValue replaces every occurrence of names with a single `names[0] value`
// at the end, or removes them all when value is empty.
func WithValue(args []string, value string, names ...string) []string {
	result := make([]string, 0, len(args)+2)
	for index := 0; index < len(args); index++ {
		matched := false
		for _, name := range names {
			if args[index] == name {
				matched = true
				if index+1 < len(args) {
					index++
				}
				break
			}
			if strings.HasPrefix(name, "--") && strings.HasPrefix(args[index], name+"=") {
				matched = true
				break
			}
		}
		if !matched {
			result = append(result, args[index])
		}
	}
	value = strings.TrimSpace(value)
	if value != "" {
		result = append(result, names[0], value)
	}
	return result
}

// WithConfigValue replaces Codex's `-c key=value` with a single trailing entry,
// or removes it when value is empty.
func WithConfigValue(args []string, key, value string) []string {
	result := make([]string, 0, len(args)+2)
	for index := 0; index < len(args); index++ {
		if (args[index] == "-c" || args[index] == "--config") && index+1 < len(args) {
			if _, ok := strings.CutPrefix(args[index+1], key+"="); ok {
				index++
				continue
			}
		}
		result = append(result, args[index])
	}
	value = strings.TrimSpace(value)
	if value != "" {
		result = append(result, "-c", key+"="+value)
	}
	return result
}
