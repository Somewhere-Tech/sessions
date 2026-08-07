package state

import "strings"

// maxConversationTitleRunes bounds a provider title to something that reads as
// a label in a list. It is well inside the 120-rune session-name rule, so a
// compacted title is always a legal session name.
const maxConversationTitleRunes = 96

// CompactConversationTitle collapses a provider title to a single line and
// bounds it. It is the shared shaping used by history conversation naming and
// by the daemon when it adopts a title as the session's name, so the two
// surfaces can never render the same conversation differently.
func CompactConversationTitle(value string) string {
	value = strings.Join(strings.Fields(strings.TrimSpace(value)), " ")
	runes := []rune(value)
	if len(runes) <= maxConversationTitleRunes {
		return value
	}
	return strings.TrimSpace(string(runes[:maxConversationTitleRunes-1])) + "…"
}

// ProviderConversationTitle returns the provider's own name for a
// conversation, or "" when the provider has not titled it yet.
//
// Claude records its title in the conversation transcript: a `custom-title`
// record is the title a person set inside Claude, an `ai-title` record is the
// one Claude generated, and a custom title outranks a generated one.
// recordClaudeLocked applies both to SessionInfo for every Claude session --
// the structured runner's events and the PTY transcript watcher's events
// arrive on the same path -- so this reads facts the daemon already has rather
// than parsing the transcript a second time.
//
// Codex has no counterpart. A local rollout carries only session_meta,
// turn_context, response_item, event_msg, world_state, and compacted records;
// `set_thread_title` exists in Codex's remote thread API but is never written
// to the rollout file, so there is no on-disk Codex title to follow.
func ProviderConversationTitle(customTitle, aiTitle string) string {
	for _, candidate := range []string{customTitle, aiTitle} {
		if value := CompactConversationTitle(candidate); value != "" {
			return value
		}
	}
	return ""
}
