package api

import "github.com/somewhere-tech/sessions/runtime/internal/state"

func teamNeedsInput(info state.SessionInfo) bool {
	if info.Exited || info.RunnerGone || info.Unreachable {
		return false
	}
	if info.PendingApproval != nil || info.IdleReason == state.IdleReasonNeedsInput {
		return true
	}
	if info.FailureKind != "" && info.Retry == nil {
		return true
	}
	if info.Start != nil {
		return info.Start.Phase == state.StartPhasePromptNotDelivered || info.Start.Phase == state.StartPhasePromptUnknown ||
			(info.Start.Prompt != nil && info.Start.Prompt.Status == state.StartPromptNotSent)
	}
	return false
}

// Project only this team's start receipts. Listing the fleet must not force a
// coordinator to read every other session's receipts or provider history.
func (s *Server) attachTeamStarts(listing *teamListing, infos []state.SessionInfo) {
	byID := make(map[string]state.SessionInfo, len(infos))
	for _, info := range infos {
		byID[info.ID] = info
	}
	listing.NeedsInput = 0
	for i := range listing.Members {
		member := &listing.Members[i]
		info := s.withStartReceipt(byID[member.ID])
		member.Start = info.Start
		member.NeedsYou = teamNeedsInput(info)
		if member.NeedsYou {
			listing.NeedsInput++
		}
		if info.FailureKind != "" {
			member.Waiting = truncateBudget(info.FailureDetail, teamSummaryBudget)
		}
		if member.Start == nil {
			continue
		}
		member.Start.Evidence = truncateBudget(member.Start.Evidence, teamSummaryBudget)
		if prompt := member.Start.Prompt; prompt != nil {
			prompt.Reason = truncateBudget(prompt.Reason, teamSummaryBudget)
		}
		if recovery := member.Start.Recovery; recovery != nil {
			recovery.Detail = truncateBudget(recovery.Detail, teamSummaryBudget)
			if member.Recovery == "" {
				member.Recovery = recovery.Command
			}
		}
	}
}
