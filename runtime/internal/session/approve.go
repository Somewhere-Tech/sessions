package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Approve answers the permission a Rich Codex lane is waiting on. A lane that
// is not autonomous asks before running a command or changing files; the
// runner holds the request open, the session reads needs-you with the request
// as its detail, and this is the one way through for the app, the CLI, and a
// manager lane acting for the person.
func (m *Manager) Approve(ctx context.Context, id string, control proto.ApprovalControl) (state.SessionInfo, error) {
	current, ok := m.registry.Get(id)
	if !ok {
		return state.SessionInfo{}, fmt.Errorf("%w: session %s", state.ErrSessionNotFound, id)
	}
	info := current.Info()
	if info.Kind != state.KindCodexAppServer {
		return state.SessionInfo{}, errors.New("only a Rich Codex session routes approvals through Sessions; a Terminal session answers its own prompt in the terminal")
	}
	if !proto.ValidApprovalDecision(control.Decision) {
		return state.SessionInfo{}, errors.New("decision must be allow, allow-session, or deny")
	}
	if info.PendingApproval == nil {
		return state.SessionInfo{}, state.ErrNoPendingApproval
	}
	return m.registry.Approve(ctx, id, control)
}
