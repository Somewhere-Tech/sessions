package watch

import "testing"

// TestCodexSessionMetaIDReadsBothSpellings pins the union of the five readers
// this replaced. Only the usage scanner accepted `session_id`, so a rollout
// spelled that way — the shape codexRolloutLines was copied from — resolved for
// billing while recovery, the resumable scanner and migrate all reported the
// conversation as having no provider identity.
func TestCodexSessionMetaIDReadsBothSpellings(t *testing.T) {
	const id = "019fd343-5368-7d40-92ac-3ff75509ab9a"
	tests := []struct {
		name    string
		payload map[string]any
		want    string
	}{
		{name: "id", payload: map[string]any{"id": id}, want: id},
		{name: "session_id alias", payload: map[string]any{"session_id": id}, want: id},
		{name: "id wins over session_id", payload: map[string]any{"id": id, "session_id": "other"}, want: id},
		{name: "blank id falls through", payload: map[string]any{"id": "", "session_id": id}, want: id},
		{name: "trimmed", payload: map[string]any{"id": " " + id + " "}, want: id},
		{name: "absent", payload: map[string]any{"cwd": "/tmp"}},
		{name: "nil payload"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CodexSessionMetaID(test.payload); got != test.want {
				t.Fatalf("CodexSessionMetaID(%v) = %q, want %q", test.payload, got, test.want)
			}
		})
	}
}

// TestCodexSessionMetaIDReadsTheRolloutFixture ties the alias to the rollout
// shape this repo records as representative of the real provider output.
func TestCodexSessionMetaIDReadsTheRolloutFixture(t *testing.T) {
	payload := map[string]any{
		"session_id": "019fd343-5368-7d40-92ac-3ff75509ab9a",
		"cwd":        "/Users/uzair/pretty-PTY",
	}
	if got := CodexSessionMetaID(payload); got != "019fd343-5368-7d40-92ac-3ff75509ab9a" {
		t.Fatalf("the repo's own rollout fixture has no readable identity: %q", got)
	}
}

// TestCodexSubagentParentUnion pins the union of the three implementations that
// disagreed: one required source.subagent.thread_spawn, one required only
// source.subagent, and the third read the explicit parent fields. A payload any
// one of them called a spawned thread is a spawned thread here.
func TestCodexSubagentParentUnion(t *testing.T) {
	const self = "11111111-2222-3333-4444-555555555555"
	const parent = "99999999-8888-7777-6666-555555555555"
	tests := []struct {
		name       string
		payload    map[string]any
		wantParent string
		wantAgent  bool
	}{
		{
			name:       "thread_spawn carries the parent",
			payload:    map[string]any{"id": self, "source": map[string]any{"subagent": map[string]any{"thread_spawn": map[string]any{"parent_thread_id": parent}}}},
			wantParent: parent, wantAgent: true,
		},
		{
			name:      "subagent without thread_spawn is still a spawned thread",
			payload:   map[string]any{"id": self, "source": map[string]any{"subagent": map[string]any{}}},
			wantAgent: true,
		},
		{
			name:       "explicit forked_from_id needs no source",
			payload:    map[string]any{"id": self, "forked_from_id": parent},
			wantParent: parent, wantAgent: true,
		},
		{
			name:       "parent_thread_id at the top level",
			payload:    map[string]any{"id": self, "parent_thread_id": parent},
			wantParent: parent, wantAgent: true,
		},
		{
			name:       "session_id naming another thread is a parent",
			payload:    map[string]any{"id": self, "session_id": parent},
			wantParent: parent, wantAgent: true,
		},
		{
			name:    "session_id naming this thread is not a parent",
			payload: map[string]any{"session_id": self},
		},
		{
			name:    "a plain cli source is not a spawned thread",
			payload: map[string]any{"id": self, "source": "cli"},
		},
		{name: "nil payload"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotParent, gotAgent := CodexSubagentParent(test.payload)
			if gotParent != test.wantParent || gotAgent != test.wantAgent {
				t.Fatalf("CodexSubagentParent(%v) = (%q, %v), want (%q, %v)",
					test.payload, gotParent, gotAgent, test.wantParent, test.wantAgent)
			}
		})
	}
}
