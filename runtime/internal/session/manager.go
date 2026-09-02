package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/liveness"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/resource"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

const (
	workingBytesThreshold = 80
	workingDecay          = 800 * time.Millisecond
	discoveryAttempts     = 3
	discoveryRetryDelay   = 800 * time.Millisecond
	orphanStartingGrace   = 30 * time.Second
	readySettle           = 800 * time.Millisecond
	// readyQuiet is how long a PTY provider has to stay silent before its
	// composer is believed to be up. Progress UI keeps a session un-ready on
	// its own: a spinner is output, and output resets the window.
	readyQuiet = 700 * time.Millisecond
	// readyCap bounds waitReady. CONTRACT/http-api.md has promised this cap
	// all along; nothing enforced it, because the wait used to be a flat
	// readySettle that could not run long.
	readyCap                  = 30 * time.Second
	defaultNotifyWaitingDelay = 30 * time.Second
	defaultNotifyCooldown     = 60 * time.Second
	DefaultMassKillLimit      = 3
	DefaultDiscoveryInterval  = 30 * time.Second
	discoveryIntervalEnv      = "SESSIONS_DISCOVERY_INTERVAL"
)

type MassKillGuard struct{ Limit int }

type MassKillError struct {
	Count int
	Limit int
	// Operation names the destructive action that was refused. Empty keeps
	// the default runner-removal wording used by the kill paths.
	Operation string
	// Remedy states what was left untouched and the safe next action. Empty
	// keeps the default force retry, which only callers with a force surface
	// should rely on.
	Remedy string
}

func (e *MassKillError) Error() string {
	operation := e.Operation
	if operation == "" {
		operation = "runner removals"
	}
	remedy := e.Remedy
	if remedy == "" {
		remedy = "retry with force"
	}
	return fmt.Sprintf("mass-kill guard refused %d %s (limit %d); %s", e.Count, operation, e.Limit, remedy)
}

func (g MassKillGuard) Check(count int, force bool) error {
	limit := g.Limit
	if limit <= 0 {
		limit = DefaultMassKillLimit
	}
	if !force && count > limit {
		return &MassKillError{Count: count, Limit: limit}
	}
	return nil
}

type ManagerOptions struct {
	MassKillLimit    int
	ActivityInterval time.Duration
	DiscoveryRetries int
	DiscoveryDelay   time.Duration
	DisableWatchers  bool
	// ProcessAlive and ProcessCommand are test seams over the shared
	// internal/liveness probes. The identity rule they feed
	// (liveness.CommandMatches) is never overridden, so a test can simulate a
	// process table without also redefining what counts as this session's
	// runner. Use Manager.runnerAlive rather than either seam directly.
	ProcessAlive       func(int) bool
	ProcessCommand     func(int) string
	Boundaries         ledger.BoundaryWriter
	Observations       ledger.ObservationWriter
	Retention          ledger.RetentionWriter
	Attributions       ledger.AttributionWriter
	LedgerReader       LedgerReader
	UsageRecorder      UsageRecorder
	Notify             func(PushPayload)
	NotifyWaitingDelay time.Duration
	NotifyCooldown     time.Duration
	ListCodexModels    func(context.Context, string) ([]codexapp.Model, error)
	// ResourceEnumerator is the process-table source used to measure what each
	// session costs the machine. nil means this platform's real one; tests
	// inject a fabricated table through it.
	ResourceEnumerator resource.Enumerator
	// ResourceInterval is the floor between whole-machine samples. It is a
	// floor, not a schedule: sampling rides the activity tick, so the real
	// spacing is the next tick at or after this interval.
	ResourceInterval time.Duration
	// ResourceClock is the clock the CPU rate is measured against. nil means
	// time.Now. Tests set it so elapsed time is exact instead of wall-clock
	// dependent.
	ResourceClock func() time.Time
}

type UsageRecorder interface {
	RecordStructured(context.Context, state.SessionInfo, json.RawMessage) error
}

type DiscoverOptions struct{ Force bool }

type LedgerReader interface {
	Events(context.Context, string) ([]ledger.Event, error)
	LiveBindingFor(context.Context, string) (*ledger.LiveBinding, error)
	MovedBinding(context.Context, string) (*ledger.MovedConversation, error)
}

type ConversationLiveError struct {
	ProviderUUID string
	Binding      ledger.LiveBinding
}

func (e *ConversationLiveError) Error() string {
	return fmt.Sprintf("conversation %s is already live as %q (session %s) — attach with `sessions attach %s`, or re-run with --force to take over.",
		e.ProviderUUID, e.Binding.Name, e.Binding.SessionID, e.Binding.SessionID)
}

type ConversationMovedError struct {
	ProviderUUID string
	Machine      string
}

func (e *ConversationMovedError) Error() string {
	return fmt.Sprintf("conversation moved to %s; reopening here forks it. --force to fork.", e.Machine)
}

type Manager struct {
	config   state.Config
	launcher proto.RunnerLauncher
	// wakeMu serializes WakePaused so two first messages cannot kick one
	// runner twice.
	wakeMu   sync.Mutex
	registry *state.Registry
	push     *PushService
	guard    MassKillGuard
	options  ManagerOptions
	started  time.Time

	boundaries   ledger.BoundaryWriter
	observations ledger.ObservationWriter
	retention    ledger.RetentionWriter
	attributions ledger.AttributionWriter
	ledgerReader LedgerReader
	usage        UsageRecorder
	notify       func(PushPayload)
	listModels   func(context.Context, string) ([]codexapp.Model, error)

	deathMu             sync.Mutex
	laneDeaths          map[string]laneDeathBurst
	notificationMu      sync.Mutex
	notifications       map[string]*sessionNotificationState
	notificationsClosed bool
	discoveryMu         sync.Mutex
	restoreHealthMu     sync.Mutex
	restoreHealthAt     time.Time
	restoreHealthCount  int
	bindMu              sync.Mutex
	// completionGeneration records the newest delegated-task completion
	// attempt per session so a fresh idle classification supersedes an
	// in-flight one instead of racing it.
	completionMu         sync.Mutex
	completionGeneration map[string]uint64

	ctx    context.Context
	cancel context.CancelFunc
	ticker *time.Ticker

	workerMu      sync.Mutex
	workerWG      sync.WaitGroup
	workersClosed bool

	mu       sync.Mutex
	runtimes map[string]*runtimeSession
	hooks    globalHooks

	// resources is the per-session memory and CPU sampler. It rides the
	// activity loop rather than owning a goroutine, and it is guarded by
	// resourceMu because Close and the loop can both touch the schedule.
	resources        *resource.Tracker
	resourceInterval time.Duration
	resourceClock    func() time.Time
	resourceMu       sync.Mutex
	resourceSampled  time.Time
	// resourceFailed suppresses repeated logging of the same enumeration
	// failure. A platform that cannot sample says so once, not every tick.
	resourceFailed bool
}

type laneDeathBurst struct {
	started  time.Time
	count    int
	digested bool
}

type globalHooks struct {
	OnIdle string `json:"onIdle"`
}

type runtimeSession struct {
	manager    *Manager
	session    *state.Session
	attachment state.Attachment

	mu                         sync.Mutex
	recentBytes                int
	structuredLifecycleWorking *bool
	pushWorkingObserved        bool
	workingStartedAt           time.Time
	structuredDone             bool
	terminalTurnDone           bool
	waitingTimer               *time.Timer
	waitingGeneration          uint64
	stopped                    bool
	watcher                    *watch.FileWatcher
	stopOnce                   sync.Once
	outputObserved             chan struct{}
	structuredEventArrived     chan struct{}
	firstMessageInput          []byte
	firstMessageDone           bool
	providerInput              []byte
	// preTurnOutput records terminal output that arrived while the session
	// has not yet completed a turn, so a provider dialog drawn before the
	// first request (Claude's folder-trust screen, a login prompt) can be
	// classified without waiting for a working-to-idle edge that never comes.
	preTurnOutput      bool
	preTurnBlocked     bool
	preTurnInspectedAt time.Time
}

func NewManager(config state.Config, launcher proto.RunnerLauncher, options ...ManagerOptions) *Manager {
	if migrated, err := state.MigrateRunnerPlistRestartPolicies(config.LaunchAgentsDir, config.RunnerStateDir); err != nil {
		log.Printf("runner restart policy migration: %v", err)
	} else if migrated > 0 {
		log.Printf("runner restart policy: bounded login restore enabled for %d existing session(s)", migrated)
	}
	selected := ManagerOptions{}
	if len(options) > 0 {
		selected = options[0]
	}
	if selected.ActivityInterval <= 0 {
		selected.ActivityInterval = workingDecay
	}
	if selected.NotifyWaitingDelay <= 0 {
		selected.NotifyWaitingDelay = defaultNotifyWaitingDelay
	}
	if selected.NotifyCooldown <= 0 {
		selected.NotifyCooldown = defaultNotifyCooldown
	}
	if selected.DiscoveryRetries <= 0 {
		selected.DiscoveryRetries = discoveryAttempts
	}
	if selected.DiscoveryDelay <= 0 {
		selected.DiscoveryDelay = discoveryRetryDelay
	}
	if selected.ProcessAlive == nil {
		selected.ProcessAlive = liveness.ProcessAlive
	}
	if selected.ProcessCommand == nil {
		selected.ProcessCommand = func(pid int) string {
			return liveness.ProcessCommand(context.Background(), pid)
		}
	}
	if selected.ResourceEnumerator == nil {
		selected.ResourceEnumerator = resource.SystemEnumerator()
	}
	if selected.ResourceInterval <= 0 {
		selected.ResourceInterval = resource.DefaultInterval
	}
	if selected.ResourceClock == nil {
		selected.ResourceClock = time.Now
	}
	root := config.UserStateRoot
	if root == "" {
		root = config.StateRoot
	}
	ctx, cancel := context.WithCancel(context.Background())
	manager := &Manager{
		config: config, launcher: launcher, registry: state.NewRegistry(config, launcher),
		push: NewPushService(root), guard: MassKillGuard{Limit: selected.MassKillLimit},
		options: selected, started: time.Now(), ctx: ctx, cancel: cancel,
		boundaries: selected.Boundaries, observations: selected.Observations,
		retention: selected.Retention, attributions: selected.Attributions,
		ledgerReader: selected.LedgerReader,
		usage:        selected.UsageRecorder,
		runtimes:     make(map[string]*runtimeSession), hooks: loadGlobalHooks(config.GlobalHooksPath),
		laneDeaths: make(map[string]laneDeathBurst), notifications: make(map[string]*sessionNotificationState),
		completionGeneration: make(map[string]uint64),
	}
	manager.resources = resource.NewTracker(selected.ResourceEnumerator, selected.ResourceClock)
	manager.resourceInterval = selected.ResourceInterval
	manager.resourceClock = selected.ResourceClock
	manager.listModels = selected.ListCodexModels
	if manager.listModels == nil {
		manager.listModels = listLiveCodexModels
	}
	manager.notify = selected.Notify
	if manager.notify == nil {
		manager.notify = func(payload PushPayload) {
			manager.startWorker(func() {
				manager.push.Send(manager.ctx, payload)
			})
		}
	}
	manager.registry.SetTerminalObservers(manager.recordRunnerExited, manager.recordReaped)
	manager.recordDaemonRestart(ctx)
	manager.ticker = time.NewTicker(selected.ActivityInterval)
	manager.startWorker(manager.activityLoop)
	return manager
}

func (m *Manager) startWorker(run func()) bool {
	m.workerMu.Lock()
	defer m.workerMu.Unlock()
	if m.workersClosed {
		return false
	}
	m.workerWG.Add(1)
	go func() {
		defer m.workerWG.Done()
		run()
	}()
	return true
}

func (m *Manager) Registry() *state.Registry { return m.registry }
func (m *Manager) Push() *PushService        { return m.push }
func (m *Manager) Config() state.Config      { return m.config }
func (m *Manager) Uptime() time.Duration     { return time.Since(m.started) }
func (m *Manager) IsDiscovering() bool       { return m.registry.IsDiscovering() }
func (m *Manager) List(includeExited bool) []state.SessionInfo {
	ctx := context.Background()
	infos := m.registry.List(includeExited)
	// Restored unconditionally, not only for the include-ended listing: a
	// session the daemon cannot currently reach has not ended, and dropping it
	// from the default list because a socket died is a kill wearing sleep's
	// clothes. withDurableClosed adds ended records only when they were asked
	// for.
	infos = m.withDurableClosed(ctx, infos, includeExited)
	infos = m.withPendingRestores(infos)
	return m.withProvenance(ctx, infos)
}
func (m *Manager) Get(id string) (*state.Session, bool) { return m.registry.Get(id) }
func (m *Manager) DeepDiagnostics() []map[string]any    { return m.registry.DeepDiagnostics() }
func (m *Manager) UpdateTags(id string, tags map[string]string) (map[string]string, error) {
	return m.registry.UpdateTags(id, tags)
}
func (m *Manager) UpdateName(ctx context.Context, id, name string) (string, error) {
	updated, err := m.registry.UpdateName(id, name)
	if err != nil {
		return "", err
	}
	if m.observations == nil {
		return updated, nil
	}
	if err := m.observations.RecordRenamed(ctx, ledger.Rename{
		Meta: ledger.Meta{LaneID: id}, Name: updated,
	}); err != nil {
		return "", fmt.Errorf("session was renamed to %q, but the durable history annotation failed: %w", updated, err)
	}
	return updated, nil
}

// ReleaseName undoes an explicit rename's claim on the card, so the session
// goes back to following the provider's conversation title. No ledger fact is
// written: the rename that is being released is already recorded, and this
// does not assert a new name of its own.
func (m *Manager) ReleaseName(ctx context.Context, id string) (string, error) {
	return m.registry.ReleaseName(id)
}
func (m *Manager) Tags(id string) (map[string]string, error) {
	return m.registry.Tags(id)
}

// UpdateDisplayParent changes only the user's visual grouping. The trusted
// creator ledger remains authoritative for who actually created the session.
func (m *Manager) UpdateDisplayParent(id, parentID string) (string, error) {
	infos := m.List(true)
	byID := make(map[string]state.SessionInfo, len(infos))
	for _, info := range infos {
		byID[info.ID] = info
	}
	if _, exists := byID[id]; !exists {
		return "", fmt.Errorf("%w: session %s", state.ErrSessionNotFound, id)
	}
	if parentID != "" {
		if _, exists := byID[parentID]; !exists {
			return "", fmt.Errorf("%w: parent session %s", state.ErrSessionNotFound, parentID)
		}
	}

	// Follow the effective display hierarchy from the proposed parent. A
	// repeated node means the existing graph is already malformed; reaching
	// id means this edit would introduce a cycle.
	visited := make(map[string]struct{}, len(infos))
	for current := parentID; current != ""; {
		if current == id {
			return "", errors.New("a session cannot be grouped under itself or one of its descendants")
		}
		if _, repeated := visited[current]; repeated {
			return "", errors.New("session display hierarchy already contains a cycle")
		}
		visited[current] = struct{}{}
		info, exists := byID[current]
		if !exists {
			break
		}
		if info.DisplayParentSessionID != nil {
			current = *info.DisplayParentSessionID
		} else {
			current = info.ParentSessionID
		}
	}
	return m.registry.UpdateDisplayParent(id, parentID)
}

// UpdateSetAside changes only default working-set visibility. Ended records
// use Archive instead; this keeps presentation organization orthogonal to
// lifecycle and retention.
func (m *Manager) UpdateSetAside(id string, setAside bool) (*int64, error) {
	var current *state.SessionInfo
	for _, info := range m.List(true) {
		if info.ID == id {
			candidate := info
			current = &candidate
			break
		}
	}
	if current == nil {
		return nil, fmt.Errorf("%w: session %s", state.ErrSessionNotFound, id)
	}
	if current.Exited {
		return nil, fmt.Errorf("%w; use archive to hide an ended record", state.ErrSessionEnded)
	}
	return m.registry.UpdateSetAside(id, setAside)
}

// UpdatePinned records the user marking a session as a workbench: it sorts
// first in every listing and the automatic machinery keeps its hands off it.
// It changes no runner state and starts or stops nothing.
//
// An ended record is refused the same way set-aside refuses one, and for a
// sharper reason: the two things a pin does are exempt a session from being
// ended automatically and keep it near the top of the working set. Neither is
// available to a conversation that has already ended, so accepting the pin
// would acknowledge a protection that does not exist. Archive is the verb for
// an ended record.
func (m *Manager) UpdatePinned(id string, pinned bool) (bool, error) {
	var current *state.SessionInfo
	for _, info := range m.List(true) {
		if info.ID == id {
			candidate := info
			current = &candidate
			break
		}
	}
	if current == nil {
		return false, fmt.Errorf("%w: session %s", state.ErrSessionNotFound, id)
	}
	if current.Exited {
		return false, fmt.Errorf("%w; a pin exempts a live session from automatic "+
			"termination and cannot protect one that already ended, so use archive to "+
			"hide the record instead", state.ErrSessionEnded)
	}
	return m.registry.UpdatePinned(id, pinned)
}

func (m *Manager) recordCreated(ctx context.Context, prepared state.PreparedSession, creatorKind ledger.CreatorKind, creatorID string) error {
	if m.boundaries == nil {
		return nil
	}
	info := prepared.Info
	providerUUID, resumeArgv := "", []string(nil)
	if prepared.Kind != state.KindLane {
		providerUUID, resumeArgv = ledger.SafeResumeRecipe(string(prepared.Tool), info.Cmd, info.Args)
	}
	if err := m.boundaries.RecordCreated(ctx, ledger.Created{
		Meta: ledger.Meta{LaneID: info.ID, AtMS: info.CreatedAt},
		Name: prepared.Name, Description: prepared.Description,
		DescriptionSource: ledger.DescriptionSource(prepared.DescriptionSource),
		Kind:              prepared.Kind, Tool: string(prepared.Tool), Cwd: info.Cwd,
		Profile: prepared.Profile, ConfigDir: prepared.ConfigDir,
		WorktreePath: prepared.WorktreePath, Branch: prepared.WorktreeBranch,
		Base: prepared.WorktreeBase, SourceRepo: prepared.SourceRepo,
		ResumeArgv: resumeArgv, LaneUUID: info.ID, ProviderUUID: providerUUID,
		CreatorKind: creatorKind, CreatorID: creatorID, DelegationKind: prepared.DelegationKind,
	}); err != nil {
		return fmt.Errorf("record lane creation before launch: %w", err)
	}
	return nil
}

func (m *Manager) resolveCreator(ctx context.Context, request state.CreateSessionRequest) (ledger.CreatorKind, string, error) {
	if request.CreatorSessionID != "" && request.CreatorOwnerID != "" {
		return "", "", errors.New("creator session and external owner cannot both be set")
	}
	if request.CreatorOwnerID != "" {
		if err := ledger.ValidateCreator(ledger.CreatorExternal, request.CreatorOwnerID); err != nil {
			return "", "", err
		}
		return ledger.CreatorExternal, request.CreatorOwnerID, nil
	}
	if request.CreatorSessionID == "" {
		id, err := ledger.LocalUserCreatorID()
		return ledger.CreatorUser, id, err
	}
	if err := ledger.ValidateCreator(ledger.CreatorSession, request.CreatorSessionID); err != nil {
		return "", "", err
	}
	if m.ledgerReader == nil {
		return "", "", errors.New("cannot validate creator session: ledger reader is unavailable")
	}
	events, err := m.ledgerReader.Events(ctx, request.CreatorSessionID)
	if err != nil {
		return "", "", fmt.Errorf("validate creator session: %w", err)
	}
	for _, candidate := range ledger.Fold(events) {
		if candidate.LaneID == request.CreatorSessionID && candidate.Created {
			if candidate.Archived {
				return "", "", fmt.Errorf("creator session %s has been archived", request.CreatorSessionID)
			}
			return ledger.CreatorSession, request.CreatorSessionID, nil
		}
	}
	return "", "", fmt.Errorf("creator session %s has no created event", request.CreatorSessionID)
}

func (m *Manager) recordLaunchStarted(ctx context.Context, prepared state.PreparedSession) {
	m.observe(ctx, "launch started", func(writer ledger.ObservationWriter) error {
		return writer.RecordLaunchStarted(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: prepared.Info.ID}})
	})
}

func (m *Manager) recordRunnerReady(ctx context.Context, info proto.RunnerInfo) {
	m.observe(ctx, "runner ready", func(writer ledger.ObservationWriter) error {
		return writer.RecordRunnerReady(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: info.ID}})
	})
	if metadata, err := state.ReadRunnerMetadata(filepath.Join(m.config.RunnerStateDir, info.ID+".json")); err == nil {
		if metadata.Kind == state.KindLane {
			return
		}
		if metadata.Kind == state.KindCodexAppServer && info.ConversationID != "" {
			resumeArgv := ledger.ResumeRecipeForProvider("codex", info.Cmd, info.ConversationID)
			m.observe(ctx, "provider bound", func(writer ledger.ObservationWriter) error {
				return writer.RecordProviderBound(ctx, ledger.ProviderBound{
					Meta: ledger.Meta{LaneID: info.ID}, ProviderUUID: info.ConversationID, ResumeArgv: resumeArgv,
				})
			})
			return
		}
		if metadata.Kind == state.KindClaudeStructured && info.ClaudeSessionID != "" {
			resumeArgv := ledger.ResumeRecipeForProvider("claude-code", info.Cmd, info.ClaudeSessionID)
			m.observe(ctx, "provider bound", func(writer ledger.ObservationWriter) error {
				return writer.RecordProviderBound(ctx, ledger.ProviderBound{
					Meta: ledger.Meta{LaneID: info.ID}, ProviderUUID: info.ClaudeSessionID, ResumeArgv: resumeArgv,
				})
			})
			return
		}
	}
	providerUUID, resumeArgv := ledger.SafeResumeRecipe("", info.Cmd, info.Args)
	if providerUUID == "" {
		return
	}
	m.observe(ctx, "provider bound", func(writer ledger.ObservationWriter) error {
		return writer.RecordProviderBound(ctx, ledger.ProviderBound{
			Meta: ledger.Meta{LaneID: info.ID}, ProviderUUID: providerUUID, ResumeArgv: resumeArgv,
		})
	})
}

func (m *Manager) recordReaped(id string) {
	m.observe(context.Background(), "reaped", func(writer ledger.ObservationWriter) error {
		return writer.RecordReaped(context.Background(), ledger.Observation{Meta: ledger.Meta{LaneID: id}})
	})
}

func (m *Manager) recordRunnerExited(id string, event proto.ExitEvent) {
	m.observe(context.Background(), "runner exited", func(writer ledger.ObservationWriter) error {
		return writer.RecordRunnerExited(context.Background(), ledger.RunnerExit{
			Meta: ledger.Meta{LaneID: id}, Code: event.Code, Signal: event.Signal,
		})
	})
	m.notifyLaneExit(id, event)
}

func (m *Manager) notifyLaneExit(id string, event proto.ExitEvent) {
	session, ok := m.registry.Get(id)
	if !ok {
		return
	}
	info := session.Info()
	if info.Kind != state.KindLane {
		return
	}
	manifest, err := state.ReadCompletionManifest(filepath.Join(m.config.RunnerStateDir, id+".manifest.json"))
	if err != nil {
		manifest.ExitCode = exitCodeOf(event)
		manifest.Signal = event.Signal
		manifest.SpecPath = info.SpecPath
		if snapshot, _, snapshotErr := session.Snapshot(context.Background(), 0); snapshotErr == nil {
			manifest.LastOutputTail = snapshot
		}
	}
	label := sessionDisplayLabel(info)
	body := lastOutputLine(manifest.LastOutputTail)
	failed := manifest.ExitCode != 0 || manifest.Signal != nil
	if !failed {
		if body == "" {
			body = "finished"
		}
		m.notify(PushPayload{
			Title: "🟢 " + label + " finished", Body: body,
			Data: map[string]any{"sessionId": id, "kind": state.KindLane, "exitCode": manifest.ExitCode},
		})
		return
	}
	if body == "" {
		body = "no output"
	}
	payload := PushPayload{
		Title: fmt.Sprintf("🔴 %s died (exit %d)", label, manifest.ExitCode), Body: body,
		Data: map[string]any{"sessionId": id, "kind": state.KindLane, "exitCode": manifest.ExitCode},
	}
	signature := fmt.Sprintf("exit:%d", manifest.ExitCode)
	if manifest.Signal != nil {
		signature += ":signal:" + *manifest.Signal
	}
	now := time.Now()
	m.deathMu.Lock()
	burst := m.laneDeaths[signature]
	if burst.started.IsZero() || now.Sub(burst.started) >= time.Minute {
		burst = laneDeathBurst{started: now}
	}
	burst.count++
	send := true
	if burst.count >= 3 {
		if burst.digested {
			send = false
		} else {
			burst.digested = true
			payload.Title = fmt.Sprintf("%d lanes died", burst.count)
			payload.Body = fmt.Sprintf("similar exit %d within 60s", manifest.ExitCode)
			payload.Data = map[string]any{"kind": state.KindLane, "exitCode": manifest.ExitCode, "count": burst.count}
		}
	}
	m.laneDeaths[signature] = burst
	m.deathMu.Unlock()
	if send {
		m.notify(payload)
	}
}

func exitCodeOf(event proto.ExitEvent) int {
	if event.Code != nil {
		return *event.Code
	}
	return 1
}

func lastOutputLine(output string) string {
	lines := snapshotLines(output)
	if len(lines) == 0 {
		return ""
	}
	return displayLine(lines[len(lines)-1])
}

func (m *Manager) observe(ctx context.Context, label string, record func(ledger.ObservationWriter) error) {
	if m.observations == nil {
		return
	}
	if err := record(m.observations); err != nil {
		log.Printf("[ledger] record %s: %v", label, err)
	}
}

func (m *Manager) ledgerStates(ctx context.Context) ([]ledger.LaneState, error) {
	if m.ledgerReader == nil {
		return nil, nil
	}
	events, err := m.ledgerReader.Events(ctx, "")
	if err != nil {
		return nil, err
	}
	return ledger.Fold(events), nil
}
