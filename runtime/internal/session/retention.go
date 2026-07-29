package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// RetentionItem is one already-closed record considered by an explicit gc
// request. Archiving removes it from retained list surfaces while leaving the
// append-only lifecycle and transcript evidence intact.
type RetentionItem struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Kind       string `json:"kind"`
	ClosedAtMS int64  `json:"closed_at_ms"`
	Status     string `json:"status"`
	Reason     string `json:"reason,omitempty"`
}

type RetentionResult struct {
	DryRun   bool            `json:"dry_run"`
	CutoffMS int64           `json:"cutoff_ms"`
	Items    []RetentionItem `json:"items"`
}

// ArchiveClosed records an explicit user-selected batch. Unlike GCClosed it
// has no age policy: the selected IDs are the policy. Live sessions and
// ancestors with retained descendants are still refused.
func (m *Manager) ArchiveClosed(ctx context.Context, ids []string) (RetentionResult, error) {
	result := RetentionResult{DryRun: false, Items: []RetentionItem{}}
	if len(ids) == 0 {
		return result, errors.New("at least one session id is required")
	}
	if m.ledgerReader == nil || m.retention == nil {
		return result, errors.New("retention ledger is unavailable")
	}
	if m.registry.IsDiscovering() {
		return result, errors.New("archive is unavailable while runner discovery is in progress")
	}
	states, err := m.ledgerStates(ctx)
	if err != nil {
		return result, err
	}
	byID := make(map[string]ledger.LaneState, len(states))
	for _, lane := range states {
		byID[lane.LaneID] = lane
	}
	requested := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			requested[id] = struct{}{}
		}
	}
	if len(requested) == 0 {
		return result, errors.New("at least one session id is required")
	}
	targets := make(map[string]struct{}, len(requested))
	reasons := make(map[string]string, len(requested))
	for id := range requested {
		lane, ok := byID[id]
		switch {
		case !ok || !lane.Created:
			reasons[id] = "record not found"
		case lane.Archived:
			reasons[id] = "already archived"
		case !durablyClosed(lane):
			reasons[id] = "session is still running"
		case runtimeStillLive(m.registry, m.config, id):
			reasons[id] = "runner is still live"
		default:
			targets[id] = struct{}{}
		}
	}
	for changed := true; changed; {
		changed = false
		for id := range targets {
			if hasRetainedDescendant(id, byID, targets) {
				delete(targets, id)
				reasons[id] = "has a retained descendant"
				changed = true
			}
		}
	}
	toArchive := make([]ledger.Archived, 0, len(targets))
	for id := range requested {
		lane := byID[id]
		closedAt := lane.ClosedAtMS
		if closedAt == 0 {
			closedAt = lane.LastEventAtMS
		}
		kind := "session"
		if lane.Tool == string(state.ToolLane) {
			kind = "lane"
		}
		item := RetentionItem{
			ID: id, Name: lane.Name, Kind: kind, ClosedAtMS: closedAt,
			Status: "skipped", Reason: reasons[id],
		}
		if _, ok := targets[id]; ok {
			item.Status = "archived"
			item.Reason = ""
			toArchive = append(toArchive, ledger.Archived{Meta: ledger.Meta{LaneID: id}})
		}
		result.Items = append(result.Items, item)
	}
	sort.Slice(result.Items, func(i, j int) bool { return result.Items[i].ID < result.Items[j].ID })
	if len(toArchive) > 0 {
		if err := m.retention.RecordArchived(ctx, toArchive); err != nil {
			return RetentionResult{}, err
		}
	}
	return result, nil
}

func (m *Manager) GCClosed(ctx context.Context, cutoffMS int64, dryRun bool) (RetentionResult, error) {
	result := RetentionResult{DryRun: dryRun, CutoffMS: cutoffMS, Items: []RetentionItem{}}
	if cutoffMS <= 0 {
		return result, errors.New("retention cutoff must be positive")
	}
	if m.ledgerReader == nil || m.retention == nil {
		return result, errors.New("retention ledger is unavailable")
	}
	if !dryRun && m.registry.IsDiscovering() {
		return result, errors.New("retention apply is unavailable while runner discovery is in progress")
	}
	states, err := m.ledgerStates(ctx)
	if err != nil {
		return result, err
	}
	byID := make(map[string]ledger.LaneState, len(states))
	targets := make(map[string]struct{})
	reasons := make(map[string]string)
	for _, lane := range states {
		byID[lane.LaneID] = lane
		if !lane.Created || !durablyClosed(lane) || lane.Archived {
			continue
		}
		closedAt := lane.ClosedAtMS
		if closedAt == 0 {
			closedAt = lane.LastEventAtMS
		}
		switch {
		case closedAt > cutoffMS:
			reasons[lane.LaneID] = "newer than retention cutoff"
		case runtimeStillLive(m.registry, m.config, lane.LaneID):
			reasons[lane.LaneID] = "runner is still live"
		default:
			targets[lane.LaneID] = struct{}{}
		}
	}

	// Never archive an ancestor while a retained descendant still refers to it.
	// Iterate because removing one blocked child from the target set can make
	// its own ancestors ineligible in the next pass.
	for changed := true; changed; {
		changed = false
		for id := range targets {
			if hasRetainedDescendant(id, byID, targets) {
				delete(targets, id)
				reasons[id] = "has a retained descendant"
				changed = true
			}
		}
	}

	toArchive := make([]ledger.Archived, 0, len(targets))
	for _, lane := range states {
		if !lane.Created || !durablyClosed(lane) || lane.Archived {
			continue
		}
		closedAt := lane.ClosedAtMS
		if closedAt == 0 {
			closedAt = lane.LastEventAtMS
		}
		kind := "session"
		if lane.Tool == string(state.ToolLane) {
			kind = "lane"
		}
		item := RetentionItem{
			ID: lane.LaneID, Name: lane.Name, Kind: kind, ClosedAtMS: closedAt,
			Status: "skipped", Reason: reasons[lane.LaneID],
		}
		if _, ok := targets[lane.LaneID]; ok {
			if dryRun {
				item.Status = "would_archive"
			} else {
				item.Status = "archived"
			}
			item.Reason = ""
			toArchive = append(toArchive, ledger.Archived{Meta: ledger.Meta{LaneID: lane.LaneID}})
		}
		result.Items = append(result.Items, item)
	}
	sort.Slice(result.Items, func(i, j int) bool {
		if result.Items[i].ClosedAtMS != result.Items[j].ClosedAtMS {
			return result.Items[i].ClosedAtMS < result.Items[j].ClosedAtMS
		}
		return result.Items[i].ID < result.Items[j].ID
	})
	if !dryRun {
		if err := m.retention.RecordArchived(ctx, toArchive); err != nil {
			return RetentionResult{}, err
		}
	}
	return result, nil
}

func runtimeStillLive(registry *state.Registry, config state.Config, id string) bool {
	session, ok := registry.Get(id)
	if ok && !session.Info().Exited {
		return true
	}
	for _, path := range []string{
		state.For(config.RunnerStateDir, id).Socket,
		state.RunnerPlistPath(config.LaunchAgentsDir, id),
		state.LegacyRunnerPlistPath(config.LaunchAgentsDir, id),
	} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	metadata, err := state.ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, id+".json"))
	if err != nil || metadata.Info.PID <= 0 {
		return false
	}
	return processAlive(metadata.Info.PID) &&
		runnerCommandMatches(processCommand(metadata.Info.PID), id, metadata.Info.Cmd)
}

func hasRetainedDescendant(
	ancestor string,
	states map[string]ledger.LaneState,
	targets map[string]struct{},
) bool {
	for id, candidate := range states {
		if id == ancestor || candidate.Archived {
			continue
		}
		if _, alsoArchived := targets[id]; alsoArchived {
			continue
		}
		visited := make(map[string]struct{})
		current := candidate
		for current.CreatorKind == ledger.CreatorSession {
			parent := current.CreatorID
			if parent == ancestor {
				return true
			}
			if _, cycle := visited[parent]; cycle {
				break
			}
			visited[parent] = struct{}{}
			next, ok := states[parent]
			if !ok {
				break
			}
			current = next
		}
	}
	return false
}
