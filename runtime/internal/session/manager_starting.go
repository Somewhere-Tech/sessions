package session

import (
	"context"
	"fmt"
	"sync"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Track before the creation event is written, not after: discovery can read
// that event immediately. This is process-local ownership of an executing
// launch, not a heuristic based on a timestamp or an old PID. A daemon restart
// still reconciles the durable record and the runner's actual identity.
func (m *Manager) createRegistryTracked(ctx context.Context, request state.CreateSessionRequest, lifecycle state.CreateLifecycle, release func()) (state.SessionInfo, error) {
	id := ""
	beforeLaunch := lifecycle.BeforeLaunch
	lifecycle.BeforeLaunch = func(ctx context.Context, prepared state.PreparedSession) error {
		id = prepared.Info.ID
		m.starting.Store(id, prepared)
		if beforeLaunch != nil {
			if err := beforeLaunch(ctx, prepared); err != nil {
				return err
			}
		}
		// The creation identity is durable now. Waiting for a service or
		// provider socket must not serialize unrelated session creation.
		release()
		return nil
	}
	defer func() {
		if id != "" {
			m.starting.Delete(id)
		}
	}()
	return m.registry.CreateWithLifecycle(ctx, request, lifecycle)
}

func (m *Manager) acquireCreation(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case m.bindGate <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-m.bindGate }) }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("waiting to record session creation: %w; this request has not launched a runner", ctx.Err())
	}
}

// validateCreateFields refuses a malformed request from its fields alone: the
// operation ids, and the account choice. It reads no ledger, settings,
// provider or disk, so it can run before anything else.
func validateCreateFields(request state.CreateSessionRequest) error {
	if err := state.ValidateStartOperationIDs(request.OperationID, request.PromptOperationID); err != nil {
		return err
	}
	if request.DefaultProfile && request.Profile != "" {
		return errAccountChoiceConflict
	}
	if request.Profile != "" {
		if _, err := profileTool(request.Cmd, request.Profile); err != nil {
			return err
		}
	}
	// The providerTerminal escape hatch is checked against the command and
	// kind alone; the resolved request itself is prepared later.
	_, err := resolveDelegatedRuntimeDefault(request)
	return err
}

// answerRecordedStart answers a repeated operation id from the ledger before
// any account, settings or provider work. That work describes a new launch;
// a recorded launch is already decided and is never re-resolved. It takes no
// lock: an operation, once recorded, stays recorded, and Create repeats the
// lookup under the creation lock so two first attempts still launch once.
func (m *Manager) answerRecordedStart(ctx context.Context, request state.CreateSessionRequest) (state.SessionInfo, bool, error) {
	if err := validateCreateFields(request); err != nil {
		return state.SessionInfo{}, true, err
	}
	return m.replayStart(ctx, request)
}

func (m *Manager) prepareCreateRequest(ctx context.Context, request state.CreateSessionRequest) (state.CreateSessionRequest, error) {
	// Settle the account before the Claude defaults read its home. The home is
	// created only after a replayed operation id has been ruled out.
	request, err := m.resolveRequestedProfile(ctx, request)
	if err != nil {
		return request, err
	}
	request, err = resolveDelegatedRuntimeDefault(request)
	if err != nil {
		return request, err
	}
	request, err = m.applyClaudeDefaults(request)
	if err != nil {
		return request, err
	}
	return m.resolveCodexModelChoice(ctx, request)
}

func (m *Manager) skipStartingArtifact(id string, candidates, deadArtifacts map[string]struct{}) bool {
	if _, starting := m.starting.Load(id); !starting {
		return false
	}
	// Create owns this launch; discovery must not race its registration or
	// classify the pre-socket metadata as lost.
	delete(candidates, id)
	delete(deadArtifacts, id)
	return true
}

// Only expose launches with a durable creation record. This is an executing
// Create call, not evidence that a runner or provider has started working.
func (m *Manager) withStartingSessions(infos []state.SessionInfo, states []ledger.LaneState) []state.SessionInfo {
	seen := make(map[string]bool, len(infos))
	for _, info := range infos {
		seen[info.ID] = true
	}
	for _, lane := range states {
		if seen[lane.LaneID] || !lane.Created || lane.Archived || durablyClosed(lane) {
			continue
		}
		value, exists := m.starting.Load(lane.LaneID)
		if !exists {
			continue
		}
		prepared := value.(state.PreparedSession)
		info := prepared.Info
		infos = append(infos, state.SessionInfo{
			ID: info.ID, Cmd: info.Cmd, Args: append([]string(nil), info.Args...), Cwd: info.Cwd,
			Cols: info.Cols, Rows: info.Rows, CreatedAt: info.CreatedAt, Launching: true,
			Name: prepared.Name, Description: prepared.Description, DescriptionSource: prepared.DescriptionSource,
			Tags: state.CloneTags(prepared.Tags), Kind: prepared.Kind, SpecPath: prepared.SpecPath,
			Tool: prepared.Tool, Permissions: prepared.Permissions, Lifecycle: prepared.Lifecycle,
		})
		seen[lane.LaneID] = true
	}
	return infos
}
