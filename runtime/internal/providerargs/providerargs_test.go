package providerargs

import (
	"reflect"
	"testing"
)

// TestClaudeSessionIDUnion pins the union of what the eight former copies of
// this parse each handled. Every row was accepted by at least one copy and
// missed by at least one other, which is what made a Claude session started
// with `-r <uuid>` or `--resume=<uuid>` invisible to the callers that had the
// narrow copy.
func TestClaudeSessionIDUnion(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "session id separated", args: []string{"--session-id", id}, want: id},
		{name: "session id joined", args: []string{"--session-id=" + id}, want: id},
		{name: "resume separated", args: []string{"--resume", id}, want: id},
		{name: "resume joined", args: []string{"--resume=" + id}, want: id},
		{name: "resume shorthand", args: []string{"-r", id}, want: id},
		{name: "identity after other flags", args: []string{"--model", "opus", "-r", id}, want: id},
		{name: "flag is not an identity", args: []string{"--resume", "-r", id}, want: id},
		{name: "trailing flag has no value", args: []string{"--resume"}},
		{name: "no identity", args: []string{"--model", "opus"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClaudeSessionID(test.args); got != test.want {
				t.Fatalf("ClaudeSessionID(%v) = %q, want %q", test.args, got, test.want)
			}
			if test.want != "" && !HasClaudeIdentity(test.args) {
				t.Fatalf("HasClaudeIdentity(%v) = false, want true", test.args)
			}
		})
	}
}

// TestClaudeResumeIDExcludesSessionID pins the one distinction that must
// survive consolidation: --session-id is an id Sessions minted for a fresh
// conversation, so it answers "which conversation" but never "was this a
// reattach".
func TestClaudeResumeIDExcludesSessionID(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	if got := ClaudeResumeID([]string{"--session-id", id}); got != "" {
		t.Fatalf("ClaudeResumeID(--session-id) = %q, want empty", got)
	}
	if got := ClaudeResumeID([]string{"-r", id}); got != id {
		t.Fatalf("ClaudeResumeID(-r) = %q, want %q", got, id)
	}
}

// TestCodexConversationIDUnion pins that -r is not a Codex resume flag and that
// the `resume` subcommand, --resume and --resume= all name a conversation. The
// usage copy read only the two long flags, so a session started as
// `codex resume <uuid>` bound for billing but not for recovery.
func TestCodexConversationIDUnion(t *testing.T) {
	const id = "11111111-2222-3333-4444-555555555555"
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "subcommand", args: []string{"resume", id}, want: id},
		{name: "flag", args: []string{"--resume", id}, want: id},
		{name: "joined", args: []string{"--resume=" + id}, want: id},
		{name: "subcommand then flag", args: []string{"resume", "--resume", id}, want: id},
		{name: "codex has no -r resume", args: []string{"-r", id}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := CodexConversationID(test.args); got != test.want {
				t.Fatalf("CodexConversationID(%v) = %q, want %q", test.args, got, test.want)
			}
		})
	}
}

// TestHasValueCountsJoinedForm pins the case the CLI copy missed: reading
// `--model=opus` as "no model chosen" appends a second --model and hands the
// provider two conflicting answers.
func TestHasValueCountsJoinedForm(t *testing.T) {
	if !HasValue([]string{"--model=opus"}, ModelFlags()...) {
		t.Fatal("HasValue did not see --model=opus")
	}
	if !HasValue([]string{"-m", "opus"}, ModelFlags()...) {
		t.Fatal("HasValue did not see -m opus")
	}
	if HasValue([]string{"--model"}, ModelFlags()...) {
		t.Fatal("HasValue accepted a trailing --model with no value")
	}
}

func TestConfigValue(t *testing.T) {
	args := []string{"-c", `model_reasoning_effort="high"`, "--config", "service_tier=priority"}
	if got := ConfigValue(args, CodexEffortKey); got != "high" {
		t.Fatalf("ConfigValue(effort) = %q, want high", got)
	}
	if got := ConfigValue(args, CodexServiceTierKey); got != "priority" {
		t.Fatalf("ConfigValue(service tier) = %q, want priority", got)
	}
	if !HasConfigValue(args, CodexEffortKey) {
		t.Fatal("HasConfigValue(effort) = false")
	}
	if HasConfigValue(args, "nothing") {
		t.Fatal("HasConfigValue(nothing) = true")
	}
}

func TestWithValueReplacesEitherForm(t *testing.T) {
	got := WithValue([]string{"--model=opus", "--verbose"}, "sonnet", ModelFlags()...)
	want := []string{"--verbose", "--model", "sonnet"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WithValue() = %v, want %v", got, want)
	}
	if cleared := WithValue([]string{"-m", "opus", "--verbose"}, "", ModelFlags()...); !reflect.DeepEqual(cleared, []string{"--verbose"}) {
		t.Fatalf("WithValue(clear) = %v", cleared)
	}
}

func TestWithConfigValue(t *testing.T) {
	got := WithConfigValue([]string{"-c", `model_reasoning_effort="low"`, "--verbose"}, CodexEffortKey, "high")
	want := []string{"--verbose", "-c", "model_reasoning_effort=high"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WithConfigValue() = %v, want %v", got, want)
	}
}

func TestIsConversationUUID(t *testing.T) {
	if !IsConversationUUID("11111111-2222-3333-4444-555555555555") {
		t.Fatal("canonical UUID rejected")
	}
	for _, bad := range []string{"--------", "abc", "11111111-2222-3333-4444", ""} {
		if IsConversationUUID(bad) {
			t.Fatalf("IsConversationUUID(%q) = true", bad)
		}
	}
}
