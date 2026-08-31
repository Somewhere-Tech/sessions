package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/somewhere-tech/sessions/runtime/internal/claudep"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ipc"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/liveness"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerargs"
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

// withDurableClosed restores closed records after Registry's short exited
// grace has elapsed. The ledger is authoritative for lifecycle and ownership;
// live runtime details continue to come from Registry while they are present.
func (m *Manager) withDurableClosed(
	ctx context.Context, infos []state.SessionInfo, includeEnded bool,
) []state.SessionInfo {
	states, err := m.ledgerStates(ctx)
	if err != nil {
		log.Printf("[ledger] read durable closed sessions: %v", err)
		return infos
	}
	archived := make(map[string]struct{})
	for _, lane := range states {
		if lane.Archived {
			archived[lane.LaneID] = struct{}{}
		}
	}
	filtered := make([]state.SessionInfo, 0, len(infos))
	for _, info := range infos {
		if _, hidden := archived[info.ID]; !hidden {
			filtered = append(filtered, info)
		}
	}
	infos = filtered
	seen := make(map[string]struct{}, len(infos))
	for _, info := range infos {
		seen[info.ID] = struct{}{}
	}
	for _, lane := range states {
		// A merely unreachable lane is still restored to the listing -- sleep
		// and a lost socket must never make work disappear -- but it is
		// restored as unreachable, not as ended. Only a reaped status makes a
		// session ended.
		if !lane.Created || lane.Archived || !(durablyClosed(lane) || lane.RunnerLost) {
			continue
		}
		if durablyClosed(lane) && !includeEnded {
			continue
		}
		if _, exists := seen[lane.LaneID]; exists {
			continue
		}
		exitedAt := lane.ClosedAtMS
		if exitedAt == 0 {
			exitedAt = lane.LastEventAtMS
		}
		info := state.SessionInfo{
			ID: lane.LaneID, Name: lane.Name, Description: lane.Description,
			DescriptionSource: string(lane.DescriptionSource), Cwd: lane.Cwd,
			Profile: lane.Profile, ConfigDir: lane.ConfigDir,
			WorktreePath: lane.WorktreePath, Branch: lane.Branch, Base: lane.Base, SourceRepo: lane.SourceRepo,
			CreatedAt: lane.CreatedAtMS, LastDataAt: lane.LastEventAtMS,
			Kind: lane.Kind, Tool: state.SessionTool(lane.Tool),
			EndedByKind: string(lane.EndInitiatorKind), EndedByID: lane.EndInitiatorID,
			EndedByName: lane.EndInitiatorName, EndedByClient: lane.EndClient,
			EndReason: lane.EndReason, EndOperationID: lane.EndOperationID,
		}
		if durablyClosed(lane) {
			info.Exited = true
			info.ExitedAt = &exitedAt
			info.ExitCode = lane.ExitCode
			info.ExitSignal = lane.ExitSignal
			info.ExitReason = durableExitReason(lane)
		} else {
			// lane.RunnerLost with no reaped status: the daemon lost contact
			// and never observed an ending. Synthesising Exited:true with a
			// nil ExitCode here is what put live sessions in the list as
			// ended-and-failed.
			lostAt := exitedAt
			info.Unreachable = true
			info.UnreachableReason = "runner-lost"
			info.UnreachableSince = &lostAt
		}
		if len(lane.ResumeArgv) > 0 {
			info.Cmd = lane.ResumeArgv[0]
			info.Args = append([]string(nil), lane.ResumeArgv[1:]...)
		}
		info.ConversationID = lane.ProviderUUID
		if info.Tool == state.ToolClaude {
			info.ClaudeSessionID = lane.ProviderUUID
		}
		if lane.Tool == string(state.ToolLane) {
			info.Kind = state.KindLane
		}
		if metadata, metadataErr := state.ReadRunnerMetadata(filepath.Join(m.config.RunnerStateDir, lane.LaneID+".json")); metadataErr == nil {
			info.Tags = state.CloneTags(metadata.Tags)
			if metadata.DisplayParentSessionID != nil {
				displayParent := *metadata.DisplayParentSessionID
				info.DisplayParentSessionID = &displayParent
			}
			if metadata.SetAsideAt != nil {
				setAsideAt := *metadata.SetAsideAt
				info.SetAsideAt = &setAsideAt
			}
			info.Pinned = metadata.Pinned
		}
		infos = append(infos, info)
		seen[lane.LaneID] = struct{}{}
	}
	return infos
}

// durablyClosed reports that a lane reached an end Sessions actually observed:
// the user asked for it, the runner reported a status, the artifacts were
// reaped, or the conversation was continued elsewhere.
//
// RunnerLost is deliberately absent. It records that the daemon could not
// reach a runner, which happens to healthy runners whenever a socket read
// fails or the daemon restarts. ORing it in here meant one lost connection
// permanently closed a session that never stopped working -- and on the
// owner's machine two live runners, probed alive at pids 43014 and 22440,
// carried runner_lost facts and were listed as ended.
func durablyClosed(lane ledger.LaneState) bool {
	return lane.UserKillRequested || lane.RunnerExited || lane.Reaped || lane.ReopenedAs != ""
}

func durableExitReason(lane ledger.LaneState) string {
	switch {
	case lane.UserKillRequested:
		return "ended-by-user"
	case lane.ReopenedAs != "":
		return "continued"
	case lane.RunnerLost:
		return "runner-lost"
	case lane.ExitSignal != nil && *lane.ExitSignal != "":
		return "signaled"
	case lane.ExitCode != nil && *lane.ExitCode != 0:
		return "failed"
	case lane.ExitCode != nil && *lane.ExitCode == 0:
		return "completed"
	case lane.RunnerExited:
		return "ended"
	case lane.Reaped:
		return "reaped"
	default:
		return "ended"
	}
}

func (m *Manager) withProvenance(ctx context.Context, infos []state.SessionInfo) []state.SessionInfo {
	if m.ledgerReader == nil || len(infos) == 0 {
		return infos
	}
	states, err := m.ledgerStates(ctx)
	if err != nil {
		log.Printf("[ledger] read provenance graph: %v", err)
		return infos
	}
	byID := make(map[string]ledger.LaneState, len(states))
	for _, candidate := range states {
		byID[candidate.LaneID] = candidate
	}
	for index := range infos {
		current, exists := byID[infos[index].ID]
		if !exists || !current.Created {
			continue
		}
		if current.Name != "" {
			infos[index].Name = current.Name
		}
		if infos[index].Exited {
			derived := durableExitReason(current)
			if infos[index].ExitReason == "" || (infos[index].ExitReason == "ended" && derived != "ended") {
				infos[index].ExitReason = derived
			}
		}
		if infos[index].DescriptionSource != state.DescriptionExplicit && current.Description != "" {
			infos[index].Description = current.Description
			infos[index].DescriptionSource = string(current.DescriptionSource)
		}
		infos[index].CreatorKind = string(current.CreatorKind)
		infos[index].CreatorID = current.CreatorID
		infos[index].DelegationKind = current.DelegationKind
		infos[index].Profile = current.Profile
		infos[index].ConfigDir = current.ConfigDir
		infos[index].WorktreePath = current.WorktreePath
		infos[index].Branch = current.Branch
		infos[index].Base = current.Base
		infos[index].SourceRepo = current.SourceRepo
		infos[index].ReopenedAs = current.ReopenedAs
		infos[index].MovedToEndpoint = current.MovedToMachine
		infos[index].MovedToSessionID = current.MovedToLaneID
		infos[index].MovedFromEndpoint = current.MovedFromMachine
		infos[index].MovedFromSessionID = current.MovedFromLaneID
		infos[index].EndedByKind = string(current.EndInitiatorKind)
		infos[index].EndedByID = current.EndInitiatorID
		infos[index].EndedByName = current.EndInitiatorName
		infos[index].EndedByClient = current.EndClient
		infos[index].EndReason = current.EndReason
		infos[index].EndOperationID = current.EndOperationID
		if current.CreatorKind == ledger.CreatorSession {
			infos[index].ParentSessionID = current.CreatorID
		}

		visited := map[string]struct{}{current.LaneID: {}}
		parentDead := false
		for current.CreatorKind == ledger.CreatorSession {
			parentID := current.CreatorID
			infos[index].CreatorAncestry = append(infos[index].CreatorAncestry, parentID)
			if _, cycle := visited[parentID]; cycle {
				infos[index].ProvenanceStatus = "cycle"
				break
			}
			visited[parentID] = struct{}{}
			parent, found := byID[parentID]
			if !found || !parent.Created {
				infos[index].RootCreatorKind = string(ledger.CreatorSession)
				infos[index].RootCreatorID = parentID
				infos[index].ProvenanceStatus = "parent-missing"
				break
			}
			if provenanceParentDead(parent) {
				parentDead = true
			}
			current = parent
		}
		if infos[index].RootCreatorKind == "" && current.CreatorKind != "" {
			infos[index].RootCreatorKind = string(current.CreatorKind)
			infos[index].RootCreatorID = current.CreatorID
		}
		if parentDead {
			infos[index].ProvenanceStatus = "parent-dead"
		} else if infos[index].ProvenanceStatus == "" {
			if len(infos[index].CreatorAncestry) > 0 {
				infos[index].ProvenanceStatus = "parent-live"
			} else if current.CreatorKind != "" {
				infos[index].ProvenanceStatus = "rooted"
			} else {
				infos[index].ProvenanceStatus = "legacy"
			}
		}
	}
	for sourceID, current := range byID {
		if current.ReopenedAs == "" {
			continue
		}
		for index := range infos {
			if infos[index].ID == current.ReopenedAs {
				infos[index].ResumedFrom = sourceID
				break
			}
		}
	}
	return infos
}

// provenanceParentDead answers whether a child's parent actually ended. Like
// durablyClosed it excludes RunnerLost: a parent the daemon briefly could not
// reach is not a dead parent, and labelling a child "parent-dead" on that
// basis is the same inference wearing a different name.
func provenanceParentDead(parent ledger.LaneState) bool {
	return parent.UserKillRequested || parent.RunnerExited || parent.Reaped
}

func (m *Manager) recordDaemonRestart(ctx context.Context) {
	lanes, err := m.ledgerStates(ctx)
	if err != nil {
		log.Printf("[ledger] read lanes for daemon restart: %v", err)
		return
	}
	for _, lane := range lanes {
		laneID := lane.LaneID
		m.observe(ctx, "daemon restart", func(writer ledger.ObservationWriter) error {
			return writer.RecordDaemonRestart(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: laneID}})
		})
	}
}

func (m *Manager) reconcileLedger(ctx context.Context) {
	lanes, err := m.ledgerStates(ctx)
	if err != nil {
		log.Printf("[ledger] read lanes for discovery reconciliation: %v", err)
		return
	}
	for _, lane := range lanes {
		closed := lane.UserKillRequested || lane.RunnerExited || lane.Reaped
		if !lane.Created || closed || lane.RunnerLost {
			continue
		}
		if _, present := m.registry.Get(lane.LaneID); present {
			continue
		}
		// Absence from the in-memory map is not death. The map holds the
		// runners this daemon process currently has sockets to; a runner that
		// outlived a daemon restart, or whose socket blipped before discovery
		// reattached it, is missing from it while its process runs perfectly
		// well. Recording a loss on that basis wrote a permanent ledger fact
		// about a live session. Ask the process.
		metadata, metadataErr := state.ReadRunnerMetadata(
			filepath.Join(m.config.RunnerStateDir, lane.LaneID+".json"))
		switch {
		case metadataErr == nil:
			metadata.Info.ID = lane.LaneID
			if m.runnerAlive(lane.LaneID, metadata.Info) {
				log.Printf(
					"[ledger] lane %s is absent from the runtime map but pid %d is alive — recording no loss",
					lane.LaneID, metadata.Info.PID)
				continue
			}
		case !errors.Is(metadataErr, os.ErrNotExist):
			// A torn, unreadable, or forward-version metadata document is
			// absence of evidence. Say nothing and retry on the next pass.
			log.Printf("[ledger] lane %s metadata unreadable — recording no loss: %v", lane.LaneID, metadataErr)
			continue
		}
		laneID := lane.LaneID
		m.observe(ctx, "runner lost during discovery reconciliation", func(writer ledger.ObservationWriter) error {
			return writer.RecordRunnerLost(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: laneID}})
		})
		info := state.SessionInfo{ID: lane.LaneID, Name: lane.Name, Cwd: lane.Cwd, Tool: state.SessionTool(lane.Tool)}
		if lane.Tool == string(state.ToolLane) {
			info.Kind = state.KindLane
		}
		if len(lane.ResumeArgv) > 0 {
			info.Cmd = lane.ResumeArgv[0]
		}
		m.notifyLost(info)
	}
}

func (m *Manager) Create(ctx context.Context, request state.CreateSessionRequest) (state.SessionInfo, error) {
	if request.Profile != "" {
		configDir, err := m.prepareProfile(request.Cmd, request.Profile)
		if err != nil {
			return state.SessionInfo{}, err
		}
		request.ConfigDir = configDir
	}
	resolvedRuntimeRequest, err := resolveDelegatedRuntimeDefault(request)
	if err != nil {
		return state.SessionInfo{}, err
	}
	request = resolvedRuntimeRequest
	resolvedClaudeRequest, err := m.applyClaudeDefaults(request)
	if err != nil {
		return state.SessionInfo{}, err
	}
	request = resolvedClaudeRequest
	resolvedRequest, err := m.resolveCodexModelChoice(ctx, request)
	if err != nil {
		return state.SessionInfo{}, err
	}
	request = resolvedRequest

	// Serialize the ledger query with the pre-launch binding write. Without
	// this lock two concurrent resume requests could both observe no owner.
	m.bindMu.Lock()
	defer m.bindMu.Unlock()

	creatorKind, creatorID, err := m.resolveCreator(ctx, request)
	if err != nil {
		return state.SessionInfo{}, fmt.Errorf("resolve lane creator: %w", err)
	}
	if creatorKind == ledger.CreatorSession {
		if request.DelegationKind == "" {
			request.DelegationKind = "agent"
		}
		if request.DelegationKind != "user" && request.DelegationKind != "agent" {
			return state.SessionInfo{}, errors.New("delegation kind must be user or agent")
		}
	} else if request.DelegationKind != "" {
		return state.SessionInfo{}, errors.New("delegation kind requires a parent session")
	}
	request, err = m.resolveDelegatedExecution(ctx, request, creatorKind, creatorID)
	if err != nil {
		return state.SessionInfo{}, fmt.Errorf("resolve delegated execution: %w", err)
	}

	providerUUID, _ := ledger.ExistingProviderResume(request.Cmd, request.Args)
	var takeover *ledger.LiveBinding
	if providerUUID != "" && m.ledgerReader != nil {
		binding, err := m.ledgerReader.LiveBindingFor(ctx, providerUUID)
		if err != nil {
			return state.SessionInfo{}, fmt.Errorf("check live conversation binding: %w", err)
		}
		moved, err := m.ledgerReader.MovedBinding(ctx, providerUUID)
		if err != nil {
			return state.SessionInfo{}, fmt.Errorf("check moved conversation binding: %w", err)
		}
		if moved != nil && !request.Force {
			return state.SessionInfo{}, &ConversationMovedError{ProviderUUID: providerUUID, Machine: moved.Machine}
		}
		if binding != nil && !request.Force {
			return state.SessionInfo{}, &ConversationLiveError{ProviderUUID: providerUUID, Binding: *binding}
		}
		if binding != nil {
			takeover = binding
		} else if moved != nil {
			// A moved source can be tombstoned locally while its remote driver
			// remains live. Record the forced fork against that source lane.
			takeover = &ledger.LiveBinding{SessionID: moved.SourceSessionID}
		}
	}

	var preparedWorktree *createdWorktree
	if request.Worktree {
		if m.boundaries == nil || m.ledgerReader == nil {
			return state.SessionInfo{}, errors.New("--worktree requires the Sessions ledger, but ledger access is unavailable; restore the daemon ledger and retry")
		}
		sourceCwd := request.Cwd
		if strings.TrimSpace(sourceCwd) == "" {
			sourceCwd = m.config.DefaultCwd
		}
		worktree, err := createGitWorktree(ctx, sourceCwd, request.Name, request.Base)
		if err != nil {
			return state.SessionInfo{}, err
		}
		request.Cwd = worktree.Path
		request.WorktreePath = worktree.Path
		request.WorktreeBranch = worktree.Branch
		request.WorktreeBase = worktree.Base
		request.SourceRepo = worktree.SourceRepo
		preparedWorktree = &worktree
	} else if strings.TrimSpace(request.Base) != "" {
		return state.SessionInfo{}, errors.New("--base requires --worktree")
	}

	creationRecorded := false
	beforeLaunch := func(ctx context.Context, prepared state.PreparedSession) error {
		if err := m.recordCreated(ctx, prepared, creatorKind, creatorID); err != nil {
			return err
		}
		creationRecorded = true
		if takeover == nil {
			return nil
		}
		if m.boundaries == nil {
			return errors.New("record forced conversation takeover before launch: ledger writer is unavailable")
		}
		if err := m.boundaries.RecordProviderRebound(ctx, ledger.ProviderRebound{
			Meta: ledger.Meta{LaneID: takeover.SessionID}, ProviderUUID: providerUUID, NewLaneID: prepared.Info.ID,
		}); err != nil {
			return fmt.Errorf("record forced conversation takeover before launch: %w", err)
		}
		return nil
	}
	info, err := m.registry.CreateWithLifecycle(ctx, request, state.CreateLifecycle{
		BeforeLaunch:  beforeLaunch,
		LaunchStarted: m.recordLaunchStarted,
		RunnerReady:   m.recordRunnerReady,
	})
	if err != nil {
		if preparedWorktree != nil && !creationRecorded {
			if rollbackErr := rollbackCreatedGitWorktree(ctx, *preparedWorktree); rollbackErr != nil {
				return state.SessionInfo{}, fmt.Errorf("%w; new worktree was preserved at %s because safe rollback was refused: %v",
					err, preparedWorktree.Path, rollbackErr)
			}
		}
		return state.SessionInfo{}, err
	}
	session, ok := m.registry.Get(info.ID)
	if !ok {
		return state.SessionInfo{}, fmt.Errorf("created session %s was not registered", info.ID)
	}
	runtime := m.manage(session)
	if request.WaitReady {
		m.waitReady(ctx, runtime)
	}
	return m.withProvenance(ctx, []state.SessionInfo{session.Info()})[0], nil
}

func (m *Manager) Kill(ctx context.Context, id string, force bool) bool {
	return m.RequestKill(ctx, id, force) == nil
}

func (m *Manager) RequestKill(ctx context.Context, id string, force bool) error {
	return m.RequestKillAttributed(ctx, id, force, state.EndSessionRequest{})
}

// RequestKillAttributed records the authenticated initiator and optional
// operator-provided reason before the irreversible runner kill. Legacy callers
// may continue to use RequestKill; their records remain explicitly unattributed.
func (m *Manager) RequestKillAttributed(ctx context.Context, id string, force bool, end state.EndSessionRequest) error {
	var err error
	end, err = m.resolveEndInitiator(ctx, end)
	if err != nil {
		return err
	}
	if err := m.guard.Check(1, force); err != nil {
		return err
	}
	if _, ok := m.registry.Get(id); !ok {
		return fmt.Errorf("session %s not found", id)
	}
	return m.killOne(ctx, id, end)
}

func (m *Manager) KillMany(ctx context.Context, ids []string, force bool) error {
	return m.KillManyAttributed(ctx, ids, force, state.EndSessionRequest{})
}

func (m *Manager) KillManyAttributed(ctx context.Context, ids []string, force bool, end state.EndSessionRequest) error {
	var err error
	end, err = m.resolveEndInitiator(ctx, end)
	if err != nil {
		return err
	}
	unique := make(map[string]struct{})
	for _, id := range ids {
		if _, ok := m.registry.Get(id); ok {
			unique[id] = struct{}{}
		}
	}
	if err := m.guard.Check(len(unique), force); err != nil {
		return err
	}
	var failures []error
	for _, id := range sortedKeys(unique) {
		if err := m.killOne(ctx, id, end); err != nil {
			failures = append(failures, fmt.Errorf("kill session %s: %w", id, err))
		}
	}
	return errors.Join(failures...)
}

func (m *Manager) resolveEndInitiator(ctx context.Context, end state.EndSessionRequest) (state.EndSessionRequest, error) {
	if end.InitiatorKind == "" && end.InitiatorID == "" {
		return end, nil
	}
	kind := ledger.CreatorKind(end.InitiatorKind)
	if err := ledger.ValidateCreator(kind, end.InitiatorID); err != nil {
		return state.EndSessionRequest{}, fmt.Errorf("validate end initiator: %w", err)
	}
	if kind != ledger.CreatorSession {
		return end, nil
	}
	if m.ledgerReader == nil {
		return state.EndSessionRequest{}, errors.New("validate end initiator session: ledger reader is unavailable")
	}
	events, err := m.ledgerReader.Events(ctx, end.InitiatorID)
	if err != nil {
		return state.EndSessionRequest{}, fmt.Errorf("validate end initiator session: %w", err)
	}
	for _, candidate := range ledger.Fold(events) {
		if candidate.LaneID == end.InitiatorID && candidate.Created {
			if candidate.Archived {
				return state.EndSessionRequest{}, fmt.Errorf("end initiator session %s has been archived", end.InitiatorID)
			}
			end.InitiatorName = strings.TrimSpace(candidate.Name)
			if end.InitiatorName == "" {
				end.InitiatorName = strings.TrimSpace(candidate.Description)
			}
			return end, nil
		}
	}
	return state.EndSessionRequest{}, fmt.Errorf("end initiator session %s has no created event", end.InitiatorID)
}

func (m *Manager) killOne(ctx context.Context, id string, end state.EndSessionRequest) error {
	if m.boundaries != nil {
		if err := m.boundaries.RecordUserKill(ctx, ledger.UserKill{
			Meta:          ledger.Meta{LaneID: id},
			InitiatorKind: ledger.CreatorKind(end.InitiatorKind),
			InitiatorID:   end.InitiatorID,
			InitiatorName: end.InitiatorName,
			Client:        end.Client,
			Reason:        end.Reason,
			OperationID:   end.OperationID,
		}); err != nil {
			return fmt.Errorf("record user kill before runner kill: %w", err)
		}
	}
	return m.registry.RequestKill(ctx, id, true)
}

func (m *Manager) Input(ctx context.Context, id, data string) bool {
	if !m.registry.Input(ctx, id, data) {
		return false
	}
	// This is the unattributed door, and everything a person sends comes
	// through it: the HTTP input and submit routes, the WebSocket mux, an
	// attached terminal, `sessions send` typed by hand. Stamping here rather
	// than in each of those surfaces is what makes the answer complete by
	// construction, and it is deliberately not the transcript, because a
	// provider's own scheduled injections are written straight into the
	// transcript and never arrive here at all.
	m.registry.RecordInputPrincipal(id, state.PrincipalHuman, data)
	m.afterInput(ctx, id, data, ledger.ActivityHumanInput)
	return true
}

// InvalidMessageSourceError means the caller supplied lane provenance which
// cannot identify a durable, retained source lane.
type InvalidMessageSourceError struct{ Reason string }

func (e *InvalidMessageSourceError) Error() string { return e.Reason }

type InvalidAttributedInputError struct{ Reason string }

func (e *InvalidAttributedInputError) Error() string { return e.Reason }

// MessageInputUnavailableError means the target was not able to accept bytes.
type MessageInputUnavailableError struct{ SessionID string }

func (e *MessageInputUnavailableError) Error() string {
	return fmt.Sprintf("session %s is not available for input", e.SessionID)
}

// MessageAttributionCommitError is deliberately explicit: the provider
// accepted the bytes, so an automated caller must not retry and duplicate the
// turn, but the durable authorship fact could not be committed.
type MessageAttributionCommitError struct{ Err error }

func (e *MessageAttributionCommitError) Error() string {
	return fmt.Sprintf("message was delivered but its Sessions authorship record failed: %v; do not retry", e.Err)
}

func (e *MessageAttributionCommitError) Unwrap() error { return e.Err }

func (m *Manager) resolveMessageAuthor(ctx context.Context, sourceID, client string) (ledger.MessageAuthor, error) {
	if err := ledger.ValidateCreator(ledger.CreatorSession, sourceID); err != nil {
		return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: err.Error()}
	}
	if m.ledgerReader == nil {
		return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: "cannot validate source session: ledger reader is unavailable"}
	}
	events, err := m.ledgerReader.Events(ctx, sourceID)
	if err != nil {
		return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: fmt.Sprintf("validate source session: %v", err)}
	}
	for _, candidate := range ledger.Fold(events) {
		if candidate.LaneID != sourceID || !candidate.Created {
			continue
		}
		if candidate.Archived {
			return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: fmt.Sprintf("source session %s has been archived", sourceID)}
		}
		name := strings.TrimSpace(candidate.Name)
		if name == "" {
			name = "Unnamed session"
		}
		return ledger.MessageAuthor{
			Kind: ledger.CreatorSession, ID: sourceID, Name: name, Client: client,
		}, nil
	}
	return ledger.MessageAuthor{}, &InvalidMessageSourceError{Reason: fmt.Sprintf("source session %s has no created event", sourceID)}
}

// InputAttributed immediately delivers one text payload and records
// content-free lane authorship. The caller sends Enter separately without
// attribution so one provider turn produces exactly one relay fact.
func (m *Manager) InputAttributed(ctx context.Context, id, data string, attribution state.InputAttribution) error {
	if m.attributions == nil {
		return errors.New("message attribution is unavailable")
	}
	if strings.TrimSpace(data) == "" {
		return &InvalidAttributedInputError{Reason: "attributed input requires non-whitespace text"}
	}
	if id == attribution.SourceSessionID {
		return &InvalidMessageSourceError{Reason: "a session cannot relay a message to itself"}
	}
	author, err := m.resolveMessageAuthor(ctx, attribution.SourceSessionID, attribution.Client)
	if err != nil {
		return err
	}
	if !m.registry.Input(ctx, id, data) {
		return &MessageInputUnavailableError{SessionID: id}
	}
	// Attribution is what makes this an agent's message rather than a person's,
	// and the stamp is taken here rather than after the ledger commit below: the
	// provider already has the bytes, so the message happened whether or not the
	// authorship record does.
	m.registry.RecordInputPrincipal(id, state.PrincipalAgent, data)
	m.clearIdleAfterInput(id)
	exact := sha256.Sum256([]byte(data))
	normalizedText := strings.TrimSpace(data)
	normalized := sha256.Sum256([]byte(normalizedText))
	if err := m.attributions.RecordMessageRelayed(ctx, ledger.MessageRelayed{
		Meta: ledger.Meta{LaneID: id}, Author: author,
		ContentSHA256: fmt.Sprintf("%x", exact[:]), ContentBytes: len([]byte(data)),
		NormalizedSHA256: fmt.Sprintf("%x", normalized[:]), NormalizedBytes: len([]byte(normalizedText)),
	}); err != nil {
		return &MessageAttributionCommitError{Err: err}
	}
	m.afterInput(ctx, id, data, ledger.ActivitySessionInput)
	return nil
}

func (m *Manager) afterInput(ctx context.Context, id, data string, source ledger.ActivitySource) {
	m.clearIdleAfterInput(id)
	m.mu.Lock()
	runtime := m.runtimes[id]
	m.mu.Unlock()
	if runtime != nil {
		runtime.observeProviderInput(data)
	}
	if current, ok := m.registry.Get(id); ok && current.Info().SetAsideAt != nil {
		if _, err := m.registry.UpdateSetAside(id, false); err != nil {
			log.Printf("[working-set] bring back %s after input: %v", id, err)
		}
	}
	m.captureFirstMessageDescription(id, data)
	m.observe(ctx, "input activity", func(writer ledger.ObservationWriter) error {
		return writer.RecordActivity(ctx, ledger.Activity{
			Meta: ledger.Meta{LaneID: id}, Source: source,
		})
	})
}

const providerInputLimit = 1024 * 1024

func (r *runtimeSession) observeProviderInput(data string) {
	if r.session.Info().Tool != state.ToolCodex || data == "" {
		return
	}
	r.mu.Lock()
	for _, value := range []byte(data) {
		switch value {
		case '\r':
			prompt := normalizedTerminalPrompt(string(r.providerInput))
			r.providerInput = r.providerInput[:0]
			watcher := r.watcher
			r.mu.Unlock()
			if watcher != nil && prompt != "" {
				watcher.ExpectInput(prompt)
			}
			return
		case 0x7f:
			if len(r.providerInput) > 0 {
				r.providerInput = r.providerInput[:len(r.providerInput)-1]
			}
		default:
			if len(r.providerInput) < providerInputLimit {
				r.providerInput = append(r.providerInput, value)
			}
		}
	}
	r.mu.Unlock()
}

func normalizedTerminalPrompt(value string) string {
	value = strings.ReplaceAll(value, "\x1b[200~", "")
	value = strings.ReplaceAll(value, "\x1b[201~", "")
	return strings.TrimSpace(value)
}

func (m *Manager) clearIdleAfterInput(id string) {
	if current, ok := m.registry.Get(id); ok {
		current.ClearIdleResult()
	}
}

// MessageRelays returns the content-free attribution facts for one target
// lane. Correlation with provider transcript text happens only in response
// memory and never writes provider history.
func (m *Manager) MessageRelays(ctx context.Context, id string) ([]ledger.MessageRelayed, error) {
	if m.ledgerReader == nil {
		return nil, errors.New("message attribution is unavailable")
	}
	events, err := m.ledgerReader.Events(ctx, id)
	if err != nil {
		return nil, err
	}
	relays := make([]ledger.MessageRelayed, 0)
	for _, event := range events {
		if event.Type != ledger.EventMessageRelayed {
			continue
		}
		relay, err := ledger.DecodeMessageRelayed(event)
		if err != nil {
			return nil, err
		}
		relays = append(relays, relay)
	}
	return relays, nil
}

func (m *Manager) ConfigureModel(ctx context.Context, id, model, effort string) (state.SessionInfo, error) {
	current, ok := m.registry.Get(id)
	if !ok {
		return state.SessionInfo{}, fmt.Errorf("%w: session %s", state.ErrSessionNotFound, id)
	}
	info := current.Info()
	if info.Kind != state.KindCodexAppServer && info.Kind != state.KindClaudeStructured {
		return state.SessionInfo{}, errors.New("model changes are available only for Rich Claude and Rich Codex sessions; Terminal sessions keep their provider's own controls")
	}
	if info.Working {
		return state.SessionInfo{}, fmt.Errorf("%w; wait for the turn to finish with `sessions wait %s`", state.ErrSessionWorking, id)
	}
	model = strings.TrimSpace(model)
	effort = strings.ToLower(strings.TrimSpace(effort))
	if model == "" {
		return state.SessionInfo{}, errors.New("model is required")
	}

	if info.Kind == state.KindCodexAppServer {
		choice := codexModelChoice(info.Args)
		choice.Model = model
		choice.Effort = effort
		catalog, err := m.listModels(ctx, info.Cmd)
		if err != nil {
			return state.SessionInfo{}, fmt.Errorf("load live Codex model catalog: %w", err)
		}
		resolved, err := codexapp.ResolveModelChoice(catalog, choice)
		if err != nil {
			return state.SessionInfo{}, err
		}
		model = resolved.Model
		effort = resolved.Effort
	} else {
		if len(model) > 128 || strings.ContainsAny(model, "\r\n\x00") {
			return state.SessionInfo{}, errors.New("invalid Claude model")
		}
		switch effort {
		case "", "low", "medium", "high", "xhigh", "max":
		default:
			return state.SessionInfo{}, errors.New("Claude effort must be low, medium, high, xhigh, max, or empty")
		}
	}

	return m.registry.ConfigureModel(ctx, id, model, effort)
}

func (m *Manager) ModelOptions(ctx context.Context, id string) ([]codexapp.Model, error) {
	current, ok := m.registry.Get(id)
	if !ok {
		return nil, fmt.Errorf("%w: session %s", state.ErrSessionNotFound, id)
	}
	info := current.Info()
	if info.Kind != state.KindCodexAppServer {
		return nil, errors.New("the live model catalog is available for Rich Codex sessions")
	}
	return m.listModels(ctx, info.Cmd)
}

// CodexModelOptions returns the same live provider catalog used to validate
// Rich Codex sessions, but does not require a session to exist yet. The native
// launcher uses it to present real choices before creating a runtime.
func (m *Manager) CodexModelOptions(ctx context.Context) ([]codexapp.Model, error) {
	return m.listModels(ctx, "codex")
}

func (m *Manager) captureFirstMessageDescription(id, data string) {
	m.mu.Lock()
	runtime := m.runtimes[id]
	m.mu.Unlock()
	if runtime == nil {
		return
	}

	runtime.mu.Lock()
	if runtime.firstMessageDone {
		runtime.mu.Unlock()
		return
	}
	info := runtime.session.Info()
	if info.DescriptionSource == state.DescriptionExplicit || info.Description != "" {
		runtime.firstMessageDone = true
		runtime.mu.Unlock()
		return
	}

	complete := false
	for _, value := range []byte(data) {
		if value == '\r' || (value == '\n' && len(data) == 1) {
			complete = len(runtime.firstMessageInput) > 0
			if complete {
				break
			}
			continue
		}
		if len(runtime.firstMessageInput) < 4096 {
			runtime.firstMessageInput = append(runtime.firstMessageInput, value)
		}
	}
	if !complete {
		runtime.mu.Unlock()
		return
	}
	description := firstMessageDescription(string(runtime.firstMessageInput))
	if description == "" {
		runtime.firstMessageInput = nil
		runtime.mu.Unlock()
		return
	}
	runtime.firstMessageDone = true
	runtime.firstMessageInput = nil
	runtime.mu.Unlock()

	changed, err := m.registry.SetFirstMessageDescription(id, description)
	if err != nil {
		log.Printf("[description] persist first-message description for %s: %v", id, err)
	}
	if !changed {
		return
	}
	m.observe(context.Background(), "derived description", func(writer ledger.ObservationWriter) error {
		return writer.RecordDescriptionDerived(context.Background(), ledger.DescriptionDerived{
			Meta: ledger.Meta{LaneID: id}, Description: description, Source: ledger.DescriptionFirstMessage,
		})
	})
}

func firstMessageDescription(value string) string {
	var cleaned strings.Builder
	escapeSequence := 0
	for _, character := range value {
		if escapeSequence != 0 {
			if escapeSequence == 1 {
				if character == '[' {
					escapeSequence = 2
				} else {
					escapeSequence = 0
				}
			} else if character >= '@' && character <= '~' {
				escapeSequence = 0
			}
			continue
		}
		if character == '\x1b' {
			escapeSequence = 1
			continue
		}
		if unicode.IsControl(character) {
			cleaned.WriteRune(' ')
			continue
		}
		cleaned.WriteRune(character)
	}
	description := strings.Join(strings.Fields(cleaned.String()), " ")
	runes := []rune(description)
	if len(runes) > 80 {
		description = string(runes[:79]) + "…"
	}
	return description
}

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
}

func (r *runtimeSession) setWorking(next bool) {
	previous, exited := r.session.SetWorking(next)
	now := time.Now()
	r.mu.Lock()
	if !previous && next {
		r.workingStartedAt = now
		r.structuredDone = false
		r.terminalTurnDone = false
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
		watcher = watch.WatchCodexRollout(watch.CodexWatcherOptions{
			CWD: info.Cwd, Args: info.Args, CreatedAt: time.UnixMilli(info.CreatedAt), SessionsDir: sessionsDir,
			RequireInputMatch: true,
		})
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
