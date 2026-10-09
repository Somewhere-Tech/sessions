package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A child's account is decided here, by the daemon, rather than by whichever
// client asked for it. A Claude or Codex child of a session on a named account
// starts on that same account unless the request names another account or
// explicitly asks for the default login. The parent's account is read from its
// own durable creation record, never from the caller, and an account is never
// carried across providers: a Claude subscription says nothing about Codex.

var errAccountChoiceConflict = errors.New("choose either a named account (profile) or the default account (defaultProfile), not both")

// resolveRequestedProfile settles which profile a create will use and names
// its provider home. It performs no writes, so a replayed operation id is
// answered before any profile directory is created or touched.
func (m *Manager) resolveRequestedProfile(ctx context.Context, request state.CreateSessionRequest) (state.CreateSessionRequest, error) {
	if request.Profile == "" && !request.DefaultProfile {
		inherited, err := m.delegatingAccount(ctx, request)
		if err != nil {
			return request, err
		}
		request.Profile = inherited
	}
	if request.Profile == "" {
		return request, nil
	}
	configDir, err := m.profileDirectory(request.Cmd, request.Profile)
	if err != nil {
		return request, err
	}
	request.ConfigDir = configDir
	return request, nil
}

// delegatingAccount is the account a same-provider child inherits, or "" when
// there is nothing to inherit. A parent that cannot be found, is archived, or
// has no named account leaves the child on the default login, exactly as
// before; resolveCreator still rejects an unknown or archived parent.
func (m *Manager) delegatingAccount(ctx context.Context, request state.CreateSessionRequest) (string, error) {
	if request.CreatorSessionID == "" || request.CreatorOwnerID != "" || m.ledgerReader == nil {
		return "", nil
	}
	if request.Kind == state.KindLane {
		return "", nil
	}
	childTool, supported := state.ProfileToolName(state.CommandTool(request.Cmd))
	if !supported || ledger.ValidateCreator(ledger.CreatorSession, request.CreatorSessionID) != nil {
		return "", nil
	}
	events, err := m.ledgerReader.Events(ctx, request.CreatorSessionID)
	if err != nil {
		return "", fmt.Errorf("read the delegating session's account: %w; nothing was launched, so retry, or pass an explicit profile or the default account", err)
	}
	for _, lane := range ledger.Fold(events) {
		if lane.LaneID != request.CreatorSessionID || !lane.Created || lane.Archived {
			continue
		}
		parentTool, supported := state.ProfileToolName(state.SessionTool(lane.Tool))
		if !supported || parentTool != childTool {
			return "", nil
		}
		return lane.Profile, nil
	}
	return "", nil
}
