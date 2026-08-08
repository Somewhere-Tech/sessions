package session

import (
	"context"
	"errors"
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
// has no age policy: the selected IDs are the policy. A session whose runner
// is still running is refused; nothing else is.
//
// A session the daemon merely lost contact with is archivable once the probe
// says its runner is not running. The user picked it out of a list and asked
// for it; refusing because Sessions never saw an exit status would hand the
// request back to the requester over a fact Sessions can check directly.
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
		case m.runtimeStillLive(id):
			// The authoritative refusal, asked first: this session's runner
			// process is running right now.
			reasons[id] = "runner is still live"
		case !durablyClosed(lane) && !lane.RunnerLost:
			reasons[id] = "session is still running"
		default:
			targets[id] = struct{}{}
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
		case m.runtimeStillLive(lane.LaneID):
			reasons[lane.LaneID] = "runner is still live"
		default:
			targets[lane.LaneID] = struct{}{}
		}
	}

	// Archiving hides a row; it deletes nothing. EventArchived is appended to
	// an append-only ledger, and a descendant's CreatorID keeps pointing at
	// this ancestor whether or not the ancestor is archived -- the lineage the
	// old guard protected survives archiving by construction, so it could not
	// lose what it was refusing to risk. What it did cost was real: finish any
	// piece of work that delegated once and neither row could ever be cleared,
	// with the reason "has a retained descendant", which means nothing to the
	// person reading it. AGENTS.md rule 10 -- a guard compensating for an
	// inference the code does not need is scope that should not exist.
	//
	// A live descendant is still refused, by runtimeStillLive above: the
	// question that matters is whether a process is running, not whether a
	// record refers to another record.

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

// runtimeStillLive answers whether a session's runner is running right now.
//
// It asks the process, not the filesystem. A socket file or a launch agent
// plist is a leftover as often as it is a runner: launchd plists outlive the
// process they started, sockets survive a crash, and a session that ends
// without a clean teardown leaves both behind. Statting them and returning
// true on any hit is how "archive from list doesn't work" happened -- an
// already-closed session refused archiving with "runner is still live" because
// a plist from days ago existed. Files may corroborate, they may not decide:
// if the recorded PID is not this session's runner, the session is not live
// however many artifacts remain on disk.
func (m *Manager) runtimeStillLive(id string) bool {
	if session, ok := m.registry.Get(id); ok {
		info := session.Info()
		// A registered, unreached session is not evidence of a live process:
		// unreachable is exactly the state where the socket died and the
		// process may or may not have. Fall through to the PID probe.
		if !info.Exited && !info.Unreachable {
			return true
		}
	}
	metadata, err := state.ReadRunnerMetadata(filepath.Join(m.config.RunnerStateDir, id+".json"))
	if err != nil {
		// No readable metadata means no PID to probe and no process this
		// daemon is holding. There is nothing here to protect.
		return false
	}
	metadata.Info.ID = id
	return m.runnerAlive(id, metadata.Info)
}
