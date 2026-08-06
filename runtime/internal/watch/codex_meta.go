package watch

import "strings"

// CodexSessionMetaID returns the conversation id recorded in a Codex rollout's
// session_meta payload.
//
// Codex spells this two ways. Most rollouts carry `id`; others carry
// `session_id`, including the rollout shape this repo's own fixtures were taken
// from (see codexRolloutLines in transcript_backfill_test.go, copied from
// ~/.codex/sessions). Five readers of the same line existed and only the usage
// scanner accepted the alias, so one rollout could resolve for billing and
// simultaneously report no provider identity to recovery, to the resumable
// scanner, and to migrate — with the file sitting right there.
//
// The payload is passed in rather than re-read. session_meta is already decoded
// by whoever is calling, and a second reader of the same line is a second thing
// to keep in agreement with the provider.
func CodexSessionMetaID(payload map[string]any) string {
	if value := strings.TrimSpace(stringField(payload, "id")); value != "" {
		return value
	}
	return strings.TrimSpace(stringField(payload, "session_id"))
}

// CodexSubagentParent reports whether a session_meta payload describes a thread
// Codex spawned from another conversation, and names the parent thread when the
// payload records one.
//
// Three implementations of this question disagreed. One required
// source.subagent.thread_spawn to be present, one required only source.subagent,
// and a third ignored source entirely unless the explicit parent fields were
// absent. The answer here is the union: any source.subagent marks a spawned
// thread — which is already what CodexSurface concludes when it folds that
// source to the actor "agent" — and the parent is whichever of the recorded
// spellings names a thread other than this one.
//
// session_id is read as a parent only when it disagrees with id, because that
// is the shape where the two keys are not aliases of each other: a payload that
// carries only session_id is naming itself, and CodexSessionMetaID reads it as
// the conversation's own id.
func CodexSubagentParent(payload map[string]any) (parent string, subagent bool) {
	if payload == nil {
		return "", false
	}
	self := CodexSessionMetaID(payload)
	for _, key := range []string{"forked_from_id", "parent_thread_id", "session_id"} {
		candidate := strings.TrimSpace(stringField(payload, key))
		if candidate != "" && candidate != self {
			parent = candidate
			break
		}
	}
	source, ok := payload["source"].(map[string]any)
	if !ok {
		return parent, parent != ""
	}
	spawnParent, ok := source["subagent"].(map[string]any)
	if !ok {
		return parent, parent != ""
	}
	subagent = true
	if parent != "" {
		return parent, true
	}
	spawn, ok := spawnParent["thread_spawn"].(map[string]any)
	if !ok {
		return "", true
	}
	return strings.TrimSpace(stringField(spawn, "parent_thread_id")), true
}
