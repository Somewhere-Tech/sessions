package session

import (
	"errors"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// resolveDelegatedRuntimeDefault keeps an agent-created Claude child on the
// provider's structured interface. That gives its manager exact events and a
// reliable submit/stop boundary without enrolling every helper in Remote
// Control. User-created linked sessions retain their chosen runtime.
//
// ProviderTerminal is explicit because an empty Kind also represents PTY
// requests from older clients. Applying the default only to newly attributed
// agent requests preserves those older clients' --pty-claude behavior.
func resolveDelegatedRuntimeDefault(request state.CreateSessionRequest) (state.CreateSessionRequest, error) {
	tool := state.CommandTool(request.Cmd)
	if request.ProviderTerminal {
		if tool != state.ToolClaude {
			return request, errors.New("providerTerminal is only valid for Claude sessions")
		}
		if strings.TrimSpace(request.Kind) == state.KindClaudeStructured {
			return request, errors.New("providerTerminal cannot be combined with a structured Claude session")
		}
		return request, nil
	}
	if request.CreatorSessionID != "" &&
		request.DelegationKind == "agent" &&
		tool == state.ToolClaude &&
		strings.TrimSpace(request.Kind) == "" {
		request.Kind = state.KindClaudeStructured
	}
	return request, nil
}
