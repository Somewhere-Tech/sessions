package ledger

import "encoding/json"

// applyLaneEvent applies one event in committed sequence order. Fold and the
// incremental Store projection share exactly the same state transitions.
func applyLaneEvent(state *LaneState, event Event) {
	if event.AtMS > state.LastEventAtMS {
		state.LastEventAtMS = event.AtMS
	}
	state.LatestEvent = event.Type
	switch event.Type {
	case EventCreated:
		applyCreatedEvent(state, event)
	case EventProviderBound, EventProviderRebound, EventMovedTo, EventMovedFrom:
		applyProviderIdentityEvent(state, event)
	case EventLaunchStarted, EventRunnerReady, EventAttached:
		applyRunnerPresenceEvent(state, event)
	case EventActivity:
		applyActivityEvent(state, event)
	case EventRenamed, EventDescriptionDerived, EventWorktreeCleanRequested, EventWorktreeCleaned:
		applyMetadataEvent(state, event)
	case EventUserKillRequested, EventRunnerExited, EventRunnerLost, EventReaped, EventReopened, EventArchived:
		applyClosureEvent(state, event)
	}
}

func applyCreatedEvent(state *LaneState, event Event) {
	switch event.Type {
	case EventCreated:
		if state.Created {
			return
		}
		var payload createdPayload
		if json.Unmarshal(event.Payload, &payload) != nil {
			return
		}
		state.Created = true
		state.CreatedAtMS = event.AtMS
		state.Name = payload.Name
		state.Description = payload.Description
		state.DescriptionSource = payload.DescriptionSource
		state.Kind = payload.Kind
		state.Tool = payload.Tool
		state.Cwd = payload.Cwd
		state.Profile = payload.Profile
		state.ConfigDir = payload.ConfigDir
		state.WorktreePath = payload.WorktreePath
		state.Branch = payload.Branch
		state.Base = payload.Base
		state.SourceRepo = payload.SourceRepo
		state.ResumeArgv = append([]string(nil), payload.ResumeArgv...)
		state.ProviderUUID = payload.ProviderUUID
		state.CreatorKind = payload.CreatorKind
		state.CreatorID = payload.CreatorID
		state.DelegationKind = payload.DelegationKind
	}
}

func applyProviderIdentityEvent(state *LaneState, event Event) {
	switch event.Type {
	case EventProviderBound:
		var payload providerPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			state.ProviderBound = true
			state.ProviderUUID = payload.ProviderUUID
			state.ResumeArgv = append([]string(nil), payload.ResumeArgv...)
		}
	case EventProviderRebound:
		var payload providerReboundPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.ProviderUUID == state.ProviderUUID {
			state.ProviderReboundAs = payload.NewLaneID
		}
	case EventMovedTo:
		var payload movedToPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			state.MovedToMachine = payload.TargetEndpoint
			state.MovedToLaneID = payload.NewLaneID
			state.MovedToSeq = event.Seq
		}
	case EventMovedFrom:
		var payload movedFromPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			state.MovedFromMachine = payload.SourceEndpoint
			state.MovedFromLaneID = payload.SourceLaneID
			state.MovedFromSeq = event.Seq
		}
	}
}

func applyRunnerPresenceEvent(state *LaneState, event Event) {
	switch event.Type {
	case EventLaunchStarted:
		state.LaunchStarted = true
	case EventRunnerReady:
		state.RunnerReady = true
		if state.RunnerLost && !state.UserKillRequested && !state.RunnerExited && !state.Reaped && state.ReopenedAs == "" {
			state.ClosedAtMS = 0
		}
		state.RunnerLost = false
		state.ManagedActive = mayBecomeManaged(state)
	case EventAttached:
		state.Attached = true
		if state.RunnerLost && !state.UserKillRequested && !state.RunnerExited && !state.Reaped && state.ReopenedAs == "" {
			state.ClosedAtMS = 0
		}
		state.RunnerLost = false
		state.ManagedActive = mayBecomeManaged(state)
	}
}

func applyActivityEvent(state *LaneState, event Event) {
	switch event.Type {
	case EventActivity:
		var payload activityPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			valid := true
			switch payload.Source {
			case ActivityHumanInput:
				if event.AtMS > state.LastHumanInputAtMS {
					state.LastHumanInputAtMS = event.AtMS
				}
			case ActivitySessionInput:
				// Relayed input is activity, but it is not direct human
				// input and must not alter human-input attribution.
			case ActivityProviderEvent:
				if event.AtMS > state.LastProviderActivityAtMS {
					state.LastProviderActivityAtMS = event.AtMS
				}
			default:
				valid = false
			}
			if valid && (event.AtMS > state.LastActivityAtMS ||
				(event.AtMS == state.LastActivityAtMS && payload.Source == ActivityHumanInput)) {
				state.LastActivityAtMS = event.AtMS
				state.LastActivitySource = payload.Source
			}
		}
	}
}

func applyMetadataEvent(state *LaneState, event Event) {
	switch event.Type {
	case EventRenamed:
		var payload renamePayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			state.Name = payload.Name
		}
	case EventDescriptionDerived:
		var payload descriptionPayload
		if json.Unmarshal(event.Payload, &payload) == nil &&
			payload.Source == DescriptionFirstMessage &&
			state.DescriptionSource == "" && state.Description == "" {
			state.Description = payload.Description
			state.DescriptionSource = payload.Source
		}
	case EventWorktreeCleanRequested:
		var payload worktreeCleanRequestedPayload
		if json.Unmarshal(event.Payload, &payload) == nil &&
			payload.WorktreePath == state.WorktreePath && payload.Branch == state.Branch &&
			payload.BranchHead != "" {
			state.WorktreeCleanRequested = true
			state.WorktreeCleanBranchHead = payload.BranchHead
		}
	case EventWorktreeCleaned:
		var payload worktreeCleanedPayload
		if json.Unmarshal(event.Payload, &payload) == nil &&
			payload.WorktreePath == state.WorktreePath && payload.Branch == state.Branch {
			state.WorktreeCleaned = true
			state.WorktreeCleanedAtMS = event.AtMS
			state.WorktreeBranchRemoved = payload.BranchRemoved
		}
	}
}

func applyClosureEvent(state *LaneState, event Event) {
	switch event.Type {
	case EventUserKillRequested:
		// This bit is monotonic. No later observation, including reopened,
		// can turn a tombstoned lane into a recovery candidate. The first
		// committed request is also the authoritative initiator: retries
		// or competing requests must not rewrite who ended the session.
		if !state.UserKillRequested {
			state.UserKillRequested = true
			var payload userKillPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				state.EndInitiatorKind = payload.InitiatorKind
				state.EndInitiatorID = payload.InitiatorID
				state.EndInitiatorName = payload.InitiatorName
				state.EndClient = payload.Client
				state.EndReason = payload.Reason
				state.EndOperationID = payload.OperationID
			}
			if state.ClosedAtMS == 0 {
				state.ClosedAtMS = event.AtMS
			}
		}
		state.ManagedActive = false
	case EventRunnerExited:
		state.RunnerExited = true
		state.ManagedActive = false
		if state.ClosedAtMS == 0 {
			state.ClosedAtMS = event.AtMS
		}
		var payload runnerExitPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			state.ExitCode = payload.Code
			state.ExitSignal = payload.Signal
		}
	case EventRunnerLost:
		state.RunnerLost = true
		state.ManagedActive = false
		if state.ClosedAtMS == 0 {
			state.ClosedAtMS = event.AtMS
		}
	case EventReaped:
		state.Reaped = true
		state.ManagedActive = false
		if state.ClosedAtMS == 0 {
			state.ClosedAtMS = event.AtMS
		}
	case EventReopened:
		var payload reopenedPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			state.ReopenedAs = payload.NewLaneID
			state.ManagedActive = false
			if state.ClosedAtMS == 0 {
				state.ClosedAtMS = event.AtMS
			}
		}
	case EventArchived:
		state.Archived = true
		state.ArchivedAtMS = event.AtMS
		state.ManagedActive = false
	}
}
