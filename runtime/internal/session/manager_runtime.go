package session

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/claudep"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

func (m *Manager) Close() {
	m.cancel()
	m.ticker.Stop()
	m.closeNotifications()
	m.mu.Lock()
	runtimes := make([]*runtimeSession, 0, len(m.runtimes))
	for _, runtime := range m.runtimes {
		runtimes = append(runtimes, runtime)
	}
	m.runtimes = make(map[string]*runtimeSession)
	m.mu.Unlock()
	for _, runtime := range runtimes {
		runtime.stop()
	}
	m.workerMu.Lock()
	m.workersClosed = true
	m.workerMu.Unlock()
	m.workerWG.Wait()
}

func (m *Manager) manage(session *state.Session) *runtimeSession {
	info := session.Info()
	m.mu.Lock()
	if existing := m.runtimes[info.ID]; existing != nil {
		m.mu.Unlock()
		return existing
	}
	attachment := session.Attach(state.AttachOptions{IncludeClaudeReplay: true, InitialReplayCap: 300})
	runtime := &runtimeSession{
		manager: m, session: session,
		// Initial replay is consumed below. Retain only the subscription and
		// cancellation handles so the manager does not keep a second deep copy
		// of every replayed output and structured event for the session lifetime.
		attachment:             state.Attachment{Events: attachment.Events, Cancel: attachment.Cancel},
		outputObserved:         make(chan struct{}, 1),
		structuredEventArrived: make(chan struct{}, 1),
	}
	if info.Kind == state.KindCodexAppServer || info.Kind == state.KindClaudeStructured {
		for _, raw := range attachment.ClaudeEvents {
			if working, ok := structuredHistoryLifecycle(info.Kind, raw); ok {
				value := working
				runtime.structuredLifecycleWorking = &value
			}
		}
		if runtime.structuredLifecycleWorking != nil {
			session.SetWorking(*runtime.structuredLifecycleWorking)
		}
	}
	for _, event := range attachment.Replay.Events {
		runtime.recentBytes += len(event.Data)
	}
	if !session.Info().Working && (len(attachment.Replay.Events) > 0 || len(attachment.ClaudeEvents) > 0) {
		if supportsTurnLifecycle(session.Info()) {
			classification, summary := inspectIdle(session)
			session.SetIdleResult(
				idleReason(classification.Outcome),
				classification.Line,
				summary,
				session.Info().LastDataAt,
			)
		} else {
			session.ClearIdleResult()
		}
	}
	m.runtimes[info.ID] = runtime
	m.mu.Unlock()
	m.observe(context.Background(), "attached", func(writer ledger.ObservationWriter) error {
		return writer.RecordAttached(context.Background(), ledger.Observation{Meta: ledger.Meta{LaneID: info.ID}})
	})
	if !m.options.DisableWatchers {
		runtime.startWatcher(info)
	}
	if !m.startWorker(runtime.observe) {
		runtime.stop()
		m.mu.Lock()
		if m.runtimes[info.ID] == runtime {
			delete(m.runtimes, info.ID)
		}
		m.mu.Unlock()
	}
	return runtime
}

// expectProviderInput gives a terminal transcript watcher authored text which
// reached the provider without crossing Manager.Input. Fresh Codex PTYs take
// their first prompt from argv, so without this handoff the safe resolver has
// no exact fact with which to choose among same-directory rollout files.
func (r *runtimeSession) expectProviderInput(input string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return
	}
	r.mu.Lock()
	watcher := r.watcher
	r.mu.Unlock()
	if watcher != nil {
		watcher.ExpectInput(input)
	}
}

func structuredHistoryLifecycle(kind string, raw json.RawMessage) (bool, bool) {
	switch kind {
	case state.KindCodexAppServer:
		return codexapp.HistoryLifecycle(raw)
	case state.KindClaudeStructured:
		return claudep.HistoryLifecycle(raw)
	default:
		return false, false
	}
}

func (m *Manager) dropRuntime(id string, expected *runtimeSession) {
	m.mu.Lock()
	if m.runtimes[id] == expected {
		delete(m.runtimes, id)
	}
	m.mu.Unlock()
	expected.stop()
}

func (m *Manager) activityLoop() {
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.ticker.C:
			m.mu.Lock()
			runtimes := make([]*runtimeSession, 0, len(m.runtimes))
			for _, runtime := range m.runtimes {
				runtimes = append(runtimes, runtime)
			}
			m.mu.Unlock()
			for _, runtime := range runtimes {
				runtime.tick()
			}
			m.sampleResources()
		}
	}
}

// sampleResources measures what every live session costs the machine.
//
// It rides the activity tick rather than owning a goroutine of its own, and it
// rides it for both the reasons that matter. A goroutine per session would put
// two hundred timers on a machine that is already the thing being measured,
// which is self-defeating; and one pass costs one walk of the process table
// whatever the session count, so the work here is flat in sessions, not linear.
// The activity tick is sub-second, so the interval gate is what actually sets
// the sampling rate.
//
// Every live session is passed to the tracker, including ones with no process.
// The tracker answers "unknown" for those and SetResources clears their fields,
// which is the whole point: a session that lost its process must stop reporting
// the memory it held when it was last seen.
// SampleResources takes one sample immediately, outside the interval gate.
// The activity loop is the normal caller through sampleResources; this is the
// entry point for a caller that needs a measurement now rather than at the
// next tick, and it is how tests drive sampling deterministically.
func (m *Manager) SampleResources() {
	m.resourceMu.Lock()
	m.resourceSampled = time.Time{}
	m.resourceMu.Unlock()
	m.sampleResources()
}

func (m *Manager) sampleResources() {
	now := m.resourceClock()
	m.resourceMu.Lock()
	if !m.resourceSampled.IsZero() && now.Sub(m.resourceSampled) < m.resourceInterval {
		m.resourceMu.Unlock()
		return
	}
	m.resourceSampled = now
	m.resourceMu.Unlock()

	infos := m.registry.List(false)
	roots := make(map[string]int, len(infos))
	for _, info := range infos {
		roots[info.ID] = info.PID
	}
	samples, err := m.resources.Sample(roots)
	if err != nil {
		m.resourceMu.Lock()
		alreadyReported := m.resourceFailed
		m.resourceFailed = true
		m.resourceMu.Unlock()
		if !alreadyReported {
			log.Printf("[resource] cannot read the process table, so session memory and CPU stay unknown: %v", err)
		}
		// Leaving the previous numbers in place would present the last
		// successful sample as current. Clear every session to unknown; the
		// sampledAt timestamp goes with them.
		for _, info := range infos {
			if session, ok := m.registry.Get(info.ID); ok {
				session.SetResources(state.ResourceSample{})
			}
		}
		return
	}
	m.resourceMu.Lock()
	m.resourceFailed = false
	m.resourceMu.Unlock()
	for id, sample := range samples {
		session, ok := m.registry.Get(id)
		if !ok {
			continue
		}
		session.SetResources(state.ResourceSample{
			Known:      sample.Known,
			MemoryByte: sample.RSSBytes,
			Processes:  sample.Processes,
			CPUPercent: sample.CPUPercent,
			CPUKnown:   sample.CPUKnown,
			At:         sample.At,
		})
	}
}

func (r *runtimeSession) observe() {
	id := r.session.Info().ID
	for event := range r.attachment.Events {
		switch event.Kind {
		case proto.EventOutput:
			r.mu.Lock()
			r.recentBytes += len(event.Output.Data)
			r.preTurnOutput = true
			r.mu.Unlock()
			select {
			case r.outputObserved <- struct{}{}:
			default:
			}
		case proto.EventClaude:
			if event.ClaudeActivityAt != 0 {
				r.manager.observe(context.Background(), "provider activity", func(writer ledger.ObservationWriter) error {
					return writer.RecordActivity(context.Background(), ledger.Activity{
						Meta: ledger.Meta{LaneID: id, AtMS: event.ClaudeActivityAt}, Source: ledger.ActivityProviderEvent,
					})
				})
			}
			r.manager.recordStructuredUsage(r.session.Info(), event.ClaudeEvent)
			r.followProviderTitle()
			select {
			case r.structuredEventArrived <- struct{}{}:
			default:
			}
		case proto.EventCodex:
			if event.ClaudeActivityAt != 0 {
				r.manager.observe(context.Background(), "provider activity", func(writer ledger.ObservationWriter) error {
					return writer.RecordActivity(context.Background(), ledger.Activity{
						Meta: ledger.Meta{LaneID: id, AtMS: event.ClaudeActivityAt}, Source: ledger.ActivityProviderEvent,
					})
				})
			}
			kind := r.session.Info().Kind
			if structuredTurnCompleted(kind, event.CodexEvent) {
				r.markStructuredDone()
			}
			if working, ok := structuredHistoryLifecycle(kind, event.CodexEvent); ok {
				r.mu.Lock()
				value := working
				r.structuredLifecycleWorking = &value
				r.mu.Unlock()
				r.setWorking(working)
			}
			r.manager.recordStructuredUsage(r.session.Info(), event.CodexEvent)
			// A structured runner's frames arrive as EventCodex whichever
			// provider it is (proto/client.go decodes every Structured frame
			// that way), and recordCodexLocked runs them through the same
			// title parsing. A Codex conversation has no title, so for Codex
			// this costs one comparison and stops.
			r.followProviderTitle()
			select {
			case r.structuredEventArrived <- struct{}{}:
			default:
			}
		case proto.EventRunnerLost:
			r.cancelWaiting()
			r.manager.notifyLost(r.session.Info())
			r.manager.observe(context.Background(), "runner lost", func(writer ledger.ObservationWriter) error {
				return writer.RecordRunnerLost(context.Background(), ledger.Observation{Meta: ledger.Meta{LaneID: id}})
			})
			r.manager.dropRuntime(id, r)
			r.manager.scheduleReconnect(id, []time.Duration{time.Second, 3 * time.Second, 10 * time.Second})
			return
		case proto.EventExit:
			r.manager.dropRuntime(id, r)
			return
		}
	}
}

func (m *Manager) recordStructuredUsage(info state.SessionInfo, raw json.RawMessage) {
	if m.usage == nil || !bytes.Contains(raw, []byte(`"usage"`)) {
		return
	}
	if err := m.usage.RecordStructured(context.Background(), info, raw); err != nil {
		log.Printf("[usage] record live structured event for %s: %v", info.ID, err)
	}
}

func (r *runtimeSession) stop() {
	r.stopOnce.Do(func() {
		r.attachment.Cancel()
		r.mu.Lock()
		r.stopped = true
		r.waitingGeneration++
		if r.waitingTimer != nil {
			r.waitingTimer.Stop()
			r.waitingTimer = nil
		}
		watcher := r.watcher
		r.watcher = nil
		r.mu.Unlock()
		if watcher != nil {
			watcher.Close()
		}
	})
}

func (r *runtimeSession) tick() {
	info := r.session.Info()
	if info.Exited {
		return
	}
	r.mu.Lock()
	r.recentBytes /= 2
	recent := r.recentBytes
	structured := r.structuredLifecycleWorking
	r.mu.Unlock()
	byteWorking := recent >= workingBytesThreshold
	next := byteWorking
	switch info.Tool {
	case state.ToolClaude:
		if info.Kind == state.KindClaudeStructured && structured != nil {
			next = *structured
		} else if recent <= 0 {
			next = false
		} else if snapshot, _, err := r.session.Snapshot(context.Background(), 0); err == nil {
			next = ClaudeWorkingFromSnapshot(snapshot)
		}
	case state.ToolCodex:
		if structured != nil {
			next = *structured
		}
	}
	r.setWorking(next)
	if !next {
		r.inspectPreTurn(recent)
	}
}

func (r *runtimeSession) setWorking(next bool) {
	previous, exited := r.session.SetWorking(next)
	now := time.Now()
	r.mu.Lock()
	if !previous && next {
		r.workingStartedAt = now
		r.structuredDone = false
		r.terminalTurnDone = false
		r.preTurnBlocked = false
		r.cancelWaitingLocked()
		r.manager.removeIdleSentinel(r.session.Info().ID)
	}
	if !r.pushWorkingObserved {
		r.pushWorkingObserved = true
		r.mu.Unlock()
		return
	}
	if !previous || next {
		r.mu.Unlock()
		return
	}
	started := r.workingStartedAt
	r.workingStartedAt = time.Time{}
	suppressWaiting := r.structuredDone
	r.structuredDone = false
	authoritativeDone := r.terminalTurnDone
	r.terminalTurnDone = false
	r.cancelWaitingLocked()
	r.mu.Unlock()
	if exited {
		return
	}
	duration := time.Duration(0)
	if !started.IsZero() {
		duration = now.Sub(started)
		if duration < 0 {
			duration = 0
		}
	}
	classification := IdleClassification{Outcome: IdleDone}
	if authoritativeDone {
		classification = r.manager.handleCompletedTurn(r.session, duration)
	} else {
		classification = r.manager.handleIdle(r.session, duration)
	}
	if !supportsTurnLifecycle(r.session.Info()) && classification.Outcome == IdleDone {
		// Shells have an idle edge for hooks and notifications, but remaining
		// alive at a prompt is not a completed session lifecycle state.
		r.session.ClearIdleResult()
	}
	if !suppressWaiting && classification.Outcome == IdleDone {
		r.notifyDone()
	} else if !suppressWaiting {
		r.scheduleWaiting()
	}
}

func (r *runtimeSession) startWatcher(info state.SessionInfo) {
	if info.Kind == state.KindCodexAppServer || info.Kind == state.KindClaudeStructured {
		return
	}
	var watcher *watch.FileWatcher
	switch info.Tool {
	case state.ToolClaude:
		projectsDir := ""
		if info.ConfigDir != "" {
			projectsDir = filepath.Join(info.ConfigDir, "projects")
		}
		// The provider owns this transcript and prunes it on its own
		// schedule, so the watcher keeps Sessions' own copy as it reads.
		// Without it a pruned conversation is simply gone: cat, source,
		// search, and usage all resolve through the same provider path.
		created, err := watch.WatchSessionFile(watch.ClaudeWatcherOptions{
			CWD: info.Cwd, ClaudeSessionID: extractClaudeSessionID(info.Args), ProjectsDir: projectsDir,
			SessionID:  info.ID,
			MirrorPath: watch.TranscriptMirrorPath(r.manager.config.RunnerStateDir, info.ID),
		})
		if err != nil {
			return
		}
		watcher = created
	case state.ToolCodex:
		sessionsDir := ""
		if info.ConfigDir != "" {
			sessionsDir = filepath.Join(info.ConfigDir, "sessions")
		}
		watcherArgs := info.Args
		requireInputMatch := true
		if providerargs.IsConversationUUID(info.ConversationID) {
			// The watcher is not launching Codex, so this synthetic resume argv is
			// only an exact provider-id lookup. Persisting the binding means a
			// daemon restart can rebuild Conversation without waiting for another
			// user message or guessing between same-folder rollouts.
			watcherArgs = []string{"resume", info.ConversationID}
			requireInputMatch = false
		}
		watcher = watch.WatchCodexRollout(watch.CodexWatcherOptions{
			CWD: info.Cwd, Args: watcherArgs, CreatedAt: time.UnixMilli(info.CreatedAt), SessionsDir: sessionsDir,
			RequireInputMatch: requireInputMatch,
		})
		if requireInputMatch && strings.TrimSpace(info.Description) != "" {
			// The desktop stores its initial request as the session description
			// before delivery. On recovery that exact authored text is evidence,
			// not a timestamp guess: the resolver binds only when one provider
			// rollout contains the same user message. CLI descriptions which are
			// merely prose match nothing and remain safely unbound until input.
			watcher.ExpectInput(info.Description)
		}
	default:
		return
	}
	r.mu.Lock()
	r.watcher = watcher
	r.mu.Unlock()
	if !r.manager.startWorker(func() {
		for watcher != nil {
			select {
			case event, ok := <-watcher.Events:
				if !ok {
					return
				}
				if info.Tool == state.ToolCodex {
					r.bindCodexWatcher(watcher.Path())
				}
				raw, err := json.Marshal(event)
				if err == nil {
					r.session.RecordClaudeEvent(raw)
				}
			case working, ok := <-watcher.Working:
				if !ok {
					return
				}
				if !working {
					r.markTerminalTurnDone()
				}
				r.mu.Lock()
				value := working
				r.structuredLifecycleWorking = &value
				r.mu.Unlock()
				r.setWorking(working)
			case _, ok := <-watcher.Errors:
				if !ok {
					return
				}
			case <-r.manager.ctx.Done():
				return
			}
		}
	}) {
		watcher.Close()
	}
}

// bindCodexWatcher makes an exact watcher resolution durable. The resolver
// reaches a path only from a provider UUID already in metadata or from one
// exact submitted-message match. Reading the rollout's session_meta therefore
// promotes provider truth; no terminal text or timestamp heuristic is parsed.
func (r *runtimeSession) bindCodexWatcher(path string) {
	info := r.session.Info()
	if info.Tool != state.ToolCodex || info.ConversationID != "" || path == "" {
		return
	}
	providerID, _, err := watch.ReadCodexConversationIdentity(path)
	if err != nil {
		log.Printf("[provider] read Codex identity for %s: %v", info.ID, err)
		return
	}
	if !providerargs.IsConversationUUID(providerID) {
		// Older or synthetic rollouts may omit the id. Their events remain
		// readable, but there is no identity Sessions can safely persist.
		return
	}
	changed, err := r.manager.registry.BindProviderConversation(info.ID, providerID)
	if err != nil {
		log.Printf("[provider] bind Codex identity for %s: %v", info.ID, err)
		return
	}
	if !changed {
		return
	}
	resumeArgv := ledger.ResumeRecipeForProvider("codex", info.Cmd, providerID)
	r.manager.observe(context.Background(), "provider bound", func(writer ledger.ObservationWriter) error {
		return writer.RecordProviderBound(context.Background(), ledger.ProviderBound{
			Meta: ledger.Meta{LaneID: info.ID}, ProviderUUID: providerID, ResumeArgv: resumeArgv,
		})
	})
}

// followProviderTitle keeps the session's stored name on whatever the provider
// currently calls the conversation.
//
// Every Claude event reaches here, from the structured runner and from the
// transcript watcher alike, and the title records among them have already been
// applied to SessionInfo by the time the event is published. So the common
// case is three in-memory comparisons and no work: the name is the user's, the
// conversation has no title yet, or the title is already the name. Only an
// actual change reaches the registry and touches the metadata file.
//
// The rename is not recorded in the ledger. RecordRenamed attributes its fact
// to ActorUser, and this is the daemon following the provider, not a person
// renaming anything.
func (r *runtimeSession) followProviderTitle() {
	info := r.session.Info()
	if info.NameSource == state.NameSourceExplicit {
		return
	}
	title := state.ProviderConversationTitle(info.ClaudeCustomTitle, info.ClaudeAITitle)
	if title == "" || title == info.Name {
		return
	}
	if _, err := r.manager.registry.AdoptProviderTitle(info.ID, title); err != nil {
		log.Printf("[name] follow provider title for %s: %v", info.ID, err)
	}
}

func supportsTurnLifecycle(info state.SessionInfo) bool {
	return info.Tool == state.ToolClaude || info.Tool == state.ToolCodex
}

// waitReady holds a create until the session can plausibly accept input.
//
// For a provider PTY this used to be a flat readySettle — a timing bet, not a
// signal. It wins on a warm machine and reliably loses on a cold one: Claude
// launched with --remote-control performs a network handshake and only then
// draws its composer, and a first request pasted at the 800ms mark landed
// inside TUI initialization and was eaten by the alt-screen redraw. Every
// layer then reported success. The user typed a message into a new session
// and nothing at all happened, which is the worst first-run Sessions can have.
//
// Readiness is now observed rather than assumed: the program has produced
// output and then stayed quiet for readyQuiet. That definition needs no
// provider version knowledge, and progress UI defends itself — a connecting
// spinner is output, so a session that is still starting keeps itself
// un-ready. A structured runtime's first event remains an earlier, stronger
// answer, and a session that exits stops being waited for.
func (m *Manager) waitReady(ctx context.Context, runtime *runtimeSession) {
	ctx, cancel := context.WithTimeout(ctx, readyCap)
	defer cancel()
	if runtime.session.ClaudeEventCount() > 0 {
		select {
		case <-ctx.Done():
		case <-time.After(readySettle):
		}
		return
	}
	info := runtime.session.Info()
	if info.Tool != state.ToolClaude && info.Tool != state.ToolCodex {
		select {
		case <-ctx.Done():
		case <-time.After(readySettle):
		}
		return
	}
	awaitProviderQuiet(ctx, runtime.structuredEventArrived, func() (int64, bool) {
		current := runtime.session.Info()
		return current.LastDataAt, current.Exited
	}, info.CreatedAt, readySettle, readyQuiet)
}

// awaitProviderQuiet returns once the observed program has produced output and
// then been silent for quiet, but never before floor has elapsed. A program
// that already produced its ready screen answers at the floor, preserving the
// fast case without confusing a never-started child with a quiet one. It
// returns early for a structured event, an exited session, or a spent context;
// it polls rather than subscribing because LastDataAt is a timestamp, not an
// edge, and a tenth of a second of extra latency is nothing against the
// seconds-long initializations this exists to survive.
func awaitProviderQuiet(
	ctx context.Context, structured <-chan struct{},
	snapshot func() (lastDataMS int64, exited bool), createdAtMS int64, floor, quiet time.Duration,
) {
	started := time.Now()
	observedOutput := false
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-structured:
			return
		case <-ticker.C:
		}
		if time.Since(started) < floor {
			continue
		}
		lastData, exited := snapshot()
		if exited {
			return
		}
		// SessionInfo initializes LastDataAt to CreatedAt before the child has
		// written a byte. Treating that initial silence as readiness is how the
		// first prompt was pasted into Claude's later alt-screen initialization
		// and disappeared. Once any real output advances the timestamp, silence
		// after that output is meaningful.
		if lastData != createdAtMS {
			observedOutput = true
		}
		if observedOutput && time.Since(time.UnixMilli(lastData)) >= quiet {
			return
		}
	}
}

// extractClaudeSessionID reads the conversation a Claude spawn was launched
// against. The spellings come from internal/providerargs; this copy used to
// read only the two long flags in separated form, so `claude -r <uuid>` and
// `claude --resume=<uuid>` both looked like sessions with no conversation.
//
// The id-shaped filter stays: this value is handed to the transcript resolver,
// and a non-id argument that happened to follow the flag would send it looking
// for a file that cannot exist. It is deliberately looser than the ledger's
// canonical-UUID rule, which governs what may be durably recorded.
func extractClaudeSessionID(args []string) string {
	for _, value := range providerargs.Values(args, providerargs.ClaudeIdentityFlags()...) {
		if len(value) < 8 {
			continue
		}
		valid := true
		for _, r := range value {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F') || r == '-') {
				valid = false
				break
			}
		}
		if valid {
			return value
		}
	}
	return ""
}

func (m *Manager) scheduleReconnect(id string, delays []time.Duration) {
	if len(delays) == 0 {
		return
	}
	delay := delays[0]
	next := delays
	if len(delays) > 1 {
		next = delays[1:]
	}
	time.AfterFunc(delay, func() {
		select {
		case <-m.ctx.Done():
			return
		default:
		}
		if existing, exists := m.registry.Get(id); exists {
			info := existing.Info()
			if info.Exited || !info.Unreachable {
				return
			}
			// An unreachable registry entry is the durable placeholder, not a
			// live connection. Continue below; RegisterMetadata atomically swaps
			// it for the reattached connection without making the ID disappear.
		}
		path := state.For(m.config.RunnerStateDir, id).Socket
		if !ipc.MayExist(path) {
			if m.reconnectArtifactsExist(id) {
				m.scheduleReconnect(id, next)
			}
			return
		}
		metadata, _ := state.ReadRunnerMetadata(filepath.Join(m.config.RunnerStateDir, id+".json"))
		metadata.Info.ID = id
		metadata.Info.SocketPath = path
		if runner, attachErr := m.launcher.Attach(m.ctx, metadata.Info); attachErr == nil {
			if session, registerErr := m.registry.RegisterMetadata(m.ctx, runner, metadata, ""); registerErr == nil {
				m.manage(session)
				log.Printf("[reconnect] runner %s reattached after unexpected disconnect", id)
				return
			}
		}
		if m.reconnectArtifactsExist(id) {
			m.scheduleReconnect(id, next)
		}
	})
}

func (m *Manager) reconnectArtifactsExist(id string) bool {
	for _, path := range []string{
		filepath.Join(m.config.RunnerStateDir, id+".json"),
		state.RunnerPlistPath(m.config.LaunchAgentsDir, id),
	} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// RunDiscoveryLoop performs the startup scan and then repeats the exact same
// guarded discovery path. SESSIONS_DISCOVERY_INTERVAL accepts Go duration
// syntax (for example, "10s"); invalid and non-positive values keep the safe
// production default.
