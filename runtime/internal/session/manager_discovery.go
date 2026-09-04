package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/liveness"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func (m *Manager) RunDiscoveryLoop() {
	interval := DefaultDiscoveryInterval
	if configured := strings.TrimSpace(os.Getenv(discoveryIntervalEnv)); configured != "" {
		parsed, err := time.ParseDuration(configured)
		if err != nil || parsed <= 0 {
			log.Printf("runner discovery: invalid %s=%q; using %s", discoveryIntervalEnv, configured, interval)
		} else {
			interval = parsed
		}
	}
	lastReportedError := ""
	run := func() {
		m.refreshPendingRestores()
		if err := m.Discover(m.ctx); err != nil && !errors.Is(err, context.Canceled) {
			message := err.Error()
			if message != lastReportedError {
				log.Printf("runner discovery: %v", err)
				lastReportedError = message
			}
		} else {
			lastReportedError = ""
		}
	}
	run()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			run()
		}
	}
}

func (m *Manager) Discover(ctx context.Context) error {
	return m.DiscoverWithOptions(ctx, DiscoverOptions{})
}

// RestorePendingCount is the calm, queryable safe-mode signal exposed through
// daemon health. It never starts or adopts a runner.
func (m *Manager) RestorePendingCount() int {
	m.pausedMu.RLock()
	defer m.pausedMu.RUnlock()
	return len(m.pausedRestores)
}

// RetiredRestoreCount reports reboot markers that had no metadata or creation
// record and were moved out of the active runner directory.
func (m *Manager) RetiredRestoreCount() int {
	m.pausedMu.RLock()
	defer m.pausedMu.RUnlock()
	return m.pausedRetiredCount
}

func (m *Manager) DiscoverWithOptions(ctx context.Context, options DiscoverOptions) error {
	m.discoveryMu.Lock()
	defer m.discoveryMu.Unlock()
	m.registry.MarkDiscovering(true)
	defer m.registry.MarkDiscovering(false)

	processes, snapshotOK := m.processSnapshot(ctx)
	candidates, deadArtifacts := m.orphanPlistCandidates(processes, snapshotOK)
	for id := range candidates {
		if _, exists := m.registry.Get(id); exists {
			// A filesystem-only orphan signal cannot override the daemon's
			// current ownership of a live session.
			delete(candidates, id)
			delete(deadArtifacts, id)
		}
	}
	artifactIDs, err := state.RunnerArtifactIDs(m.config.RunnerStateDir)
	if err != nil {
		return fmt.Errorf("read runner state directory: %w", err)
	}
	if err == nil {
		for _, id := range artifactIDs {
			if existing, exists := m.registry.Get(id); exists && !existing.Info().Unreachable {
				continue
			}
			metadataPath := filepath.Join(m.config.RunnerStateDir, id+".json")
			metadata, metadataErr := state.ReadRunnerMetadata(metadataPath)
			if metadataErr != nil {
				// A torn, temporarily unreadable, or forward-version metadata
				// document is absence of evidence, not evidence that the
				// runner died. Preserve every artifact for a later retry.
				log.Printf("[discover] runner %s metadata unreadable — leaving it alone: %v", id, metadataErr)
				delete(candidates, id)
				continue
			}
			if _, pendingErr := os.Stat(state.For(m.config.RunnerStateDir, id).RestorePending); pendingErr == nil {
				// The runner deliberately declined to respawn this provider after a
				// reboot. Keep its metadata, transcripts, and launch record intact so
				// recovery can show the user what was paused; a stale-artifact sweep
				// must not erase the evidence that safe mode exists to preserve.
				delete(candidates, id)
				delete(deadArtifacts, id)
				continue
			}
			probe := metadata.Info
			probe.ID = id
			probe.SocketPath = state.For(m.config.RunnerStateDir, id).Socket
			// After a reboot, old coordination artifacts can outnumber the live
			// sessions by dozens. A PID which no longer exists is definitive: no
			// runner can answer that socket. Classify it without paying three
			// attach attempts and two retry delays. Ambiguous live/PID-reuse cases
			// still take the conservative attach-first path below.
			identityAlive := m.runnerAliveWithSnapshot(id, metadata.Info, processes, snapshotOK)
			if metadata.Info.PID > 0 && !identityAlive {
				if m.attachDiscovered(ctx, probe, metadata) {
					delete(candidates, id)
					continue
				}
				deadArtifacts[id] = struct{}{}
				candidates[id] = struct{}{}
				continue
			}
			connected := false
			for attempt := 0; attempt < m.options.DiscoveryRetries; attempt++ {
				runner, attachErr := m.launcher.Attach(ctx, probe)
				if attachErr == nil {
					if session, registerErr := m.registry.RegisterMetadata(ctx, runner, metadata, ""); registerErr == nil {
						m.manage(session)
						connected = true
						break
					}
				}
				if attempt+1 < m.options.DiscoveryRetries && !waitContext(ctx, m.options.DiscoveryDelay) {
					return ctx.Err()
				}
			}
			if connected {
				delete(candidates, id)
				continue
			}
			if metadata.Info.PID <= 0 {
				log.Printf("[discover] runner %s has no trustworthy pid — leaving it alone", id)
				delete(candidates, id)
				continue
			}
			if m.runnerAliveWithSnapshot(id, metadata.Info, processes, snapshotOK) {
				log.Printf("[discover] runner %s unreachable but pid %d alive — leaving it alone", id, metadata.Info.PID)
				delete(candidates, id)
				delete(deadArtifacts, id)
				continue
			}
			deadArtifacts[id] = struct{}{}
			candidates[id] = struct{}{}
		}
	}

	allIDs := sortedKeys(candidates)
	ids := allIDs
	if !options.Force && len(ids) > DefaultDiscoveryBatch {
		ids = ids[:DefaultDiscoveryBatch]
	}
	var cleanupErrors []error
	retired := 0
	for _, id := range ids {
		_, dead := deadArtifacts[id]
		removed, retireErr := m.retireRunnerArtifacts(ctx, id, dead)
		if removed {
			retired++
		}
		if retireErr != nil {
			cleanupErrors = append(cleanupErrors, retireErr)
		}
	}
	m.recordArtifactSweep(retired, len(allIDs)-retired)
	m.reconcileLedger(ctx)
	return errors.Join(cleanupErrors...)
}

func (m *Manager) orphanPlistCandidates(processes map[int]string, snapshotOK bool) (map[string]struct{}, map[string]struct{}) {
	candidates := make(map[string]struct{})
	deadArtifacts := make(map[string]struct{})
	entries, err := os.ReadDir(m.config.LaunchAgentsDir)
	if err != nil {
		return candidates, deadArtifacts
	}
	prefixes := []string{
		"tech.somewhere.sessions.runner.",
		"tech.pretty-pty.runner.",
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".plist") {
			continue
		}
		id := ""
		for _, prefix := range prefixes {
			if strings.HasPrefix(name, prefix) {
				id = strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".plist")
				break
			}
		}
		if id == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(m.config.RunnerStateDir, id+".events")); err == nil {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < orphanStartingGrace {
			continue
		}
		_, socketErr := os.Stat(state.For(m.config.RunnerStateDir, id).Socket)
		metadataPath := filepath.Join(m.config.RunnerStateDir, id+".json")
		_, metadataErr := os.Stat(metadataPath)
		if !errors.Is(socketErr, os.ErrNotExist) {
			continue
		}
		if errors.Is(metadataErr, os.ErrNotExist) {
			candidates[id] = struct{}{}
			continue
		}
		if metadataErr != nil {
			continue
		}
		metadata, metadataErr := state.ReadRunnerMetadata(metadataPath)
		if metadataErr != nil || metadata.Info.ID != id || metadata.Info.PID <= 0 {
			continue
		}
		if m.runnerAliveWithSnapshot(id, metadata.Info, processes, snapshotOK) {
			continue
		}
		candidates[id] = struct{}{}
		deadArtifacts[id] = struct{}{}
	}
	return candidates, deadArtifacts
}

func (m *Manager) processSnapshot(ctx context.Context) (map[int]string, bool) {
	if m.options.ProcessSnapshot == nil {
		return nil, false
	}
	processes, err := m.options.ProcessSnapshot(ctx)
	if err != nil {
		log.Printf("[discover] process snapshot unavailable; using per-runner probes: %v", err)
		return nil, false
	}
	return processes, true
}

func (m *Manager) runnerAliveWithSnapshot(id string, info proto.RunnerInfo, processes map[int]string, ok bool) bool {
	if !ok {
		return m.runnerAlive(id, info)
	}
	command, alive := processes[info.PID]
	return info.PID > 0 && alive && liveness.CommandMatches(command, id, info.Cmd)
}

func (m *Manager) attachDiscovered(ctx context.Context, probe proto.RunnerInfo, metadata state.RunnerMetadata) bool {
	runner, err := m.launcher.Attach(ctx, probe)
	if err != nil {
		return false
	}
	session, err := m.registry.RegisterMetadata(ctx, runner, metadata, "")
	if err != nil {
		log.Printf("[discover] runner %s answered its socket but metadata registration failed — leaving artifacts alone: %v", probe.ID, err)
		return true
	}
	m.manage(session)
	return true
}

func (m *Manager) retireRunnerArtifacts(ctx context.Context, id string, dead bool) (bool, error) {
	var cleanupErrors []error
	if dead {
		// Provider histories and event logs are conversation evidence. Only the
		// socket and metadata are runner coordination artifacts.
		for _, suffix := range []string{".sock", ".json"} {
			path := filepath.Join(m.config.RunnerStateDir, id+suffix)
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
	}
	if err := m.reap(id); err != nil {
		cleanupErrors = append(cleanupErrors, err)
	}
	if m.runnerArtifactsRemain(id) {
		return false, errors.Join(cleanupErrors...)
	}
	m.observe(ctx, "runner artifacts retired", func(writer ledger.ObservationWriter) error {
		return writer.RecordRunnerArtifactsRetired(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: id}})
	})
	log.Printf("[discover] retired stale runner artifacts for %s", id)
	return true, errors.Join(cleanupErrors...)
}

func (m *Manager) runnerArtifactsRemain(id string) bool {
	for _, path := range []string{
		state.RunnerPlistPath(m.config.LaunchAgentsDir, id),
		state.LegacyRunnerPlistPath(m.config.LaunchAgentsDir, id),
		state.For(m.config.RunnerStateDir, id).Socket,
		filepath.Join(m.config.RunnerStateDir, id+".json"),
	} {
		if _, err := os.Stat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return true
		}
	}
	return false
}

func (m *Manager) recordArtifactSweep(retired, pending int) {
	m.pausedMu.Lock()
	m.artifactRetired += retired
	m.artifactPending = pending
	m.pausedMu.Unlock()
}

// ArtifactRetirementHealth reports bounded discovery cleanup since daemon
// start and the stale sets left for later ticks.
func (m *Manager) ArtifactRetirementHealth() (int, int) {
	m.pausedMu.RLock()
	defer m.pausedMu.RUnlock()
	return m.artifactRetired, m.artifactPending
}

// runnerAlive is the manager's only liveness answer: is THIS session's runner
// process running right now? It routes the shared rule in internal/liveness
// through the injectable probes so discovery tests can simulate a process
// table, and it answers about the session rather than about a bare PID, so a
// recycled PID cannot masquerade as a live runner.
func (m *Manager) runnerAlive(id string, info proto.RunnerInfo) bool {
	if info.PID <= 0 || !m.options.ProcessAlive(info.PID) {
		return false
	}
	return liveness.CommandMatches(m.options.ProcessCommand(info.PID), id, info.Cmd)
}

func (m *Manager) reap(id string) error {
	if reaper, ok := m.launcher.(interface{ Reap(string) error }); ok {
		return reaper.Reap(id)
	}
	var reapErrors []error
	for _, path := range []string{
		state.RunnerPlistPath(m.config.LaunchAgentsDir, id),
		state.LegacyRunnerPlistPath(m.config.LaunchAgentsDir, id),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			reapErrors = append(reapErrors, err)
		}
	}
	return errors.Join(reapErrors...)
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func loadGlobalHooks(path string) globalHooks {
	if path == "" {
		return globalHooks{}
	}
	encoded, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return globalHooks{}
	}
	if err != nil {
		log.Printf("[hooks] ignoring malformed %s: %v", path, err)
		return globalHooks{}
	}
	var raw map[string]any
	if json.Unmarshal(encoded, &raw) != nil {
		log.Printf("[hooks] ignoring malformed %s: expected an object", path)
		return globalHooks{}
	}
	value, exists := raw["onIdle"]
	if !exists {
		return globalHooks{}
	}
	onIdle, ok := value.(string)
	if !ok || strings.TrimSpace(onIdle) == "" {
		log.Printf("[hooks] ignoring malformed %s: onIdle must be a non-empty string", path)
		return globalHooks{}
	}
	return globalHooks{OnIdle: onIdle}
}
