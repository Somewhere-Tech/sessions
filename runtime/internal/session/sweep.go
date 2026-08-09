package session

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// SweptArtifact is one file the startup sweep unlinked, reported so the daemon
// log says what it removed rather than leaving the user to notice.
type SweptArtifact struct {
	SessionID string
	Path      string
}

// SweepStaleRunnerArtifacts unlinks launch agents and sockets left behind by
// sessions that are over.
//
// These files are why "archive from list doesn't work" was reported: a plist
// or socket that outlives its process is indistinguishable, to anything that
// only stats files, from a running runner. The right fix is to stop inferring
// liveness from files (see runtimeStillLive), and then to stop leaving the
// files around.
//
// It is deliberately timid, because the cost of the two mistakes is not
// symmetric. Leaving a stale file costs one line in a log and another pass
// later. Unlinking a live session's launch agent costs the user a session that
// will not come back. So every one of these must hold before anything is
// removed:
//
//   - The daemon does not currently hold the session. Live work is never
//     touched.
//   - The ledger says the session actually ended -- a user-requested end, a
//     reported runner status, a reap, a continuation elsewhere, or an archive.
//     "The daemon could not reach it" (runner_lost) explicitly does not
//     qualify: that is the inference this whole change exists to remove.
//   - The recorded runner process is not running. If metadata names a live pid
//     that still matches this session, the session is live whatever the ledger
//     says, and nothing is removed.
//   - Nothing is mid-launch. Metadata this daemon cannot read is unknown, not
//     dead, and an artifact younger than the launch grace may belong to a
//     runner that has not reported in yet.
//
// Transcripts, structured histories, event logs, and runner metadata are never
// touched: they are the durable record of the conversation, not runner
// coordination state.
func (m *Manager) SweepStaleRunnerArtifacts(ctx context.Context) []SweptArtifact {
	lanes, err := m.ledgerStates(ctx)
	if err != nil {
		log.Printf("[sweep] read lanes: %v; leaving every runner artifact in place", err)
		return nil
	}
	closed := make(map[string]struct{}, len(lanes))
	for _, lane := range lanes {
		if laneClosedForSweep(lane) {
			closed[lane.LaneID] = struct{}{}
		}
	}

	var removed []SweptArtifact
	for _, id := range m.staleArtifactCandidates(closed) {
		for _, path := range []string{
			state.RunnerPlistPath(m.config.LaunchAgentsDir, id),
			state.LegacyRunnerPlistPath(m.config.LaunchAgentsDir, id),
			state.For(m.config.RunnerStateDir, id).Socket,
		} {
			err := os.Remove(path)
			switch {
			case err == nil:
				log.Printf("[sweep] session %s ended and its runner is not running — removed %s", id, path)
				removed = append(removed, SweptArtifact{SessionID: id, Path: path})
			case errors.Is(err, os.ErrNotExist):
			default:
				log.Printf("[sweep] session %s: cannot remove %s: %v", id, path, err)
			}
		}
	}
	if len(removed) > 0 {
		log.Printf("[sweep] removed %d stale runner artifact(s)", len(removed))
	}
	return removed
}

// staleArtifactCandidates returns the session ids whose launch agents and
// sockets may be unlinked, given the set of ids the ledger reports as ended.
func (m *Manager) staleArtifactCandidates(closed map[string]struct{}) []string {
	seen := make(map[string]struct{})
	for _, id := range runnerPlistIDs(m.config.LaunchAgentsDir) {
		seen[id] = struct{}{}
	}
	for _, id := range runnerSocketIDs(m.config.RunnerStateDir) {
		seen[id] = struct{}{}
	}

	candidates := make([]string, 0, len(seen))
	for id := range seen {
		if _, ended := closed[id]; !ended {
			// Either the ledger has no ending for this session or it only has
			// a loss of contact. Both are "not known to be over".
			continue
		}
		if _, live := m.registry.Get(id); live {
			// The ledger and the runtime map disagree. The runtime map is
			// holding a socket; that outranks a record.
			log.Printf("[sweep] session %s is recorded as ended but the daemon still holds it — leaving its artifacts alone", id)
			continue
		}
		if m.artifactsStarting(id) {
			continue
		}
		metadata, err := state.ReadRunnerMetadata(filepath.Join(m.config.RunnerStateDir, id+".json"))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("[sweep] session %s metadata unreadable — leaving its artifacts alone: %v", id, err)
			continue
		}
		if err == nil {
			metadata.Info.ID = id
			if m.runnerAlive(id, metadata.Info) {
				log.Printf("[sweep] session %s is recorded as ended but pid %d is running — leaving its artifacts alone",
					id, metadata.Info.PID)
				continue
			}
		}
		candidates = append(candidates, id)
	}
	sort.Strings(candidates)
	return candidates
}

// artifactsStarting reports whether any of a session's runner artifacts is
// young enough to belong to a launch still in progress. A runner writes its
// plist before it reports a pid, so a fresh plist with no live pid is the
// normal look of a session that is starting, not of one that is over.
func (m *Manager) artifactsStarting(id string) bool {
	for _, path := range []string{
		state.RunnerPlistPath(m.config.LaunchAgentsDir, id),
		state.LegacyRunnerPlistPath(m.config.LaunchAgentsDir, id),
		state.For(m.config.RunnerStateDir, id).Socket,
		filepath.Join(m.config.RunnerStateDir, id+".json"),
	} {
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if time.Since(info.ModTime()) < orphanStartingGrace {
			return true
		}
	}
	return false
}

// runnerPlistIDs lists the session ids that own a runner launch agent, under
// both the current and the legacy label prefix.
func runnerPlistIDs(launchAgentsDir string) []string {
	if launchAgentsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(launchAgentsDir)
	if err != nil {
		return nil
	}
	prefixes := []string{"tech.somewhere.sessions.runner.", "tech.pretty-pty.runner."}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".plist") {
			continue
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".plist")
				if id != "" {
					ids = append(ids, id)
				}
				break
			}
		}
	}
	return ids
}

// runnerSocketIDs lists the session ids that still have a runner socket file.
func runnerSocketIDs(runnerStateDir string) []string {
	if runnerStateDir == "" {
		return nil
	}
	entries, err := os.ReadDir(runnerStateDir)
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".sock") {
			continue
		}
		if id := strings.TrimSuffix(name, ".sock"); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// laneClosedForSweep is the sweep's reading of one ledger record, kept beside
// the sweep so the predicate that authorises unlinking cannot drift away from
// the comment explaining it.
func laneClosedForSweep(lane ledger.LaneState) bool {
	return lane.Created && (durablyClosed(lane) || lane.Archived)
}
