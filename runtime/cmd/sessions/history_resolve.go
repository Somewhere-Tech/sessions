package main

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

type historyResolution struct {
	Reference string
	Alias     string
	Machine   string
	Session   integrations.HistorySession
}

type historyListOutcome struct {
	target  fleetTarget
	history integrations.HistoryResponse
	err     error
}

// resolveHistoryReference lets humans and agents use the durable Sessions
// title they can see instead of first discovering an opaque provider id. With
// no explicit target it asks every approved machine and returns a qualified
// reference. Ambiguity is always reported rather than guessed.
func (a *app) resolveHistoryReference(value string) (historyResolution, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return historyResolution{}, fail(1, "conversation name or id is required")
	}
	if alias, id, qualified := splitQualifiedHistoryReference(value); qualified {
		return historyResolution{Reference: value, Alias: alias, Session: integrations.HistorySession{ID: id}}, nil
	}
	// Explicit endpoint callers already chose the machine. Preserve the old
	// zero-round-trip path for canonical ids while still resolving friendly
	// names through that daemon.
	if a.explicitTarget && looksLikeHistoryID(value) {
		return historyResolution{Reference: value, Alias: "local", Session: integrations.HistorySession{ID: value}}, nil
	}

	targets := []fleetTarget{{Alias: "local", Name: "This machine", Endpoint: "local", Client: a.api}}
	if !a.explicitTarget {
		var err error
		targets, err = a.approvedFleetTargets()
		if err != nil {
			return historyResolution{}, fail(2, "read approved machines: %s", err)
		}
	}
	outcomes := make([]historyListOutcome, len(targets))
	var wait sync.WaitGroup
	for index := range targets {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			outcomes[index].target = targets[index]
			outcomes[index].err = getJSONFromClient(
				targets[index].Client, "/api/history?summary=true",
				&outcomes[index].history, fleetRequestTimeout,
			)
		}(index)
	}
	wait.Wait()
	for _, target := range targets {
		if target.Owned {
			target.Client.close()
		}
	}

	type candidate struct {
		historyResolution
		rank int
	}
	candidates := make([]candidate, 0)
	successes := 0
	for _, outcome := range outcomes {
		if outcome.err != nil {
			continue
		}
		successes++
		for _, session := range outcome.history.Sessions {
			rank := 0
			switch {
			case session.ID == value:
				rank = 3
			case strings.EqualFold(strings.TrimSpace(session.Name), value):
				rank = 2
			case strings.HasPrefix(strings.ToLower(session.ID), strings.ToLower(value)):
				rank = 1
			}
			if rank == 0 {
				continue
			}
			reference := session.ID
			if !a.explicitTarget {
				reference = qualifiedHistoryReference(outcome.target.Alias, session.ID)
			}
			candidates = append(candidates, candidate{historyResolution: historyResolution{
				Reference: reference, Alias: outcome.target.Alias,
				Machine: outcome.target.Name, Session: session,
			}, rank: rank})
		}
	}
	if successes == 0 {
		return historyResolution{}, fail(2, "no approved Sessions machine answered while looking for %q", value)
	}
	if len(candidates) == 0 {
		return historyResolution{}, fail(1, "no saved conversation named %q was found on the approved fleet", value)
	}
	bestRank := 0
	for _, candidate := range candidates {
		bestRank = max(bestRank, candidate.rank)
	}
	best := candidates[:0]
	for _, candidate := range candidates {
		if candidate.rank == bestRank {
			best = append(best, candidate)
		}
	}
	if len(best) != 1 {
		sort.Slice(best, func(i, j int) bool { return best[i].Reference < best[j].Reference })
		lines := make([]string, 0, len(best))
		for _, candidate := range best {
			lines = append(lines, fmt.Sprintf("  %s  %s  %s", candidate.Reference, candidate.Session.Tool, candidate.Session.CWD))
		}
		return historyResolution{}, fail(1,
			"%q matches %d saved conversations; use one exact reference:\n%s",
			value, len(best), strings.Join(lines, "\n"),
		)
	}
	return best[0].historyResolution, nil
}

func looksLikeHistoryID(value string) bool {
	if strings.HasPrefix(value, "provider-history:") || strings.HasPrefix(value, "provider:") {
		return true
	}
	if len(value) != 36 {
		return false
	}
	for index, character := range value {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			if character != '-' {
				return false
			}
			continue
		}
		if !((character >= '0' && character <= '9') || (character >= 'a' && character <= 'f') || (character >= 'A' && character <= 'F')) {
			return false
		}
	}
	return true
}
