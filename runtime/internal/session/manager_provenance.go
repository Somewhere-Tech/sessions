package session

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// withDurableClosed restores closed records after Registry's short exited
// grace has elapsed. The ledger is authoritative for lifecycle and ownership;
// live runtime details continue to come from Registry while they are present.
func (m *Manager) withDurableClosed(
	ctx context.Context, infos []state.SessionInfo, includeEnded bool,
) []state.SessionInfo {
	states, err := m.ledgerStates(ctx)
	if err != nil {
		log.Printf("[ledger] read durable closed sessions: %v", err)
		return infos
	}
	return m.withDurableClosedStates(infos, states, includeEnded)
}

func (m *Manager) withDurableClosedStates(
	infos []state.SessionInfo, states []ledger.LaneState, includeEnded bool,
) []state.SessionInfo {
	archived := make(map[string]struct{})
	for _, lane := range states {
		if lane.Archived {
			archived[lane.LaneID] = struct{}{}
		}
	}
	filtered := make([]state.SessionInfo, 0, len(infos))
	for _, info := range infos {
		if _, hidden := archived[info.ID]; !hidden {
			filtered = append(filtered, info)
		}
	}
	infos = filtered
	seen := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		seen[info.ID] = struct{}{}
	}
	for _, lane := range states {
		// A merely unreachable lane is still restored to the listing -- sleep
		// and a lost socket must never make work disappear -- but it is
		// restored as unreachable, not as ended. Only a reaped status makes a
		// session ended.
		if !lane.Created || lane.Archived || !(durablyClosed(lane) || lane.RunnerLost) {
			continue
		}
		if durablyClosed(lane) && !includeEnded {
			continue
		}
		if _, exists := seen[lane.LaneID]; exists {
			continue
		}
		exitedAt := lane.ClosedAtMS
		if exitedAt == 0 {
			exitedAt = lane.LastEventAtMS
		}
		info := state.SessionInfo{
			ID: lane.LaneID, Name: lane.Name, Description: lane.Description,
			DescriptionSource: string(lane.DescriptionSource), Cwd: lane.Cwd,
			Profile: lane.Profile, ConfigDir: lane.ConfigDir,
			WorktreePath: lane.WorktreePath, Branch: lane.Branch, Base: lane.Base, SourceRepo: lane.SourceRepo,
			CreatedAt: lane.CreatedAtMS, LastDataAt: lane.LastEventAtMS,
			Kind: lane.Kind, Tool: state.SessionTool(lane.Tool),
			EndedByKind: string(lane.EndInitiatorKind), EndedByID: lane.EndInitiatorID,
			EndedByName: lane.EndInitiatorName, EndedByClient: lane.EndClient,
			EndReason: lane.EndReason, EndOperationID: lane.EndOperationID,
		}
		if durablyClosed(lane) {
			info.Exited = true
			info.ExitedAt = &exitedAt
			info.ExitCode = lane.ExitCode
			info.ExitSignal = lane.ExitSignal
			info.ExitReason = durableExitReason(lane)
		} else {
			// lane.RunnerLost with no reaped status: the daemon lost contact
			// and never observed an ending. Synthesising Exited:true with a
			// nil ExitCode here is what put live sessions in the list as
			// ended-and-failed.
			lostAt := exitedAt
			info.Unreachable = true
			info.UnreachableReason = "runner-lost"
			info.UnreachableSince = &lostAt
		}
		if len(lane.ResumeArgv) > 0 {
			info.Cmd = lane.ResumeArgv[0]
			info.Args = append([]string(nil), lane.ResumeArgv[1:]...)
		}
		info.ConversationID = lane.ProviderUUID
		if info.Tool == state.ToolClaude {
			info.ClaudeSessionID = lane.ProviderUUID
		}
		if lane.Tool == string(state.ToolLane) {
			info.Kind = state.KindLane
		}
		if metadata, metadataErr := state.ReadRunnerMetadata(filepath.Join(m.config.RunnerStateDir, lane.LaneID+".json")); metadataErr == nil {
			info.Tags = state.CloneTags(metadata.Tags)
			if metadata.DisplayParentSessionID != nil {
				displayParent := *metadata.DisplayParentSessionID
				info.DisplayParentSessionID = &displayParent
			}
			if metadata.SetAsideAt != nil {
				setAsideAt := *metadata.SetAsideAt
				info.SetAsideAt = &setAsideAt
			}
			info.Pinned = metadata.Pinned
		}
		infos = append(infos, info)
		seen[lane.LaneID] = struct{}{}
	}
	return infos
}

// durablyClosed reports that a lane reached an end Sessions actually observed:
// the user asked for it, the runner reported a status, the artifacts were
// reaped, or the conversation was continued elsewhere.
//
// RunnerLost is deliberately absent. It records that the daemon could not
// reach a runner, which happens to healthy runners whenever a socket read
// fails or the daemon restarts. ORing it in here meant one lost connection
// permanently closed a session that never stopped working -- and on the
// owner's machine two live runners, probed alive at pids 43014 and 22440,
// carried runner_lost facts and were listed as ended.
func durablyClosed(lane ledger.LaneState) bool {
	return lane.UserKillRequested || lane.RunnerExited || lane.Reaped || lane.ReopenedAs != ""
}

func durableExitReason(lane ledger.LaneState) string {
	switch {
	case lane.UserKillRequested:
		return "ended-by-user"
	case lane.ReopenedAs != "":
		return "continued"
	case lane.RunnerLost:
		return "runner-lost"
	case lane.ExitSignal != nil && *lane.ExitSignal != "":
		return "signaled"
	case lane.ExitCode != nil && *lane.ExitCode != 0:
		return "failed"
	case lane.ExitCode != nil && *lane.ExitCode == 0:
		return "completed"
	case lane.RunnerExited:
		return "ended"
	case lane.Reaped:
		return "reaped"
	default:
		return "ended"
	}
}

func (m *Manager) withProvenance(ctx context.Context, infos []state.SessionInfo) []state.SessionInfo {
	states, err := m.ledgerStates(ctx)
	if err != nil {
		log.Printf("[ledger] read provenance graph: %v", err)
		return infos
	}
	return m.withProvenanceStates(infos, states)
}

func (m *Manager) withProvenanceStates(infos []state.SessionInfo, states []ledger.LaneState) []state.SessionInfo {
	if m.ledgerReader == nil || len(infos) == 0 {
		return infos
	}
	byID := make(map[string]ledger.LaneState, len(states))
	for _, candidate := range states {
		byID[candidate.LaneID] = candidate
	}
	for index := range infos {
		current, exists := byID[infos[index].ID]
		if !exists || !current.Created {
			continue
		}
		if current.Name != "" {
			infos[index].Name = current.Name
		}
		if infos[index].Exited {
			derived := durableExitReason(current)
			if infos[index].ExitReason == "" || (infos[index].ExitReason == "ended" && derived != "ended") {
				infos[index].ExitReason = derived
			}
		}
		if infos[index].DescriptionSource != state.DescriptionExplicit && current.Description != "" {
			infos[index].Description = current.Description
			infos[index].DescriptionSource = string(current.DescriptionSource)
		}
		infos[index].CreatorKind = string(current.CreatorKind)
		infos[index].CreatorID = current.CreatorID
		infos[index].DelegationKind = current.DelegationKind
		infos[index].Profile = current.Profile
		infos[index].ConfigDir = current.ConfigDir
		infos[index].WorktreePath = current.WorktreePath
		infos[index].Branch = current.Branch
		infos[index].Base = current.Base
		infos[index].SourceRepo = current.SourceRepo
		infos[index].ReopenedAs = current.ReopenedAs
		infos[index].MovedToEndpoint = current.MovedToMachine
		infos[index].MovedToSessionID = current.MovedToLaneID
		infos[index].MovedFromEndpoint = current.MovedFromMachine
		infos[index].MovedFromSessionID = current.MovedFromLaneID
		infos[index].EndedByKind = string(current.EndInitiatorKind)
		infos[index].EndedByID = current.EndInitiatorID
		infos[index].EndedByName = current.EndInitiatorName
		infos[index].EndedByClient = current.EndClient
		infos[index].EndReason = current.EndReason
		infos[index].EndOperationID = current.EndOperationID
		if current.CreatorKind == ledger.CreatorSession {
			infos[index].ParentSessionID = current.CreatorID
		}

		visited := map[string]struct{}{current.LaneID: {}}
		parentDead := false
		for current.CreatorKind == ledger.CreatorSession {
			parentID := current.CreatorID
			infos[index].CreatorAncestry = append(infos[index].CreatorAncestry, parentID)
			if _, cycle := visited[parentID]; cycle {
				infos[index].ProvenanceStatus = "cycle"
				break
			}
			visited[parentID] = struct{}{}
			parent, found := byID[parentID]
			if !found || !parent.Created {
				infos[index].RootCreatorKind = string(ledger.CreatorSession)
				infos[index].RootCreatorID = parentID
				infos[index].ProvenanceStatus = "parent-missing"
				break
			}
			if provenanceParentDead(parent) {
				parentDead = true
			}
			current = parent
		}
		if infos[index].RootCreatorKind == "" && current.CreatorKind != "" {
			infos[index].RootCreatorKind = string(current.CreatorKind)
			infos[index].RootCreatorID = current.CreatorID
		}
		if parentDead {
			infos[index].ProvenanceStatus = "parent-dead"
		} else if infos[index].ProvenanceStatus == "" {
			if len(infos[index].CreatorAncestry) > 0 {
				infos[index].ProvenanceStatus = "parent-live"
			} else if current.CreatorKind != "" {
				infos[index].ProvenanceStatus = "rooted"
			} else {
				infos[index].ProvenanceStatus = "legacy"
			}
		}
	}
	for sourceID, current := range byID {
		if current.ReopenedAs == "" {
			continue
		}
		for index := range infos {
			if infos[index].ID == current.ReopenedAs {
				infos[index].ResumedFrom = sourceID
				break
			}
		}
	}
	return infos
}

// provenanceParentDead answers whether a child's parent actually ended. Like
// durablyClosed it excludes RunnerLost: a parent the daemon briefly could not
// reach is not a dead parent, and labelling a child "parent-dead" on that
// basis is the same inference wearing a different name.
func provenanceParentDead(parent ledger.LaneState) bool {
	return parent.UserKillRequested || parent.RunnerExited || parent.Reaped
}

func (m *Manager) recordDaemonRestart(ctx context.Context) {
	lanes, err := m.ledgerStates(ctx)
	if err != nil {
		log.Printf("[ledger] read lanes for daemon restart: %v", err)
		return
	}
	for _, lane := range lanes {
		laneID := lane.LaneID
		m.observe(ctx, "daemon restart", func(writer ledger.ObservationWriter) error {
			return writer.RecordDaemonRestart(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: laneID}})
		})
	}
}

func (m *Manager) reconcileLedger(ctx context.Context) {
	lanes, err := m.ledgerStates(ctx)
	if err != nil {
		log.Printf("[ledger] read lanes for discovery reconciliation: %v", err)
		return
	}
	for _, lane := range lanes {
		closed := lane.UserKillRequested || lane.RunnerExited || lane.Reaped
		if !lane.Created || closed || lane.RunnerLost {
			continue
		}
		if _, present := m.registry.Get(lane.LaneID); present {
			continue
		}
		// Absence from the in-memory map is not death. The map holds the
		// runners this daemon process currently has sockets to; a runner that
		// outlived a daemon restart, or whose socket blipped before discovery
		// reattached it, is missing from it while its process runs perfectly
		// well. Recording a loss on that basis wrote a permanent ledger fact
		// about a live session. Ask the process.
		metadata, metadataErr := state.ReadRunnerMetadata(
			filepath.Join(m.config.RunnerStateDir, lane.LaneID+".json"))
		switch {
		case metadataErr == nil:
			metadata.Info.ID = lane.LaneID
			if m.runnerAlive(lane.LaneID, metadata.Info) {
				log.Printf(
					"[ledger] lane %s is absent from the runtime map but pid %d is alive — recording no loss",
					lane.LaneID, metadata.Info.PID)
				continue
			}
		case !errors.Is(metadataErr, os.ErrNotExist):
			// A torn, unreadable, or forward-version metadata document is
			// absence of evidence. Say nothing and retry on the next pass.
			log.Printf("[ledger] lane %s metadata unreadable — recording no loss: %v", lane.LaneID, metadataErr)
			continue
		}
		laneID := lane.LaneID
		m.observe(ctx, "runner lost during discovery reconciliation", func(writer ledger.ObservationWriter) error {
			return writer.RecordRunnerLost(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: laneID}})
		})
		info := state.SessionInfo{ID: lane.LaneID, Name: lane.Name, Cwd: lane.Cwd, Tool: state.SessionTool(lane.Tool)}
		if lane.Tool == string(state.ToolLane) {
			info.Kind = state.KindLane
		}
		if len(lane.ResumeArgv) > 0 {
			info.Cmd = lane.ResumeArgv[0]
		}
		m.notifyLost(info)
	}
}

func (m *Manager) Create(ctx context.Context, request state.CreateSessionRequest) (state.SessionInfo, error) {
	if request.Profile != "" {
		configDir, err := m.prepareProfile(request.Cmd, request.Profile)
		if err != nil {
			return state.SessionInfo{}, err
		}
		request.ConfigDir = configDir
	}
	resolvedRuntimeRequest, err := resolveDelegatedRuntimeDefault(request)
	if err != nil {
		return state.SessionInfo{}, err
	}
	request = resolvedRuntimeRequest
	resolvedClaudeRequest, err := m.applyClaudeDefaults(request)
	if err != nil {
		return state.SessionInfo{}, err
	}
	request = resolvedClaudeRequest
	resolvedRequest, err := m.resolveCodexModelChoice(ctx, request)
	if err != nil {
		return state.SessionInfo{}, err
	}
	request = resolvedRequest

	// Serialize the ledger query with the pre-launch binding write. Without
	// this lock two concurrent resume requests could both observe no owner.
	m.bindMu.Lock()
	defer m.bindMu.Unlock()

	creatorKind, creatorID, err := m.resolveCreator(ctx, request)
	if err != nil {
		return state.SessionInfo{}, fmt.Errorf("resolve lane creator: %w", err)
	}
	if creatorKind == ledger.CreatorSession {
		if request.DelegationKind == "" {
			request.DelegationKind = "agent"
		}
		if request.DelegationKind != "user" && request.DelegationKind != "agent" {
			return state.SessionInfo{}, errors.New("delegation kind must be user or agent")
		}
	} else if request.DelegationKind != "" {
		return state.SessionInfo{}, errors.New("delegation kind requires a parent session")
	}
	request, err = m.resolveDelegatedExecution(ctx, request, creatorKind, creatorID)
	if err != nil {
		return state.SessionInfo{}, fmt.Errorf("resolve delegated execution: %w", err)
	}

	providerUUID, _ := ledger.ExistingProviderResume(request.Cmd, request.Args)
	var takeover *ledger.LiveBinding
	if providerUUID != "" && m.ledgerReader != nil {
		binding, err := m.ledgerReader.LiveBindingFor(ctx, providerUUID)
		if err != nil {
			return state.SessionInfo{}, fmt.Errorf("check live conversation binding: %w", err)
		}
		moved, err := m.ledgerReader.MovedBinding(ctx, providerUUID)
		if err != nil {
			return state.SessionInfo{}, fmt.Errorf("check moved conversation binding: %w", err)
		}
		if moved != nil && !request.Force {
			return state.SessionInfo{}, &ConversationMovedError{ProviderUUID: providerUUID, Machine: moved.Machine}
		}
		if binding != nil && !request.Force {
			return state.SessionInfo{}, &ConversationLiveError{ProviderUUID: providerUUID, Binding: *binding}
		}
		if binding != nil {
			takeover = binding
		} else if moved != nil {
			// A moved source can be tombstoned locally while its remote driver
			// remains live. Record the forced fork against that source lane.
			takeover = &ledger.LiveBinding{SessionID: moved.SourceSessionID}
		}
	}

	var preparedWorktree *createdWorktree
	if request.Worktree && request.WorktreeDefaulted && (m.boundaries == nil || m.ledgerReader == nil) {
		// No ledger means no worktree bookkeeping; a defaulted lane simply
		// shares its folder rather than failing to start.
		request.Worktree = false
		request.WorktreeDefaulted = false
	}
	if request.Worktree {
		if m.boundaries == nil || m.ledgerReader == nil {
			return state.SessionInfo{}, errors.New("--worktree requires the Sessions ledger, but ledger access is unavailable; restore the daemon ledger and retry")
		}
		sourceCwd := request.Cwd
		if strings.TrimSpace(sourceCwd) == "" {
			sourceCwd = m.config.DefaultCwd
		}
		worktree, err := createGitWorktree(ctx, sourceCwd, request.Name, request.Base)
		if err != nil && request.WorktreeDefaulted {
			// The lane was going to get a worktree by default, but this folder
			// cannot host one (not a Git checkout, bare, shallow, or detached).
			// Sharing the manager's folder is the pre-worktree behavior and is
			// never a failure; the reason is logged so the choice is visible.
			log.Printf("[worktree] lane %q shares %s instead of a worktree: %v", request.Name, sourceCwd, err)
			request.Worktree = false
			request.WorktreeDefaulted = false
			request.Base = ""
		} else if err != nil {
			return state.SessionInfo{}, err
		} else {
			request.Cwd = worktree.Path
			request.WorktreePath = worktree.Path
			request.WorktreeBranch = worktree.Branch
			request.WorktreeBase = worktree.Base
			request.SourceRepo = worktree.SourceRepo
			preparedWorktree = &worktree
		}
	} else if strings.TrimSpace(request.Base) != "" {
		return state.SessionInfo{}, errors.New("--base requires --worktree")
	}

	creationRecorded := false
	beforeLaunch := func(ctx context.Context, prepared state.PreparedSession) error {
		if err := m.recordCreated(ctx, prepared, creatorKind, creatorID); err != nil {
			return err
		}
		creationRecorded = true
		if takeover == nil {
			return nil
		}
		if m.boundaries == nil {
			return errors.New("record forced conversation takeover before launch: ledger writer is unavailable")
		}
		if err := m.boundaries.RecordProviderRebound(ctx, ledger.ProviderRebound{
			Meta: ledger.Meta{LaneID: takeover.SessionID}, ProviderUUID: providerUUID, NewLaneID: prepared.Info.ID,
		}); err != nil {
			return fmt.Errorf("record forced conversation takeover before launch: %w", err)
		}
		return nil
	}
	info, err := m.registry.CreateWithLifecycle(ctx, request, state.CreateLifecycle{
		BeforeLaunch:  beforeLaunch,
		LaunchStarted: m.recordLaunchStarted,
		RunnerReady:   m.recordRunnerReady,
	})
	if err != nil {
		if preparedWorktree != nil && !creationRecorded {
			if rollbackErr := rollbackCreatedGitWorktree(ctx, *preparedWorktree); rollbackErr != nil {
				return state.SessionInfo{}, fmt.Errorf("%w; new worktree was preserved at %s because safe rollback was refused: %v",
					err, preparedWorktree.Path, rollbackErr)
			}
		}
		return state.SessionInfo{}, err
	}
	session, ok := m.registry.Get(info.ID)
	if !ok {
		return state.SessionInfo{}, fmt.Errorf("created session %s was not registered", info.ID)
	}
	runtime := m.manage(session)
	runtime.expectProviderInput(request.InitialInput)
	if strings.TrimSpace(request.InitialInput) != "" {
		m.captureFirstMessageDescription(info.ID, request.InitialInput)
		m.captureFirstMessageDescription(info.ID, "\r")
	}
	if request.WaitReady {
		m.waitReady(ctx, runtime)
	}
	return m.withProvenance(ctx, []state.SessionInfo{session.Info()})[0], nil
}
