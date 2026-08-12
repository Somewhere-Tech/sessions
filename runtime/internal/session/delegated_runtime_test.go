package session

import (
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestAgentCreatedClaudeChildDefaultsToStructuredRuntime(t *testing.T) {
	request, err := resolveDelegatedRuntimeDefault(state.CreateSessionRequest{
		Cmd: "claude", CreatorSessionID: "11111111-1111-4111-8111-111111111111", DelegationKind: "agent",
	})
	if err != nil {
		t.Fatal(err)
	}
	if request.Kind != state.KindClaudeStructured {
		t.Fatalf("kind = %q, want %q", request.Kind, state.KindClaudeStructured)
	}
}

func TestDelegatedClaudeRuntimeDefaultPreservesExplicitChoices(t *testing.T) {
	tests := []struct {
		name    string
		request state.CreateSessionRequest
		want    string
	}{
		{
			name:    "person-created linked session stays interactive",
			request: state.CreateSessionRequest{Cmd: "claude", CreatorSessionID: "parent", DelegationKind: "user"},
		},
		{
			name:    "explicit terminal child stays interactive",
			request: state.CreateSessionRequest{Cmd: "claude", CreatorSessionID: "parent", DelegationKind: "agent", ProviderTerminal: true},
		},
		{
			name:    "explicit structured child stays structured",
			request: state.CreateSessionRequest{Cmd: "claude", CreatorSessionID: "parent", DelegationKind: "agent", Kind: state.KindClaudeStructured},
			want:    state.KindClaudeStructured,
		},
		{
			name:    "legacy unattributed child remains compatible",
			request: state.CreateSessionRequest{Cmd: "claude", CreatorSessionID: "parent"},
		},
		{
			name:    "codex child keeps its selected runtime",
			request: state.CreateSessionRequest{Cmd: "codex", CreatorSessionID: "parent", DelegationKind: "agent"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveDelegatedRuntimeDefault(test.request)
			if err != nil {
				t.Fatal(err)
			}
			if got.Kind != test.want {
				t.Fatalf("kind = %q, want %q", got.Kind, test.want)
			}
		})
	}
}

func TestDelegatedRuntimeRejectsInvalidTerminalHints(t *testing.T) {
	for _, request := range []state.CreateSessionRequest{
		{Cmd: "codex", ProviderTerminal: true},
		{Cmd: "claude", ProviderTerminal: true, Kind: state.KindClaudeStructured},
	} {
		if _, err := resolveDelegatedRuntimeDefault(request); err == nil {
			t.Fatalf("request %#v unexpectedly succeeded", request)
		}
	}
}
