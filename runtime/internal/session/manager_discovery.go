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
	m.restoreHealthMu.Lock()
	defer m.restoreHealthMu.Unlock()
	if !m.restoreHealthAt.IsZero() && time.Since(m.restoreHealthAt) < 5*time.Second {
		return m.restoreHealthCount
	}
	count, err := state.CountRestorePending(m.config.RunnerStateDir)
	if err != nil {
		log.Printf("count paused reboot restores: %v", err)
		return m.restoreHealthCount
	}
	m.restoreHealthAt = time.Now()
	m.restoreHealthCount = count
	return count
}

func (m *Manager) DiscoverWithOptions(ctx context.Context, options DiscoverOptions) error {
	m.discoveryMu.Lock()
	defer m.discoveryMu.Unlock()
	m.registry.MarkDiscovering(true)
	defer m.registry.MarkDiscovering(false)

	candidates, deadArtifacts := m.orphanPlistCandidates()
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
			if metadata.Info.PID > 0 && !m.options.ProcessAlive(metadata.Info.PID) {
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
			if m.runnerAlive(id, metadata.Info) {
				log.Printf("[discover] runner %s unreachable but pid %d alive — leaving it alone", id, metadata.Info.PID)
				delete(candidates, id)
				delete(deadArtifacts, id)
				continue
			}
			if m.options.ProcessAlive(metadata.Info.PID) {
				log.Printf("[discover] runner %s pid %d is PID reuse (%s) — treating as dead",
					id, metadata.Info.PID, truncate(m.options.ProcessCommand(metadata.Info.PID), 60))
			}
			deadArtifacts[id] = struct{}{}
			candidates[id] = struct{}{}
		}
	}

	ids := sortedKeys(candidates)
	if err := m.guard.Check(len(ids), options.Force); err != nil {
		// The guard only protects the destructive half of the sweep. Skipping
		// reconciliation as well would wedge discovery permanently: nothing
		// else records runner_lost, so every ledger-derived view would keep
		// reporting these sessions as live and no later sweep could ever get
		// back under the limit. Reconciliation writes observations only — it
		// removes no socket, metadata document, or launch agent.
		m.reconcileLedger(ctx)
		var guardErr *MassKillError
		if errors.As(err, &guardErr) {
			return &MassKillError{
				Count: guardErr.Count, Limit: guardErr.Limit,
				Operation: "stale runner artifact removals during discovery",
				Remedy: fmt.Sprintf(
					"sockets, metadata, and launch agents were left in place and no session was touched, "+
						"and lost runners were still recorded in the ledger. Review them with `sessions ls -a` and end "+
						"the ones you no longer need with `sessions kill <id>...` (--force is required for more than %d "+
						"targets); discovery finishes the cleanup by itself once at most %d runners are stale",
					guardErr.Limit, guardErr.Limit),
			}
		}
		return err
	}
	var cleanupErrors []error
	for _, id := range ids {
		if _, dead := deadArtifacts[id]; dead {
			// Structured histories are durable conversation evidence, not
			// disposable runner coordination artifacts. An unreachable or dead
			// runner may lose its socket and metadata, but discovery must never
			// erase the transcript needed to inspect or continue that session.
			for _, suffix := range []string{".sock", ".json"} {
				if removeErr := os.Remove(filepath.Join(m.config.RunnerStateDir, id+suffix)); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					cleanupErrors = append(cleanupErrors, removeErr)
				}
			}
		}
		if reapErr := m.reap(id); reapErr != nil {
			cleanupErrors = append(cleanupErrors, reapErr)
		}
	}
	m.reconcileLedger(ctx)
	return errors.Join(cleanupErrors...)
}

func (m *Manager) orphanPlistCandidates() (map[string]struct{}, map[string]struct{}) {
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
		if m.runnerAlive(id, metadata.Info) {
			continue
		}
		if m.options.ProcessAlive(metadata.Info.PID) {
			log.Printf("[discover] orphan runner %s pid %d is PID reuse (%s) — treating as dead",
				id, metadata.Info.PID, truncate(m.options.ProcessCommand(metadata.Info.PID), 60))
		}
		candidates[id] = struct{}{}
		deadArtifacts[id] = struct{}{}
	}
	return candidates, deadArtifacts
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

func truncate(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
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
