package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/somewhere-tech/sessions/runtime/internal/claudep"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type recordingUsage struct {
	calls int
	info  state.SessionInfo
	raw   json.RawMessage
}

func (r *recordingUsage) RecordStructured(_ context.Context, info state.SessionInfo, raw json.RawMessage) error {
	r.calls++
	r.info = info
	r.raw = append(r.raw[:0], raw...)
	return nil
}

func TestManagerForwardsOnlyStructuredUsageCandidates(t *testing.T) {
	recorder := &recordingUsage{}
	manager := &Manager{usage: recorder}
	info := state.SessionInfo{ID: "session-live", Tool: state.ToolCodex}
	manager.recordStructuredUsage(info, json.RawMessage(`{"type":"assistant"}`))
	manager.recordStructuredUsage(info, json.RawMessage(`{"type":"codex","usage":{"last":{}}}`))
	if recorder.calls != 1 || recorder.info.ID != info.ID || !strings.Contains(string(recorder.raw), `"usage"`) {
		t.Fatalf("usage recorder = calls %d info %#v raw %s", recorder.calls, recorder.info, recorder.raw)
	}
}

func TestDurableExitReasonRequiresCleanExitEvidence(t *testing.T) {
	zero, failure := 0, 17
	if got := durableExitReason(ledger.LaneState{RunnerExited: true}); got != "ended" {
		t.Fatalf("runner exit without code = %q, want ended", got)
	}
	if got := durableExitReason(ledger.LaneState{RunnerExited: true, ExitCode: &zero}); got != "completed" {
		t.Fatalf("runner exit code zero = %q, want completed", got)
	}
	if got := durableExitReason(ledger.LaneState{RunnerExited: true, ExitCode: &failure}); got != "failed" {
		t.Fatalf("runner exit code nonzero = %q, want failed", got)
	}
}

func TestInteractiveShellIdleDoesNotClaimCompletion(t *testing.T) {
	root := t.TempDir()
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{DisableWatchers: true})
	t.Cleanup(manager.Close)
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/bash", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	runtime := manager.runtimes[created.ID]
	manager.mu.Unlock()
	runtime.setWorking(true)
	runtime.setWorking(false)
	info, _ := manager.Get(created.ID)
	if got := info.Info().IdleReason; got != "" {
		t.Fatalf("live shell idle reason = %q, want no completed/failed claim", got)
	}
}

func TestTerminalCodexTaskCompleteOverridesStaleScreenError(t *testing.T) {
	root := t.TempDir()
	launcher := prototest.NewLauncher()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, Notify: func(PushPayload) {},
	})
	t.Cleanup(manager.Close)
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "codex", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	runtime := manager.runtimes[created.ID]
	manager.mu.Unlock()
	runtime.setWorking(true)
	launcher.Runner(created.ID).AddOutput("old fatal error text still visible\n")
	runtime.markTerminalTurnDone()
	runtime.setWorking(false)
	info, _ := manager.Get(created.ID)
	if got := info.Info().IdleReason; got != state.IdleReasonCompleted {
		t.Fatalf("Codex task_complete idle reason = %q, want %q", got, state.IdleReasonCompleted)
	}
}

func TestCodexAppServerStructuredHistoryAndLifecycleAreAuthoritative(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	config.UserStateRoot = blockIdlePushState(t, root)
	launcher := prototest.NewLauncher()
	manager := NewManager(config, launcher, ManagerOptions{
		ActivityInterval: time.Hour,
		Notify:           func(PushPayload) {},
	})
	t.Cleanup(manager.Close)

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: root, Kind: state.KindCodexAppServer,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != state.KindCodexAppServer || created.Tool != state.ToolCodex {
		t.Fatalf("created structured session = %#v", created)
	}
	runner := launcher.Runner(created.ID)
	user, err := codexapp.UserHistoryEvent("conversation-1", "hello", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	runner.AddCodexEvent(json.RawMessage(user))
	started, _ := codexapp.HistoryEvent(codexapp.TurnStarted{ConversationID: "conversation-1", TurnID: "turn-1"}, time.Unix(2, 0))
	runner.AddCodexEvent(json.RawMessage(started))
	awaitCondition(t, func() bool {
		info, ok := manager.Get(created.ID)
		return ok && info.Info().Working
	})
	manager.mu.Lock()
	runtime := manager.runtimes[created.ID]
	manager.mu.Unlock()
	runtime.mu.Lock()
	runtime.pushWorkingObserved = false
	runtime.mu.Unlock()

	phase := "final_answer"
	assistant, _ := codexapp.HistoryEvent(codexapp.ItemCompleted{
		ConversationID: "conversation-1", TurnID: "turn-1", CompletedAtMS: 3000,
		Item: codexapp.ThreadItem{ID: "message-1", Type: "agentMessage", Text: "STRUCTURED_OK", Phase: &phase},
	}, time.Unix(3, 0))
	runner.AddCodexEvent(json.RawMessage(assistant))
	completed, _ := codexapp.HistoryEvent(codexapp.TurnComplete{
		ConversationID: "conversation-1", TurnID: "turn-1", Status: "completed",
	}, time.Unix(4, 0))
	runner.AddCodexEvent(json.RawMessage(completed))
	awaitCondition(t, func() bool {
		info, ok := manager.Get(created.ID)
		return ok && !info.Info().Working && info.ClaudeEventCount() == 4
	})
	session, _ := manager.Get(created.ID)
	window := session.EventsWindow(nil, nil, nil)
	if len(window.Events) != 4 || !strings.Contains(string(window.Events[2]), `"source":"codex-app-server"`) {
		t.Fatalf("structured history = %s", window.Events)
	}
	snapshot, _, err := session.Snapshot(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot, "[user]\nhello") || !strings.Contains(snapshot, "[assistant]\nSTRUCTURED_OK") {
		t.Fatalf("structured snapshot = %q", snapshot)
	}
}

type codexBoundLauncher struct{ *prototest.Launcher }

func (l codexBoundLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	request.Info.ConversationID = "019f7181-cb32-76e0-952d-2f5f7862e668"
	request.Info.RemoteEndpoint = "unix:///tmp/codex-test.sock"
	return l.Launcher.Launch(ctx, request)
}

func TestCodexAppServerConversationPersistsInMetadataAndLedger(t *testing.T) {
	root := t.TempDir()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	launcher := codexBoundLauncher{Launcher: prototest.NewLauncher()}
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "codex", Cwd: root, Kind: state.KindCodexAppServer,
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := state.ReadRunnerMetadata(filepath.Join(manager.config.RunnerStateDir, created.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Info.ConversationID != "019f7181-cb32-76e0-952d-2f5f7862e668" || metadata.Info.RemoteEndpoint != "unix:///tmp/codex-test.sock" {
		t.Fatalf("persisted metadata = %#v", metadata.Info)
	}
	events, err := store.Events(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	lanes := ledger.Fold(events)
	if len(lanes) != 1 || lanes[0].ProviderUUID != "019f7181-cb32-76e0-952d-2f5f7862e668" {
		t.Fatalf("ledger state = %#v", lanes)
	}
}

func TestNormalizedTerminalPromptRemovesBracketedPaste(t *testing.T) {
	got := normalizedTerminalPrompt("\x1b[200~line one\nline two\x1b[201~")
	if got != "line one\nline two" {
		t.Fatalf("normalized prompt = %q", got)
	}
}

type claudeStructuredBoundLauncher struct{ *prototest.Launcher }

func (l claudeStructuredBoundLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	request.Info.ClaudeSessionID = "019f7181-cb32-46e0-952d-2f5f7862e668"
	return l.Launcher.Launch(ctx, request)
}

type reapRecordingLauncher struct {
	*prototest.Launcher
	reaped []string
}

func (l *reapRecordingLauncher) Reap(id string) error {
	l.reaped = append(l.reaped, id)
	return nil
}

func TestClaudeStructuredHistoryLifecycleMetadataLedgerAndAuth(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	config.UserStateRoot = blockIdlePushState(t, root)
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	t.Setenv("ANTHROPIC_API_KEY", "must-not-reach-structured-runner")
	launcher := claudeStructuredBoundLauncher{Launcher: prototest.NewLauncher()}
	manager := NewManager(config, launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		Notify: func(PushPayload) {},
	})
	t.Cleanup(manager.Close)
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: root, Kind: state.KindClaudeStructured,
		Args: []string{"--dangerously-skip-permissions", "--session-id", "019f7181-cb32-46e0-952d-2f5f7862e668"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Kind != state.KindClaudeStructured || created.Tool != state.ToolClaude || created.ClaudeSessionID == "" {
		t.Fatalf("created structured Claude session = %#v", created)
	}
	metadata, err := state.ReadRunnerMetadata(filepath.Join(manager.config.RunnerStateDir, created.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Info.ClaudeSessionID != created.ClaudeSessionID {
		t.Fatalf("metadata Claude session = %q, want %q", metadata.Info.ClaudeSessionID, created.ClaudeSessionID)
	}
	launch := launcher.Launches[len(launcher.Launches)-1]
	if _, present := launch.Env["ANTHROPIC_API_KEY"]; present {
		t.Fatalf("structured runner launch environment contains ANTHROPIC_API_KEY")
	}

	runner := launcher.Runner(created.ID)
	user, _ := claudep.UserHistoryEvent(created.ClaudeSessionID, "hello", time.Unix(1, 0))
	started, _ := claudep.TurnStartedEvent(created.ClaudeSessionID, time.Unix(2, 0))
	runner.AddCodexEvent(json.RawMessage(user))
	runner.AddCodexEvent(json.RawMessage(started))
	awaitCondition(t, func() bool {
		session, ok := manager.Get(created.ID)
		return ok && session.Info().Working
	})
	manager.mu.Lock()
	managed := manager.runtimes[created.ID]
	manager.mu.Unlock()
	managed.mu.Lock()
	managed.pushWorkingObserved = false
	managed.mu.Unlock()
	assistant, err := claudep.NormalizeEvent(json.RawMessage(`{"type":"assistant","session_id":"019f7181-cb32-46e0-952d-2f5f7862e668","message":{"role":"assistant","content":[{"type":"text","text":"CLAUDEP_UNIT_OK"}]}}`), time.Unix(3, 0))
	if err != nil {
		t.Fatal(err)
	}
	result, err := claudep.NormalizeEvent(json.RawMessage(`{"type":"result","subtype":"success","session_id":"019f7181-cb32-46e0-952d-2f5f7862e668","result":"CLAUDEP_UNIT_OK","usage":{"input_tokens":1,"output_tokens":1}}`), time.Unix(4, 0))
	if err != nil {
		t.Fatal(err)
	}
	runner.AddCodexEvent(assistant.Raw)
	runner.AddCodexEvent(result.Raw)
	awaitCondition(t, func() bool {
		session, ok := manager.Get(created.ID)
		return ok && !session.Info().Working && session.ClaudeEventCount() == 4
	})
	session, _ := manager.Get(created.ID)
	snapshot, _, err := session.Snapshot(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot, "[user]\nhello") || !strings.Contains(snapshot, "[assistant]\nCLAUDEP_UNIT_OK") {
		t.Fatalf("structured Claude snapshot = %q", snapshot)
	}
	events, err := store.Events(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	lanes := ledger.Fold(events)
	if len(lanes) != 1 || lanes[0].ProviderUUID != created.ClaudeSessionID {
		t.Fatalf("Claude provider ledger state = %#v", lanes)
	}
}

// Lifecycle tests do not exercise web push. A regular file makes both the
// idle-sentinel and VAPID directories unavailable, preventing their detached
// goroutines from racing TempDir cleanup after the state assertions finish.
func blockIdlePushState(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "push-disabled")
	if err := os.WriteFile(path, []byte("disabled"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

type compositionLauncher struct {
	base  *prototest.Launcher
	store *ledger.Store

	mu          sync.Mutex
	killOrdered map[string]bool
}

func (l *compositionLauncher) ProgramArguments(request proto.LaunchRequest) []string {
	return l.base.ProgramArguments(request)
}

func (l *compositionLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	runner, err := l.base.Launch(ctx, request)
	if err != nil {
		return nil, err
	}
	return &compositionRunner{Runner: runner, launcher: l, laneID: request.Info.ID}, nil
}

func (l *compositionLauncher) Attach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	return l.base.Attach(ctx, info)
}

type compositionRunner struct {
	proto.Runner
	launcher *compositionLauncher
	laneID   string
}

// rediscoveryLauncher models one live runner process with replaceable
// daemon-side connections. Closing a connection emits runner-lost only to the
// daemon; the process remains attachable and records any accidental reap.
type rediscoveryLauncher struct {
	mu          sync.Mutex
	process     *rediscoveryProcess
	connections []*rediscoveryConnection
	attachCount int
	attachFails int
	reaped      []string
}

type rediscoveryProcess struct {
	mu      sync.Mutex
	info    proto.RunnerInfo
	outputs []proto.OutputEvent
	alive   bool
}

type rediscoveryConnection struct {
	process *rediscoveryProcess
	mu      sync.Mutex
	stream  chan proto.Event
	closed  bool
}

func (l *rediscoveryLauncher) ProgramArguments(proto.LaunchRequest) []string {
	return []string{"/usr/bin/true"}
}

func (l *rediscoveryLauncher) Launch(_ context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	request.Info.PID = os.Getpid()
	request.Info.ProtocolVersion = proto.ProtocolVersion
	process := &rediscoveryProcess{info: request.Info, alive: true}
	connection := &rediscoveryConnection{process: process}
	l.mu.Lock()
	l.process = process
	l.connections = append(l.connections, connection)
	l.mu.Unlock()
	return connection, nil
}

func (l *rediscoveryLauncher) Attach(_ context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.process == nil || !l.process.isAlive() || l.process.info.ID != info.ID {
		return nil, errors.New("scratch runner is unavailable")
	}
	l.attachCount++
	if l.attachFails > 0 {
		l.attachFails--
		return nil, errors.New("scratch runner socket is temporarily unavailable")
	}
	connection := &rediscoveryConnection{process: l.process}
	l.connections = append(l.connections, connection)
	return connection, nil
}

func (l *rediscoveryLauncher) Reap(id string) error {
	l.mu.Lock()
	l.reaped = append(l.reaped, id)
	l.mu.Unlock()
	return nil
}

func (l *rediscoveryLauncher) firstConnection() *rediscoveryConnection {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.connections[0]
}

func (l *rediscoveryLauncher) counts() (int, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.attachCount, len(l.reaped)
}

func (l *rediscoveryLauncher) failNextAttaches(count int) {
	l.mu.Lock()
	l.attachFails = count
	l.mu.Unlock()
}

func (p *rediscoveryProcess) isAlive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.alive
}

func (p *rediscoveryProcess) runnerInfo() proto.RunnerInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	info := p.info
	info.Args = append([]string(nil), p.info.Args...)
	if len(p.outputs) > 0 {
		info.CurrentSeq = p.outputs[len(p.outputs)-1].Seq
	}
	return info
}

func (c *rediscoveryConnection) Info() proto.RunnerInfo { return c.process.runnerInfo() }

func (c *rediscoveryConnection) Replay(_ context.Context, after uint32) proto.ReplayWindow {
	c.process.mu.Lock()
	defer c.process.mu.Unlock()
	events := make([]proto.OutputEvent, 0, len(c.process.outputs))
	for _, event := range c.process.outputs {
		if event.Seq > after {
			events = append(events, event)
		}
	}
	current := uint32(0)
	if len(c.process.outputs) > 0 {
		current = c.process.outputs[len(c.process.outputs)-1].Seq
	}
	return proto.ReplayWindow{Events: events, Current: current}
}

func (c *rediscoveryConnection) Input(context.Context, string) error {
	if !c.process.isAlive() {
		return errors.New("scratch runner exited")
	}
	return nil
}

func (c *rediscoveryConnection) ConfigureModel(context.Context, proto.ModelControl) error {
	if !c.process.isAlive() {
		return errors.New("scratch runner exited")
	}
	return nil
}

func (c *rediscoveryConnection) Resize(context.Context, int, int) error { return nil }

func (c *rediscoveryConnection) Kill(context.Context) error {
	c.process.mu.Lock()
	c.process.alive = false
	c.process.mu.Unlock()
	zero := 0
	c.finish(proto.Event{Kind: proto.EventExit, Exit: proto.ExitEvent{Code: &zero}})
	return nil
}

func (c *rediscoveryConnection) Subscribe() (<-chan proto.Event, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream == nil {
		c.stream = make(chan proto.Event, 8)
	}
	stream := c.stream
	return stream, func() {
		c.mu.Lock()
		if !c.closed {
			c.closed = true
			close(stream)
		}
		c.mu.Unlock()
	}
}

func (c *rediscoveryConnection) blip() {
	c.finish(proto.Event{Kind: proto.EventRunnerLost, Exit: proto.ExitEvent{Reason: "scratch-socket-blip"}})
}

func (c *rediscoveryConnection) finish(event proto.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	if c.stream == nil {
		c.stream = make(chan proto.Event, 8)
	}
	c.stream <- event
	close(c.stream)
}

func (r *compositionRunner) Kill(ctx context.Context) error {
	events, err := r.launcher.store.Events(ctx, r.laneID)
	if err != nil {
		return err
	}
	ordered := len(events) > 0 && events[len(events)-1].Type == ledger.EventUserKillRequested
	r.launcher.mu.Lock()
	r.launcher.killOrdered[r.laneID] = ordered
	r.launcher.mu.Unlock()
	if !ordered {
		return errors.New("runner kill observed no committed tombstone")
	}
	return r.Runner.Kill(ctx)
}

func TestMassKillGuardThenTombstoneThenRunnerKillComposition(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	store, err := ledger.Open(context.Background(), ledger.Options{
		Path: filepath.Join(root, "ledger", "lanes.sqlite3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	launcher := &compositionLauncher{
		base: prototest.NewLauncher(), store: store, killOrdered: make(map[string]bool),
	}
	manager := NewManager(config, launcher, ManagerOptions{
		MassKillLimit: 3, DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	defer manager.Close()

	ids := make([]string, 0, 4)
	for range 4 {
		created, createErr := manager.Create(context.Background(), state.CreateSessionRequest{
			Cmd: "/bin/sh", Cwd: root,
		})
		if createErr != nil {
			t.Fatal(createErr)
		}
		ids = append(ids, created.ID)
	}

	err = manager.KillMany(context.Background(), ids, false)
	var guardErr *MassKillError
	if !errors.As(err, &guardErr) || guardErr.Count != len(ids) {
		t.Fatalf("KillMany() error = %v, want guard refusal for %d", err, len(ids))
	}
	for _, id := range ids {
		events, readErr := store.Events(context.Background(), id)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, event := range events {
			if event.Type == ledger.EventUserKillRequested {
				t.Fatalf("guard-refused lane %s received tombstone", id)
			}
		}
		if !manager.Input(context.Background(), id, "still alive") {
			t.Fatalf("guard-refused lane %s was killed", id)
		}
	}

	if err := manager.KillMany(context.Background(), ids, true); err != nil {
		t.Fatal(err)
	}
	launcher.mu.Lock()
	for _, id := range ids {
		if !launcher.killOrdered[id] {
			t.Errorf("lane %s runner kill did not observe committed tombstone", id)
		}
	}
	launcher.mu.Unlock()
	for _, id := range ids {
		awaitCondition(t, func() bool {
			events, readErr := store.Events(context.Background(), id)
			if readErr != nil {
				return false
			}
			for _, event := range events {
				if event.Type == ledger.EventReaped {
					return true
				}
			}
			return false
		})
	}
}

func TestPeriodicDiscoveryReattachesAfterDaemonSideSocketBlipWithoutReapingLiveRunner(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	launcher := &rediscoveryLauncher{}
	manager := NewManager(config, launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		DiscoveryRetries: 1, DiscoveryDelay: time.Millisecond,
	})
	t.Cleanup(manager.Close)
	t.Setenv(discoveryIntervalEnv, "20ms")
	go manager.RunDiscoveryLoop()

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(config.RunnerStateDir, created.ID+".sock")
	if err := os.WriteFile(socketPath, []byte("scratch daemon-side socket"), 0o600); err != nil {
		t.Fatal(err)
	}

	launcher.firstConnection().blip()
	deadline := time.Now().Add(750 * time.Millisecond) // well below the 1s reconnect timer
	for {
		attachCount, reapCount := launcher.counts()
		session, present := manager.Get(created.ID)
		if attachCount > 0 && present && !session.Info().Exited {
			if reapCount != 0 {
				t.Fatalf("periodic discovery reaped a live runner %d times", reapCount)
			}
			if !launcher.process.isAlive() {
				t.Fatal("daemon-side socket blip killed the scratch runner process")
			}
			t.Logf("periodic discovery reattached %s after %d attach attempt(s); reaps=%d", created.ID, attachCount, reapCount)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("periodic discovery did not reattach before reconnect timer: attaches=%d present=%v reaps=%d", attachCount, present, reapCount)
		}
		runtime.Gosched()
	}
}

func TestReconnectRepeatsFinalBackoffUntilLiveRunnerReappears(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	launcher := &rediscoveryLauncher{}
	manager := NewManager(config, launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
	})
	t.Cleanup(manager.Close)

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config.RunnerStateDir, created.ID+".sock"), []byte("scratch socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher.firstConnection().blip()
	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		if _, present := manager.Get(created.ID); !present {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lost scratch connection remained registered")
		}
		runtime.Gosched()
	}

	launcher.failNextAttaches(2)
	manager.scheduleReconnect(created.ID, []time.Duration{10 * time.Millisecond})
	for {
		attempts, reaps := launcher.counts()
		reattached, present := manager.Get(created.ID)
		if attempts == 3 && present && !reattached.Info().Exited {
			if reaps != 0 || !launcher.process.isAlive() {
				t.Fatalf("reconnect harmed live runner: reaps=%d alive=%v", reaps, launcher.process.isAlive())
			}
			t.Logf("terminal reconnect backoff repeated through %d attempts", attempts)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal reconnect delay exhausted: attempts=%d present=%v", attempts, present)
		}
		runtime.Gosched()
	}
}

func TestStartupRestartThenDiscoveryReconcilesAbsentLedgerLane(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	store, err := ledger.Open(context.Background(), ledger.Options{
		Path: filepath.Join(root, "ledger", "lanes.sqlite3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const laneID = "00000000-0000-4000-8000-000000000123"
	if err := store.Boundaries().RecordCreated(context.Background(), ledger.Created{
		Meta: ledger.Meta{LaneID: laneID}, LaneUUID: laneID, Tool: string(state.ToolTerminal), Cwd: root,
		CreatorKind: ledger.CreatorExternal, CreatorID: "manager-test",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Observations().RecordRunnerReady(context.Background(), ledger.Observation{
		Meta: ledger.Meta{LaneID: laneID},
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	defer manager.Close()
	if err := manager.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}

	events, err := store.Events(context.Background(), laneID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[2].Type != ledger.EventDaemonRestart || events[3].Type != ledger.EventRunnerLost {
		t.Fatalf("startup reconciliation events = %#v, want created, ready, daemon_restart, runner_lost", events)
	}
}

// Reconciliation used to read "not in the in-memory map" as "the runner is
// gone" and write a permanent runner_lost fact. The map holds the runners this
// daemon process currently has sockets to; a runner that outlived a daemon
// restart is missing from it while working perfectly well. On the owner's
// machine two runners probed alive at pids 43014 and 22440 carried runner_lost
// facts and were listed as ended. Absence from a map is not death: probe.
func TestReconcileLedgerRecordsNoLossWhileTheRunnerPIDIsAlive(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const alive = "00000000-0000-4000-8000-000000000201"
	const dead = "00000000-0000-4000-8000-000000000202"
	const recycled = "00000000-0000-4000-8000-000000000203"
	for _, id := range []string{alive, dead, recycled} {
		if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
			Meta: ledger.Meta{LaneID: id}, LaneUUID: id,
			Tool: string(state.ToolTerminal), Cwd: root,
			CreatorKind: ledger.CreatorExternal, CreatorID: "manager-test",
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Observations().RecordRunnerReady(ctx, ledger.Observation{
			Meta: ledger.Meta{LaneID: id},
		}); err != nil {
			t.Fatal(err)
		}
	}
	pids := map[string]int{alive: 4242, dead: 4243, recycled: 4244}
	for id, pid := range pids {
		paths := state.For(config.RunnerStateDir, id)
		if err := state.WriteMetadata(paths.Meta, state.Metadata{
			ID: id, Cmd: "/bin/sh", Cwd: root, Cols: 300, Rows: 50,
			CreatedAt: 1, PID: pid, SockPath: paths.Socket,
		}); err != nil {
			t.Fatal(err)
		}
	}

	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		Notify:       func(PushPayload) {},
		ProcessAlive: func(pid int) bool { return pid != pids[dead] },
		ProcessCommand: func(pid int) string {
			if pid == pids[recycled] {
				// A live pid running something that is not this session.
				return "/Applications/Xcode.app/Contents/MacOS/Xcode"
			}
			return "/opt/sessions/sessions-runner"
		},
	})
	defer manager.Close()

	manager.reconcileLedger(ctx)

	folded := make(map[string]ledger.LaneState)
	for _, lane := range ledger.Fold(mustLedgerEvents(t, store)) {
		folded[lane.LaneID] = lane
	}
	if folded[alive].RunnerLost {
		t.Fatal("reconciliation recorded a loss for a lane whose runner pid is alive")
	}
	if !folded[dead].RunnerLost {
		t.Fatal("reconciliation recorded no loss for a lane whose runner pid is dead")
	}
	if !folded[recycled].RunnerLost {
		t.Fatal("reconciliation treated a recycled pid running an unrelated program as a live runner")
	}
}

// A lane the daemon merely lost contact with stays in the listing, and stays
// out of the ended state. Synthesising Exited:true with a nil exit code for a
// lost socket is how live sessions appeared in the list as ended-and-failed.
func TestUnreachableLaneIsListedButNotEnded(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config := testConfig(root)
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const lost = "00000000-0000-4000-8000-000000000301"
	const ended = "00000000-0000-4000-8000-000000000302"
	for _, id := range []string{lost, ended} {
		if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
			Meta: ledger.Meta{LaneID: id, AtMS: 10}, LaneUUID: id,
			Tool: string(state.ToolTerminal), Cwd: root,
			CreatorKind: ledger.CreatorExternal, CreatorID: "manager-test",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Observations().RecordRunnerLost(ctx, ledger.Observation{
		Meta: ledger.Meta{LaneID: lost, AtMS: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{
		Meta: ledger.Meta{LaneID: ended, AtMS: 100},
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		Notify: func(PushPayload) {},
	})
	defer manager.Close()

	byID := make(map[string]state.SessionInfo)
	for _, info := range manager.withDurableClosed(ctx, nil, true) {
		byID[info.ID] = info
	}
	unreachable, listed := byID[lost]
	if !listed {
		t.Fatal("an unreachable session disappeared from the listing")
	}
	if unreachable.Exited {
		t.Fatalf("an unreachable session was presented as ended: %#v", unreachable)
	}
	if !unreachable.Unreachable || unreachable.UnreachableSince == nil {
		t.Fatalf("an unreachable session was not marked unreachable: %#v", unreachable)
	}
	if unreachable.ExitCode != nil || unreachable.ExitedAt != nil || unreachable.ExitReason != "" {
		t.Fatalf("an unreachable session carried invented exit details: %#v", unreachable)
	}
	if closed := byID[ended]; !closed.Exited || closed.Unreachable {
		t.Fatalf("a session with a reaped status = %#v, want exited and not unreachable", closed)
	}
}

func TestProviderActivityTimestampFlowsFromRecordClaudeLocked(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	store, err := ledger.Open(context.Background(), ledger.Options{
		Path: filepath.Join(root, "ledger", "lanes.sqlite3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	launcher := prototest.NewLauncher()
	manager := NewManager(config, launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	defer manager.Close()
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err != nil {
		t.Fatal(err)
	}
	const timestamp = "2026-07-16T12:34:56.789Z"
	wantAt, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	managed := manager.runtimes[created.ID]
	manager.mu.Unlock()
	if managed == nil {
		t.Fatal("created session has no managed runtime")
	}
	launcher.Runner(created.ID).AddClaudeEvent(map[string]any{
		"type": "assistant", "timestamp": timestamp,
	})
	<-managed.structuredEventArrived
	events, err := store.Events(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == ledger.EventActivity && event.AtMS == wantAt.UnixMilli() && strings.Contains(string(event.Payload), "provider_event") {
			return
		}
	}
	t.Fatalf("provider activity with timestamp %d was not recorded: %#v", wantAt.UnixMilli(), events)
}

func TestDescriptionPersistsAndFirstMessageFallbackNeverOverridesExplicit(t *testing.T) {
	tests := []struct {
		name            string
		explicit        string
		firstInput      string
		wantDescription string
		wantSource      string
	}{
		{
			name: "first message fallback", firstInput: "\x1b[200~Investigate flaky cleanup ownership\x1b[201~",
			wantDescription: "Investigate flaky cleanup ownership", wantSource: state.DescriptionFirstMessage,
		},
		{
			name: "explicit wins", explicit: "Keep the release lane healthy", firstInput: "Replace the explicit purpose",
			wantDescription: "Keep the release lane healthy", wantSource: state.DescriptionExplicit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			manager := NewManager(testConfig(root), prototest.NewLauncher(), ManagerOptions{
				DisableWatchers: true, ActivityInterval: time.Hour,
				Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
			})
			defer manager.Close()

			created, err := manager.Create(context.Background(), state.CreateSessionRequest{
				Cmd: "/bin/sh", Cwd: root, Description: test.explicit,
			})
			if err != nil {
				t.Fatal(err)
			}
			if !manager.Input(context.Background(), created.ID, test.firstInput) ||
				!manager.Input(context.Background(), created.ID, "\r") {
				t.Fatal("first user message was not accepted")
			}
			current, ok := manager.Get(created.ID)
			if !ok {
				t.Fatal("created session disappeared")
			}
			if got := current.Info(); got.Description != test.wantDescription || got.DescriptionSource != test.wantSource {
				t.Fatalf("session description = %q/%q, want %q/%q", got.Description, got.DescriptionSource, test.wantDescription, test.wantSource)
			}
			metadata, err := state.ReadRunnerMetadata(filepath.Join(manager.config.RunnerStateDir, created.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Description != test.wantDescription || metadata.DescriptionSource != test.wantSource {
				t.Fatalf("metadata description = %q/%q, want %q/%q", metadata.Description, metadata.DescriptionSource, test.wantDescription, test.wantSource)
			}
			events, err := store.Events(context.Background(), created.ID)
			if err != nil {
				t.Fatal(err)
			}
			folded := ledger.Fold(events)
			if len(folded) != 1 || folded[0].Description != test.wantDescription || string(folded[0].DescriptionSource) != test.wantSource {
				t.Fatalf("folded description = %#v, want %q/%q", folded, test.wantDescription, test.wantSource)
			}
			if test.explicit != "" {
				if !strings.Contains(string(events[0].Payload), `"description":"Keep the release lane healthy"`) ||
					!strings.Contains(string(events[0].Payload), `"description_source":"explicit"`) {
					t.Fatalf("created event did not persist explicit description: %s", events[0].Payload)
				}
			}
		})
	}
}

func TestMassKillGuardRefusesDiscoverySweepBeforeBootout(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.LaunchAgentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	paths := make([]string, 0, DefaultMassKillLimit+1)
	for i := 0; i < DefaultMassKillLimit+1; i++ {
		id := "00000000-0000-4000-8000-00000000000" + string(rune('0'+i))
		path := state.RunnerPlistPath(config.LaunchAgentsDir, id)
		if err := os.WriteFile(path, []byte("scratch"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{DisableWatchers: true})
	t.Cleanup(manager.Close)
	err := manager.Discover(context.Background())
	var guardErr *MassKillError
	if !errors.As(err, &guardErr) || guardErr.Count != DefaultMassKillLimit+1 {
		t.Fatalf("Discover() error = %v, want mass-kill guard for %d", err, DefaultMassKillLimit+1)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("guarded sweep mutated %s: %v", path, err)
		}
	}
	if err := manager.DiscoverWithOptions(context.Background(), DiscoverOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("forced sweep left %s: %v", path, err)
		}
	}
}

func TestDiscoveryPreservesRegisteredSessionWithStaleLaunchArtifact(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	config := testConfig(root)
	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true,
	})
	t.Cleanup(manager.Close)

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "registered session",
	})
	if err != nil {
		t.Fatal(err)
	}

	// A daemon restart or an older runner can leave launchd bookkeeping that
	// looks orphaned even though the current registry already owns the session.
	// Discovery must trust the live registry and never reap that session.
	for _, path := range []string{
		filepath.Join(config.RunnerStateDir, created.ID+".events"),
		filepath.Join(config.RunnerStateDir, created.ID+".sock"),
	} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	plistPath := state.RunnerPlistPath(config.LaunchAgentsDir, created.ID)
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(plistPath, old, old); err != nil {
		t.Fatal(err)
	}

	if err := manager.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Get(created.ID); !ok {
		t.Fatal("discovery reaped a session already owned by the live registry")
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("discovery removed the registered session plist: %v", err)
	}
}

func TestDiscoveryReapsExitedLegacyRunnerPlist(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.LaunchAgentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-000000000099"
	path := state.LegacyRunnerPlistPath(config.LaunchAgentsDir, id)
	if err := os.WriteFile(path, []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{DisableWatchers: true})
	t.Cleanup(manager.Close)
	if err := manager.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy exited-runner plist remained after discovery: %v", err)
	}
}

func TestLaunchdFreeCreateWritesMetadataAndPlist(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	config := testConfig(root)
	launcher := prototest.NewLauncher()
	manager := NewManager(config, launcher, ManagerOptions{DisableWatchers: true})
	t.Cleanup(manager.Close)

	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: root, Name: "  lane acceptance  ",
		Env: map[string]string{
			"SAFE": "a&b<c>", "RUNNER_ID": "caller-value",
			"SESSIONS_SESSION_ID": "caller-session", "NODE_OPTIONS": "bad",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Cols != 300 || created.Rows != 50 || created.Name != "lane acceptance" {
		t.Fatalf("unexpected defaults: %#v", created)
	}
	joinedArgs := strings.Join(created.Args, " ")
	for _, want := range []string{"--session-id " + created.ID} {
		if !strings.Contains(joinedArgs, want) {
			t.Fatalf("args %q missing %q", joinedArgs, want)
		}
	}
	if strings.Contains(joinedArgs, "--dangerously-skip-permissions") {
		t.Fatalf("default Claude args unexpectedly enable full access: %q", joinedArgs)
	}

	metadataPath := filepath.Join(config.RunnerStateDir, created.ID+".json")
	plistPath := state.RunnerPlistPath(config.LaunchAgentsDir, created.ID)
	metadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(metadata, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["id"] != created.ID || decoded["cols"] != float64(300) || decoded["rows"] != float64(50) {
		t.Fatalf("unexpected metadata: %s", metadata)
	}
	plist, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tech.somewhere.sessions.runner." + created.ID,
		"<key>KeepAlive</key>", "<key>SuccessfulExit</key>",
		"<string>Interactive</string>", "<key>RUNNER_ID</key>", "<key>SESSIONS_SESSION_ID</key>",
		"<string>" + created.ID + "</string>",
		"<string>a&amp;b&lt;c&gt;</string>",
	} {
		if !strings.Contains(string(plist), want) {
			t.Errorf("plist missing %q:\n%s", want, plist)
		}
	}
	if strings.Contains(string(plist), "NODE_OPTIONS") || strings.Contains(string(plist), "caller-value") ||
		strings.Contains(string(plist), "caller-session") {
		t.Fatalf("unsafe environment leaked into plist:\n%s", plist)
	}
	assertMode(t, metadataPath, 0o600)
	assertMode(t, plistPath, 0o600)
}

func TestWorkingEdgeWritesSentinelAndHookEnvironment(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	launcher := prototest.NewLauncher()
	manager := NewManager(config, launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
	})
	t.Cleanup(manager.Close)
	hookOutput := filepath.Join(root, "hook.txt")
	hookTemporary := hookOutput + ".tmp"
	script := `printf '%s|%s|%s|%s' "$SESSIONS_SESSION_ID" "$SESSIONS_SESSION_NAME" "$SESSIONS_OUTCOME" "$SESSIONS_DURATION_MS" > "` + hookTemporary + `" && mv "` + hookTemporary + `" "` + hookOutput + `"`
	hookWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = hookWatcher.Close() })
	if err := hookWatcher.Add(root); err != nil {
		t.Fatal(err)
	}
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "idle edge", OnIdle: script,
	})
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	runtime := manager.runtimes[created.ID]
	manager.mu.Unlock()
	if runtime == nil {
		t.Fatal("created session has no managed runtime")
	}
	// Drive classifier samples synchronously. The first idle sample only
	// initializes pushWorkingObserved and must not emit an idle edge.
	runtime.tick()
	if _, err := os.Stat(filepath.Join(config.UserStateRoot, "idle", created.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial idle sample wrote a sentinel: %v", err)
	}
	runner := launcher.Runner(created.ID)
	// Keep the byte signal above threshold for several classifier ticks so the
	// true state is observable even under the race detector's instrumentation.
	output := "Completed cleanly, all checks passed.\n" + strings.Repeat("x", 900)
	runner.AddOutput(output)
	<-runtime.outputObserved
	runtime.mu.Lock()
	observedBytes := runtime.recentBytes
	runtime.mu.Unlock()
	if observedBytes < len(output) {
		t.Fatalf("output observer recorded %d bytes, want at least %d", observedBytes, len(output))
	}
	runtime.tick()
	if info, ok := manager.Get(created.ID); !ok || !info.Info().Working {
		t.Fatalf("synchronized working sample was not observable: ok=%v info=%#v", ok, info)
	}
	for attempt := 0; attempt < 10; attempt++ {
		info, _ := manager.Get(created.ID)
		if !info.Info().Working {
			break
		}
		runtime.tick()
	}
	if info, ok := manager.Get(created.ID); !ok || info.Info().Working {
		t.Fatalf("working state did not decay after controlled samples: ok=%v info=%#v", ok, info)
	}
	sentinelPath := filepath.Join(config.UserStateRoot, "idle", created.ID)
	if _, err := os.Stat(sentinelPath); err != nil {
		t.Fatalf("idle sentinel was not written: %v", err)
	}
	awaitFile(t, hookWatcher, hookOutput)
	hook, err := os.ReadFile(hookOutput)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(string(hook), "|")
	if len(parts) != 4 || parts[0] != created.ID || parts[1] != "idle edge" || parts[2] != "done" || parts[3] == "" {
		t.Fatalf("hook environment = %q", hook)
	}
	assertMode(t, sentinelPath, 0o600)
}

func TestDiscoveryPreservesUnreachableLivePID(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-000000000099"
	socket := filepath.Join(config.RunnerStateDir, id+".sock")
	metadata := filepath.Join(config.RunnerStateDir, id+".json")
	if err := os.WriteFile(socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	encoded := `{"id":"` + id + `","cmd":"/bin/sh","args":[],"cwd":"` + root + `","cols":300,"rows":50,"createdAt":1,"pid":1234,"sockPath":"` + socket + `"}`
	if err := os.WriteFile(metadata, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	launcher := &reapRecordingLauncher{Launcher: prototest.NewLauncher()}
	manager := NewManager(config, launcher, ManagerOptions{
		DisableWatchers: true, DiscoveryRetries: 1, DiscoveryDelay: time.Millisecond,
		ProcessAlive: func(int) bool { return true }, ProcessCommand: func(int) string { return "" },
	})
	t.Cleanup(manager.Close)
	if err := manager.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{socket, metadata} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("discovery removed sacred state %s: %v", path, err)
		}
	}
	if len(launcher.reaped) != 0 {
		t.Fatalf("discovery reaped unreachable live runner: %v", launcher.reaped)
	}
}

func TestDiscoveryRecognizesStructuredRunnerAndPreservesHistory(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-000000000098"
	paths := state.For(config.RunnerStateDir, id)
	if err := os.WriteFile(paths.Socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Structured, []byte("{\"source\":\"codex-app-server\"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteMetadata(paths.Meta, state.Metadata{
		ID: id, Kind: state.KindCodexAppServer, Cmd: "codex", Cwd: root,
		Cols: 300, Rows: 50, CreatedAt: 1, PID: 1234, SockPath: paths.Socket,
	}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, DiscoveryRetries: 1, DiscoveryDelay: time.Millisecond,
		ProcessAlive:   func(int) bool { return true },
		ProcessCommand: func(int) string { return "/Applications/Sessions.app/Contents/Resources/sessions-runner" },
	})
	t.Cleanup(manager.Close)
	if err := manager.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.Socket, paths.Meta, paths.Structured} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("discovery removed live structured-session state %s: %v", path, err)
		}
	}
}

func TestDiscoveryNeverDeletesStructuredHistoryForDeadRunner(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-000000000097"
	paths := state.For(config.RunnerStateDir, id)
	for _, historyPath := range []string{paths.Structured, paths.ClaudeP} {
		if err := os.WriteFile(historyPath, []byte("{\"retained\":true}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths.Socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteMetadata(paths.Meta, state.Metadata{
		ID: id, Kind: state.KindCodexAppServer, Cmd: "codex", Cwd: root,
		Cols: 300, Rows: 50, CreatedAt: 1, PID: 1234, SockPath: paths.Socket,
	}); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, DiscoveryRetries: 1, DiscoveryDelay: time.Millisecond,
		ProcessAlive:   func(int) bool { return true },
		ProcessCommand: func(int) string { return "/usr/bin/unrelated-process" },
	})
	t.Cleanup(manager.Close)
	if err := manager.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, coordinationPath := range []string{paths.Socket, paths.Meta} {
		if _, err := os.Stat(coordinationPath); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("discovery left dead coordination artifact %s: %v", coordinationPath, err)
		}
	}
	for _, historyPath := range []string{paths.Structured, paths.ClaudeP} {
		if _, err := os.Stat(historyPath); err != nil {
			t.Fatalf("discovery deleted structured history %s: %v", historyPath, err)
		}
	}
}

func TestDiscoveryPreservesArtifactsWhenMetadataIsCorrupt(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := "00000000-0000-4000-8000-000000000096"
	paths := state.For(config.RunnerStateDir, id)
	for _, path := range []string{paths.Socket, paths.Structured} {
		if err := os.WriteFile(path, []byte("sacred"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(paths.Meta, []byte(`{"id":"`+id+`","pid":`), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, DiscoveryRetries: 1, DiscoveryDelay: time.Millisecond,
	})
	t.Cleanup(manager.Close)
	if err := manager.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{paths.Socket, paths.Meta, paths.Structured} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("discovery removed state with ambiguous metadata %s: %v", path, err)
		}
	}
}

func testConfig(root string) state.Config {
	return state.Config{
		Host: "127.0.0.1", Port: 8787,
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		StateRoot:       filepath.Join(root, "state"),
		UserStateRoot:   filepath.Join(root, "user-state"),
		RunnerStateDir:  filepath.Join(root, "state", "runners"),
		TokenPath:       filepath.Join(root, "state", "token"),
		OpenPath:        filepath.Join(root, "state", "open"),
		LaunchAgentsDir: filepath.Join(root, "LaunchAgents"),
		GlobalHooksPath: filepath.Join(root, "config", "hooks.json"),
		RunnerPath:      "/scratch/runner",
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Fatalf("mode(%s) = %04o, want %04o", path, info.Mode().Perm(), want)
	}
}

func awaitCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitConditionBudget)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("test ended before condition became true"+" (waited %s)", waitConditionBudget)
		}
		time.Sleep(waitConditionPoll)
	}
}

func awaitFile(t *testing.T, watcher *fsnotify.Watcher, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect %s: %v", path, err)
		}
		select {
		case _, ok := <-watcher.Events:
			if !ok {
				t.Fatalf("watcher closed before %s was published", path)
			}
		case err, ok := <-watcher.Errors:
			if ok {
				t.Fatalf("watch %s: %v", path, err)
			}
		case <-time.After(waitConditionBudget):
			t.Fatalf("test ended before %s was published", path)
		}
	}
}

// The mass-kill guard protects the destructive half of a discovery sweep. It
// must not also stop the safe half: nothing else records runner_lost, so a
// refusal that skipped reconciliation left those lanes reported as live in
// every ledger-derived view and no later sweep could get back under the limit.
// DiscoverWithOptions{Force:true} is reachable only from tests, so an operator
// had no way out short of deleting files by hand.
func TestGuardedDiscoverySweepStillReconcilesLedgerAndExplainsTheRefusal(t *testing.T) {
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.LaunchAgentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	old := time.Now().Add(-time.Minute)
	ids := make([]string, 0, DefaultMassKillLimit+1)
	paths := make([]string, 0, DefaultMassKillLimit+1)
	for index := 0; index < DefaultMassKillLimit+1; index++ {
		id := fmt.Sprintf("00000000-0000-4000-8000-00000000000%d", index)
		ids = append(ids, id)
		path := state.RunnerPlistPath(config.LaunchAgentsDir, id)
		if err := os.WriteFile(path, []byte("scratch"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
		if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
			Meta: ledger.Meta{LaneID: id}, LaneUUID: id, Tool: "terminal", Cwd: root,
			CreatorKind: ledger.CreatorExternal, CreatorID: "guard-test",
		}); err != nil {
			t.Fatal(err)
		}
	}

	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		Notify: func(PushPayload) {},
	})
	t.Cleanup(manager.Close)

	discoverErr := manager.Discover(ctx)
	var guardErr *MassKillError
	if !errors.As(discoverErr, &guardErr) || guardErr.Count != DefaultMassKillLimit+1 || guardErr.Limit != DefaultMassKillLimit {
		t.Fatalf("Discover() error = %v, want a mass-kill refusal for %d candidates", discoverErr, DefaultMassKillLimit+1)
	}
	// The destructive half stays guarded.
	for _, path := range paths {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("guarded sweep mutated %s: %v", path, statErr)
		}
	}
	// The safe half still runs.
	events, err := store.Events(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	reconciled := make(map[string]bool, len(ids))
	for _, lane := range ledger.Fold(events) {
		if lane.RunnerLost {
			reconciled[lane.LaneID] = true
		}
	}
	for _, id := range ids {
		if !reconciled[id] {
			t.Fatalf("guarded sweep skipped ledger reconciliation for lane %s: %#v", id, reconciled)
		}
	}
	// The refusal names what it refused and what is safe to do next.
	message := discoverErr.Error()
	for _, want := range []string{
		"discovery", "left in place", "sessions kill", fmt.Sprintf("limit %d", DefaultMassKillLimit),
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("mass-kill refusal %q does not mention %q", message, want)
		}
	}
	if strings.Contains(message, "retry with force") {
		t.Fatalf("discovery refusal points at a force flag no operator surface exposes: %q", message)
	}
	t.Logf("guarded discovery refusal: %s", message)
}
