package recovery

import (
	"context"
	"fmt"
	"sort"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type SessionCreator interface {
	Create(context.Context, state.CreateSessionRequest) (state.SessionInfo, error)
}

type ReopenStatus string

const (
	ReopenCreated ReopenStatus = "reopened"
	ReopenSkipped ReopenStatus = "skipped-live-provider"
	ReopenBlocked ReopenStatus = "blocked"
	ReopenFailed  ReopenStatus = "failed"
)

type ReopenOutcome struct {
	SourceLaneID string       `json:"sourceLaneId"`
	Name         string       `json:"name,omitempty"`
	ProviderUUID string       `json:"providerUuid,omitempty"`
	Status       ReopenStatus `json:"status"`
	NewLaneID    string       `json:"newLaneId,omitempty"`
	Error        string       `json:"error,omitempty"`
}

type ReopenResult struct {
	OK       bool            `json:"ok"`
	Outcomes []ReopenOutcome `json:"outcomes"`
}

type ReopenOptions struct {
	Force bool
}

// Reopen creates at most one live lane per provider UUID. Every lost lane is
// represented in the result, including provider-unbound and missing-source
// refusals, so callers never silently omit an unsafe recovery candidate.
func Reopen(ctx context.Context, report Report, creator SessionCreator, observations ledger.ObservationWriter, options ...ReopenOptions) ReopenResult {
	selected := ReopenOptions{}
	if len(options) > 0 {
		selected = options[0]
	}
	result := ReopenResult{OK: true, Outcomes: make([]ReopenOutcome, 0)}
	createdProviders := make(map[string]string)
	recipes := make(map[string]ledger.RecoveryRecipe, len(report.Plan.Recipes))
	for _, recipe := range report.Plan.Recipes {
		recipes[recipe.SourceLaneID] = recipe
	}
	lost := make([]Lane, 0)
	for _, lane := range report.Lanes {
		if lane.Class == ledger.ClassUnexpectedlyLost {
			lost = append(lost, lane)
		}
	}
	sort.SliceStable(lost, func(i, j int) bool {
		if lost[i].LastActivityAtMS != lost[j].LastActivityAtMS {
			return lost[i].LastActivityAtMS > lost[j].LastActivityAtMS
		}
		if lost[i].LastActivitySource != lost[j].LastActivitySource {
			return lost[i].LastActivitySource == ledger.ActivityHumanInput
		}
		return lost[i].ID < lost[j].ID
	})
	for _, lane := range lost {
		outcome := ReopenOutcome{SourceLaneID: lane.ID, Name: lane.Name, ProviderUUID: lane.ProviderUUID}
		recipe, safe := recipes[lane.ID]
		if lane.ProviderUUID == "" {
			outcome.Status = ReopenBlocked
			outcome.Error = string(ledger.AnomalyProviderUnbound)
			result.OK = false
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}
		if !safe || recipe.Cmd == "" {
			outcome.Status = ReopenBlocked
			outcome.Error = "no-safe-resume-recipe"
			result.OK = false
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}
		if recipe.Blocked {
			outcome.Status = ReopenBlocked
			outcome.Error = string(ledger.AnomalyResumeSourceMissing)
			result.OK = false
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}
		if liveID := createdProviders[lane.ProviderUUID]; liveID != "" {
			outcome.Status = ReopenSkipped
			outcome.NewLaneID = liveID
			if observations != nil && lane.ReopenedAs == "" {
				if err := observations.RecordReopened(ctx, ledger.Reopened{
					Meta: ledger.Meta{LaneID: lane.ID}, NewLaneID: liveID,
				}); err != nil {
					outcome.Status = ReopenFailed
					outcome.Error = err.Error()
					result.OK = false
				}
			}
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}
		created, err := creator.Create(ctx, state.CreateSessionRequest{
			Cmd: recipe.Cmd, Args: append([]string(nil), recipe.Args...),
			Cwd: recipe.Cwd, Name: recipe.Name, Profile: lane.Profile, Force: selected.Force,
		})
		if err != nil {
			outcome.Status = ReopenFailed
			outcome.Error = err.Error()
			result.OK = false
			result.Outcomes = append(result.Outcomes, outcome)
			continue
		}
		outcome.Status = ReopenCreated
		outcome.NewLaneID = created.ID
		createdProviders[lane.ProviderUUID] = created.ID
		if observations != nil {
			if err := observations.RecordReopened(ctx, ledger.Reopened{
				Meta: ledger.Meta{LaneID: lane.ID}, NewLaneID: created.ID,
			}); err != nil {
				outcome.Status = ReopenFailed
				outcome.Error = fmt.Sprintf("lane reopened as %s but ledger annotation failed: %v", created.ID, err)
				result.OK = false
			}
		}
		result.Outcomes = append(result.Outcomes, outcome)
	}
	return result
}
