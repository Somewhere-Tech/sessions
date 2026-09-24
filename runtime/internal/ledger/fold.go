package ledger

import (
	"sort"
)

type LaneState struct {
	LaneID                   string
	Name                     string
	Description              string
	DescriptionSource        DescriptionSource
	Kind                     string
	Tool                     string
	Cwd                      string
	Profile                  string
	ConfigDir                string
	WorktreePath             string
	Branch                   string
	Base                     string
	SourceRepo               string
	ResumeArgv               []string
	ProviderUUID             string
	CreatorKind              CreatorKind
	CreatorID                string
	DelegationKind           string
	StartOperationID         string
	PromptOperationID        string
	CreatedAtMS              int64
	LastEventAtMS            int64
	LastActivityAtMS         int64
	LastHumanInputAtMS       int64
	LastProviderActivityAtMS int64
	LastActivitySource       ActivitySource
	LatestEvent              EventType
	ClosedAtMS               int64
	ArchivedAtMS             int64
	ExitCode                 *int
	ExitSignal               *string
	EndInitiatorKind         CreatorKind
	EndInitiatorID           string
	EndInitiatorName         string
	EndClient                string
	EndReason                string
	EndOperationID           string

	Created                 bool
	LaunchStarted           bool
	RunnerReady             bool
	ProviderBound           bool
	Attached                bool
	ManagedActive           bool
	UserKillRequested       bool
	RunnerExited            bool
	RunnerLost              bool
	Reaped                  bool
	ReopenedAs              string
	Archived                bool
	ProviderReboundAs       string
	MovedToMachine          string
	MovedToLaneID           string
	MovedToSeq              int64
	MovedFromMachine        string
	MovedFromLaneID         string
	MovedFromSeq            int64
	WorktreeCleanRequested  bool
	WorktreeCleanBranchHead string
	WorktreeCleaned         bool
	WorktreeCleanedAtMS     int64
	WorktreeBranchRemoved   bool
}

// Fold reduces an event stream in seq order. Input order is irrelevant as
// long as seq values are the unique database sequence numbers.
func Fold(events []Event) []LaneState {
	ordered := append([]Event(nil), events...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Seq != ordered[j].Seq {
			return ordered[i].Seq < ordered[j].Seq
		}
		return ordered[i].EventID < ordered[j].EventID
	})
	states := make(map[string]*LaneState)
	for _, event := range ordered {
		if event.LaneID == "" {
			continue
		}
		state := states[event.LaneID]
		if state == nil {
			state = &LaneState{LaneID: event.LaneID}
			states[event.LaneID] = state
		}
		applyLaneEvent(state, event)
	}
	return snapshotLaneStates(states)
}

func cloneLaneState(state LaneState) LaneState {
	state.ResumeArgv = append([]string(nil), state.ResumeArgv...)
	if state.ExitCode != nil {
		code := *state.ExitCode
		state.ExitCode = &code
	}
	if state.ExitSignal != nil {
		signal := *state.ExitSignal
		state.ExitSignal = &signal
	}
	return state
}

func snapshotLaneStates(states map[string]*LaneState) []LaneState {
	result := make([]LaneState, 0, len(states))
	for _, state := range states {
		result = append(result, cloneLaneState(*state))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].LaneID < result[j].LaneID })
	return result
}

func mayBecomeManaged(state *LaneState) bool {
	return !state.UserKillRequested && !state.RunnerExited && !state.Reaped && state.ReopenedAs == ""
}

type Class string

const (
	ClassLiveManaged      Class = "live-managed"
	ClassClosed           Class = "closed"
	ClassUnexpectedlyLost Class = "unexpectedly-lost"
	ClassExternal         Class = "external"
)

type Anomaly string

const (
	AnomalyClosedButRunning    Anomaly = "closed-but-running"
	AnomalyNeverBecameReady    Anomaly = "never-became-ready"
	AnomalyResumeSourceMissing Anomaly = "resume-source-missing"
	AnomalyProviderUnbound     Anomaly = "provider-unbound"
)

type RuntimeState struct {
	Running            bool
	ResumeSourceKnown  bool
	ResumeSourceExists bool
	// TranscriptMirrorUsable reports that Sessions holds its own readable copy
	// of the conversation. It is deliberately separate from
	// ResumeSourceExists, which means only that the provider's file is still
	// there: a mirror makes the conversation recoverable, it does not make a
	// native provider resume possible.
	TranscriptMirrorUsable bool
}

type Classification struct {
	Lane      LaneState
	Class     Class
	Anomalies []Anomaly
	// TranscriptMirrorUsable carries the runtime observation forward so
	// BuildRecoveryPlan, which sees only classifications, can tell a
	// conversation that is gone from one Sessions can still read.
	TranscriptMirrorUsable bool
}

func ClassifyLane(lane LaneState, runtime RuntimeState) Classification {
	classification := Classification{Lane: lane, TranscriptMirrorUsable: runtime.TranscriptMirrorUsable}
	closed := lane.UserKillRequested || lane.RunnerExited || lane.Reaped || lane.ReopenedAs != "" || lane.Archived
	switch {
	case closed:
		classification.Class = ClassClosed
	case !lane.Created && runtime.Running:
		classification.Class = ClassExternal
	case lane.Created && runtime.Running:
		classification.Class = ClassLiveManaged
	case lane.Created:
		classification.Class = ClassUnexpectedlyLost
	default:
		classification.Class = ClassClosed
	}
	if closed && runtime.Running {
		classification.Anomalies = append(classification.Anomalies, AnomalyClosedButRunning)
	}
	if lane.Created && !lane.RunnerReady && !lane.Attached {
		classification.Anomalies = append(classification.Anomalies, AnomalyNeverBecameReady)
	}
	if lane.Created && isProviderTool(lane.Tool) && lane.ProviderUUID == "" {
		classification.Anomalies = append(classification.Anomalies, AnomalyProviderUnbound)
	}
	if classification.Class == ClassUnexpectedlyLost && lane.ProviderUUID != "" &&
		runtime.ResumeSourceKnown && !runtime.ResumeSourceExists {
		classification.Anomalies = append(classification.Anomalies, AnomalyResumeSourceMissing)
	}
	return classification
}

// ClassifyAll includes runtime-only lanes as external and returns lane-id
// order, making the result stable across map iteration and daemon restarts.
func ClassifyAll(lanes []LaneState, runtime map[string]RuntimeState) []Classification {
	states := make(map[string]LaneState, len(lanes))
	for _, lane := range lanes {
		states[lane.LaneID] = lane
	}
	for laneID, observed := range runtime {
		if _, exists := states[laneID]; !exists && observed.Running {
			states[laneID] = LaneState{LaneID: laneID}
		}
	}
	result := make([]Classification, 0, len(states))
	for laneID, lane := range states {
		result = append(result, ClassifyLane(lane, runtime[laneID]))
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Lane.LaneID < result[j].Lane.LaneID })
	return result
}

func isProviderTool(tool string) bool { return tool == "claude-code" || tool == "codex" }

func HasAnomaly(classification Classification, anomaly Anomaly) bool {
	for _, candidate := range classification.Anomalies {
		if candidate == anomaly {
			return true
		}
	}
	return false
}

type RecoveryRecipe struct {
	SourceLaneID       string         `json:"sourceLaneId"`
	Name               string         `json:"name,omitempty"`
	Tool               string         `json:"tool"`
	Cwd                string         `json:"cwd"`
	Cmd                string         `json:"cmd"`
	Args               []string       `json:"args"`
	ProviderUUID       string         `json:"providerUuid"`
	LastActivityAtMS   int64          `json:"lastActivityAtMs"`
	LastActivitySource ActivitySource `json:"lastActivitySource,omitempty"`
	Blocked            bool           `json:"blocked"`
	// TranscriptRecovery reports that the provider's own transcript is gone
	// but Sessions kept a copy, so this recipe must be recovered from that
	// copy rather than by handing the provider a resume flag it will reject.
	TranscriptRecovery bool      `json:"transcriptRecovery,omitempty"`
	Anomalies          []Anomaly `json:"anomalies"`
}

type RecoveryPlan struct {
	Recipes []RecoveryRecipe `json:"recipes"`
}

// BuildRecoveryPlan emits only create-with-resume commands. Lost lanes whose
// provider never bound have no safe recipe and are intentionally omitted.
// Missing on-disk resume sources remain visible as blocked recipes so callers
// can explain the loss without accidentally launching them.
func BuildRecoveryPlan(classifications []Classification) RecoveryPlan {
	plan := RecoveryPlan{Recipes: make([]RecoveryRecipe, 0)}
	for _, classification := range classifications {
		lane := classification.Lane
		if classification.Class != ClassUnexpectedlyLost || len(lane.ResumeArgv) == 0 {
			continue
		}
		recipe := RecoveryRecipe{
			SourceLaneID:       lane.LaneID,
			Name:               lane.Name,
			Tool:               lane.Tool,
			Cwd:                lane.Cwd,
			Cmd:                lane.ResumeArgv[0],
			Args:               append([]string(nil), lane.ResumeArgv[1:]...),
			ProviderUUID:       lane.ProviderUUID,
			LastActivityAtMS:   lane.LastActivityAtMS,
			LastActivitySource: lane.LastActivitySource,
			Anomalies:          append([]Anomaly(nil), classification.Anomalies...),
		}
		// The anomaly still stands -- the provider's transcript really is
		// missing, and a caller that hands `claude --resume` this id will
		// still be refused. What changes is that a conversation Sessions kept
		// its own copy of is no longer presented as unrecoverable, which is
		// the whole reason the copy exists.
		missingSource := HasAnomaly(classification, AnomalyResumeSourceMissing)
		recipe.TranscriptRecovery = missingSource && classification.TranscriptMirrorUsable
		recipe.Blocked = missingSource && !classification.TranscriptMirrorUsable
		plan.Recipes = append(plan.Recipes, recipe)
	}
	sort.Slice(plan.Recipes, func(i, j int) bool {
		if plan.Recipes[i].LastActivityAtMS != plan.Recipes[j].LastActivityAtMS {
			return plan.Recipes[i].LastActivityAtMS > plan.Recipes[j].LastActivityAtMS
		}
		if plan.Recipes[i].LastActivitySource != plan.Recipes[j].LastActivitySource {
			return plan.Recipes[i].LastActivitySource == ActivityHumanInput
		}
		return plan.Recipes[i].SourceLaneID < plan.Recipes[j].SourceLaneID
	})
	return plan
}
