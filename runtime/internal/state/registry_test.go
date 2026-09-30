package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/mirror"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
)

type failingReapLauncher struct {
	*prototest.Launcher
	reaped []string
}

func (l *failingReapLauncher) Reap(id string) error {
	l.reaped = append(l.reaped, id)
	return nil
}

func TestCreateReapsOnlyItsFailedLaunchRegistration(t *testing.T) {
	root := t.TempDir()
	launcher := &failingReapLauncher{Launcher: prototest.NewLauncher()}
	launcher.Err = errors.New("runner did not create socket")
	registry := NewRegistry(Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}, launcher)
	_, err := registry.Create(context.Background(), CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err == nil || !strings.Contains(err.Error(), "runner did not create socket") {
		t.Fatalf("Create() error = %v", err)
	}
	if len(launcher.reaped) != 1 || launcher.reaped[0] == "" {
		t.Fatalf("failed launch cleanup = %#v, want its one generated id", launcher.reaped)
	}
	if sessions := registry.List(true); len(sessions) != 0 {
		t.Fatalf("failed launch registered sessions = %#v", sessions)
	}
}

func TestDiscoveryAttachesKnownSocketsAndPreservesUnknownOnes(t *testing.T) {
	root := t.TempDir()
	config := Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	launcher := prototest.NewLauncher()
	first := NewRegistry(config, launcher)
	created, err := first.Create(context.Background(), CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, Name: "survives discovery",
		Tags: map[string]string{"Product.Line": " Sessions "},
	})
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(config.RunnerStateDir, created.ID+".sock")
	if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	second := NewRegistry(config, launcher)
	if err := second.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sessions := second.List(false); len(sessions) != 1 || sessions[0].ID != created.ID || sessions[0].Name != "survives discovery" || sessions[0].Tags["product.line"] != "Sessions" {
		t.Fatalf("discovered sessions = %#v", sessions)
	}
	metadata, err := ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, created.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Name != "survives discovery" {
		t.Fatalf("persisted metadata name = %q", metadata.Name)
	}
	if metadata.Tags["product.line"] != "Sessions" {
		t.Fatalf("persisted metadata tags = %#v", metadata.Tags)
	}
	updated, err := second.UpdateTags(created.ID, map[string]string{"team": "native"})
	if err != nil {
		t.Fatal(err)
	}
	if updated["team"] != "native" || len(updated) != 1 {
		t.Fatalf("UpdateTags() = %#v", updated)
	}
	metadata, err = ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, created.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Tags["team"] != "native" || len(metadata.Tags) != 1 {
		t.Fatalf("updated metadata tags = %#v", metadata.Tags)
	}

	unknownID := "00000000-0000-4000-8000-000000000000"
	unknownSocket := filepath.Join(config.RunnerStateDir, unknownID+".sock")
	unknownMetadata := filepath.Join(config.RunnerStateDir, unknownID+".json")
	if err := os.WriteFile(unknownSocket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	encoded := `{"id":"` + unknownID + `","cmd":"/bin/sh","args":[],"cwd":"` + root + `","cols":300,"rows":50,"createdAt":1,"pid":999,"sockPath":"` + unknownSocket + `"}`
	if err := os.WriteFile(unknownMetadata, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := second.Discover(context.Background()); err == nil {
		t.Fatal("discovering an unavailable fake runner unexpectedly succeeded")
	}
	for _, path := range []string{unknownSocket, unknownMetadata} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("discovery removed sacred state %s: %v", path, err)
		}
	}
}

func TestCreateRefusesMissingRunnerBeforeWritingState(t *testing.T) {
	root := t.TempDir()
	config := Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	registry := NewRegistry(config, NewLaunchdLauncher(config))
	_, err := registry.Create(context.Background(), CreateSessionRequest{Cmd: "/bin/sh", Cwd: root})
	if err == nil || !strings.Contains(err.Error(), "runner executable unavailable") {
		t.Fatalf("Create() error = %v, want clear runner executable refusal", err)
	}
	for _, path := range []string{config.RunnerStateDir, config.LaunchAgentsDir} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed create mutated %s: %v", path, statErr)
		}
	}
}

func TestCreateRefusesMissingSessionCommandBeforeWritingState(t *testing.T) {
	root := t.TempDir()
	runner := filepath.Join(root, "sessions-runner")
	if err := os.WriteFile(runner, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	config := Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerPath: runner, RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	registry := NewRegistry(config, NewLaunchdLauncher(config))
	launchStarted := false
	_, err := registry.CreateWithLifecycle(context.Background(), CreateSessionRequest{
		Cmd: "missing-agent", Cwd: root, Env: map[string]string{"PATH": filepath.Join(root, "bin")},
	}, CreateLifecycle{LaunchStarted: func(context.Context, PreparedSession) { launchStarted = true }})
	if err == nil || !strings.Contains(err.Error(), "is not executable in the Sessions runner PATH") {
		t.Fatalf("Create() error = %v, want clear missing command refusal", err)
	}
	if launchStarted {
		t.Fatal("missing command crossed the durable launch-started boundary")
	}
	for _, path := range []string{config.RunnerStateDir, config.LaunchAgentsDir} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("failed command preflight mutated %s: %v", path, statErr)
		}
	}
}

func TestLaunchdPathIncludesCommonUserAgentLocations(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	existing := "/usr/bin:/bin:/opt/homebrew/bin"
	parts := strings.Split(launchdPath(existing), ":")
	want := []string{
		filepath.Join(home, ".local", "bin"),
		filepath.Join(home, ".npm-global", "bin"),
		filepath.Join(home, ".bun", "bin"),
		filepath.Join(home, ".cargo", "bin"),
		"/opt/homebrew/sbin",
		"/usr/local/bin",
	}
	for _, expected := range want {
		count := 0
		for _, part := range parts {
			if part == expected {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("launchdPath() contains %q %d times, want exactly once: %q", expected, count, parts)
		}
	}
	if count := strings.Count(launchdPath(existing), "/opt/homebrew/bin"); count != 1 {
		t.Fatalf("existing Homebrew path duplicated %d times", count)
	}
}

func TestRunnerEnvironmentRejectsCaseVariantsOfProtectedKeys(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	config := Config{
		Host: "127.0.0.1", Port: 9911,
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 80, DefaultRows: 24,
		RunnerStateDir: filepath.Join(root, "runners"),
	}
	registry := NewRegistry(config, nil)
	environment := registry.runnerEnvironment(proto.RunnerInfo{
		ID: "11111111-1111-4111-8111-111111111111", Cmd: "/bin/sh", Cwd: root, Cols: 80, Rows: 24,
	}, map[string]string{
		"runner_id":          "forged",
		"codex_home":         filepath.Join(root, "forged-codex"),
		"node_options":       "--require=forged.js",
		"SESSIONS_HOST":      "127.0.0.99",
		"SESSIONS_PORT":      "8787",
		"SESSIONS_STATE_DIR": filepath.Join(root, "wrong-runners"),
	})
	for key := range environment {
		switch strings.ToUpper(key) {
		case "CODEX_HOME", "NODE_OPTIONS":
			t.Fatalf("protected key escaped filtering through case variant: %q", key)
		}
	}
	if got := environment["RUNNER_ID"]; got != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("RUNNER_ID = %q, want canonical session identity", got)
	}
	if got := environment["SESSIONS_HOST"]; got != config.Host {
		t.Fatalf("SESSIONS_HOST = %q, want owning daemon host %q", got, config.Host)
	}
	if got := environment["SESSIONS_PORT"]; got != "9911" {
		t.Fatalf("SESSIONS_PORT = %q, want owning daemon port", got)
	}
	if got := environment["SESSIONS_STATE_DIR"]; got != config.RunnerStateDir {
		t.Fatalf("SESSIONS_STATE_DIR = %q, want owning daemon runner state %q", got, config.RunnerStateDir)
	}
}

func TestDiscoveryStressConcurrentFakeRunnersSkipsTruncatedMetadata(t *testing.T) {
	const runnerCount = 96
	root := t.TempDir()
	runnerDir := filepath.Join(root, "runners")
	if err := os.MkdirAll(runnerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := Config{
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 80, DefaultRows: 24,
		RunnerStateDir: runnerDir, LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	launcher := prototest.NewLauncher()
	registry := NewRegistry(config, launcher)

	malformedID := "malformed-runner"
	if err := os.WriteFile(filepath.Join(runnerDir, malformedID+".sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runnerDir, malformedID+".json"), []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	writeErrors := make(chan error, runnerCount)
	ids := make([]string, runnerCount)
	var writers sync.WaitGroup
	for index := 0; index < runnerCount; index++ {
		index := index
		ids[index] = fmt.Sprintf("%08x-0000-4000-8000-%012x", index+1, index+1)
		writers.Add(1)
		go func() {
			defer writers.Done()
			<-start
			id := ids[index]
			socketPath := filepath.Join(runnerDir, id+".sock")
			info := proto.RunnerInfo{
				ID: id, Cmd: "/bin/sh", Cwd: root, Cols: 80, Rows: 24,
				CreatedAt: int64(index + 1), SocketPath: socketPath,
			}
			runner, err := launcher.Launch(t.Context(), proto.LaunchRequest{Info: info})
			if err != nil {
				writeErrors <- err
				return
			}
			if err := os.WriteFile(socketPath, nil, 0o600); err != nil {
				writeErrors <- err
				return
			}
			metadataPath := filepath.Join(runnerDir, id+".json")
			if err := os.WriteFile(metadataPath, []byte(`{"id":"`+id+`"`), 0o600); err != nil {
				writeErrors <- err
				return
			}
			runtime.Gosched()
			actual := runner.Info()
			if err := WriteMetadata(metadataPath, Metadata{
				ID: actual.ID, Name: fmt.Sprintf("runner-%03d", index),
				Cmd: actual.Cmd, Args: actual.Args, Cwd: actual.Cwd,
				Cols: actual.Cols, Rows: actual.Rows, CreatedAt: actual.CreatedAt,
				PID: actual.PID, SockPath: actual.SocketPath,
			}); err != nil {
				writeErrors <- err
			}
		}()
	}
	writersDone := make(chan struct{})
	go func() {
		writers.Wait()
		close(writersDone)
	}()
	close(start)

	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	transientErrors := 0
	writersFinished := false
	for len(registry.List(false)) < runnerCount {
		if err := registry.Discover(ctx); err != nil {
			transientErrors++
		}
		select {
		case <-writersDone:
			writersFinished = true
		case <-ctx.Done():
			t.Fatalf("discovery timed out with %d/%d runners: %v", len(registry.List(false)), runnerCount, ctx.Err())
		default:
		}
		if writersFinished {
			// All files are now stable. One last scan must attach every valid
			// runner even though the permanent malformed entry is still skipped.
			_ = registry.Discover(ctx)
			break
		}
		runtime.Gosched()
	}
	if !writersFinished {
		<-writersDone
	}
	close(writeErrors)
	for err := range writeErrors {
		t.Errorf("materialize fake runner: %v", err)
	}
	if t.Failed() {
		return
	}
	discovered := len(registry.List(false))
	if discovered != runnerCount {
		t.Fatalf("discovered=%d, want %d", discovered, runnerCount)
	}
	if _, exists := registry.Get(malformedID); exists {
		t.Fatal("runner with truncated metadata was attached")
	}
	if _, err := os.Stat(filepath.Join(runnerDir, malformedID+".json")); err != nil {
		t.Fatalf("discovery removed malformed metadata: %v", err)
	}
	for _, id := range ids {
		if runner := launcher.Runner(id); runner != nil {
			runner.Emit(proto.Event{Kind: proto.EventRunnerLost})
		}
	}
	t.Logf("concurrent_fake_runners=%d discovered=%d malformed_skipped=1 transient_scans=%d",
		runnerCount, discovered, transientErrors)
}

// applyEvent broadcasts under the session write lock, and the pump goroutine
// that calls it is the only sender on a subscriber channel. Draining a full
// subscriber with a bare receive can therefore block forever if the slow
// client catches up between the failed send and the drain, which strands the
// write lock and hangs Info, Attach, Input, Snapshot, and Registry.List for
// every caller. The drain must be non-blocking, exactly as
// proto.SocketRunner.broadcastLocked does it.
func TestTerminalBroadcastNeverDeadlocksWhenASlowClientCatchesUp(t *testing.T) {
	const rounds = 200
	const subscribers = 64
	for round := 0; round < rounds; round++ {
		session := &Session{subs: make(map[uint64]chan proto.Event), nextSeq: 1}
		var readers sync.WaitGroup
		for index := 0; index < subscribers; index++ {
			stream := make(chan proto.Event, 1)
			// Full buffer: the non-blocking send in applyEvent must fail.
			stream <- proto.Event{Kind: proto.EventOutput, Output: proto.OutputEvent{Seq: 1, Data: "backlog"}}
			session.subs[uint64(index)] = stream
			readers.Add(1)
			go func(stream chan proto.Event) {
				defer readers.Done()
				<-stream
			}(stream)
		}
		code := 0
		done := make(chan bool, 1)
		go func() {
			done <- session.applyEvent(proto.Event{Kind: proto.EventExit, Exit: proto.ExitEvent{Code: &code}})
		}()
		select {
		case terminal := <-done:
			if !terminal {
				t.Fatalf("round %d: exit event was not terminal", round)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d: terminal broadcast deadlocked draining a subscriber while holding the session write lock", round)
		}
		info := make(chan SessionInfo, 1)
		go func() { info <- session.Info() }()
		select {
		case got := <-info:
			if !got.Exited {
				t.Fatalf("round %d: session info = %#v, want exited", round, got)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("round %d: session write lock was never released after the terminal broadcast", round)
		}
		readers.Wait()
	}
}

// Dropping a slow client's backlog is intentional; WebSocket reconnect and
// replay repair it from sequence numbers. The terminal event is the one event
// that must still arrive, and non-terminal events must still be dropped rather
// than queued behind a stalled reader.
func TestFullSubscriberDropsBacklogForTerminalAndDropsNonTerminalEvents(t *testing.T) {
	code := 0
	terminalSession := &Session{subs: make(map[uint64]chan proto.Event), nextSeq: 1}
	terminalStream := make(chan proto.Event, 1)
	terminalStream <- proto.Event{Kind: proto.EventOutput, Output: proto.OutputEvent{Seq: 1, Data: "backlog"}}
	terminalSession.subs[0] = terminalStream
	if !terminalSession.applyEvent(proto.Event{Kind: proto.EventExit, Exit: proto.ExitEvent{Code: &code}}) {
		t.Fatal("exit event was not terminal")
	}
	delivered := make([]proto.Event, 0, 2)
	for event := range terminalStream {
		delivered = append(delivered, event)
	}
	if len(delivered) != 1 || delivered[0].Kind != proto.EventExit {
		t.Fatalf("terminal delivery to a full subscriber = %#v, want the exit event only", delivered)
	}

	outputSession := &Session{subs: make(map[uint64]chan proto.Event), nextSeq: 1}
	outputSession.mirror = mustTestMirror(t)
	outputStream := make(chan proto.Event, 1)
	outputStream <- proto.Event{Kind: proto.EventOutput, Output: proto.OutputEvent{Seq: 1, Data: "backlog"}}
	outputSession.subs[0] = outputStream
	if outputSession.applyEvent(proto.Event{Kind: proto.EventOutput, Output: proto.OutputEvent{Seq: 2, Data: "dropped"}}) {
		t.Fatal("output event reported terminal")
	}
	queued := <-outputStream
	if queued.Output.Data != "backlog" {
		t.Fatalf("full subscriber queue = %#v, want the original backlog and a dropped new event", queued)
	}
	select {
	case extra := <-outputStream:
		t.Fatalf("non-terminal event was not dropped for a full subscriber: %#v", extra)
	default:
	}
}

// List(true) includes exited sessions, which registry.register removes after
// the post-exit grace period. Deep diagnostics must survive a request that
// races that timer instead of panicking the handler goroutine on a nil
// *Session.
func TestDeepDiagnosticsSurvivesSessionEvictedBetweenListAndGet(t *testing.T) {
	staged := false
	for attempt := 0; attempt < 10 && !staged; attempt++ {
		root := t.TempDir()
		registry := NewRegistry(Config{
			DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
			RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
		}, prototest.NewLauncher())
		evicted, err := registry.Create(context.Background(), CreateSessionRequest{Cmd: "/bin/sh", Cwd: root, Name: "evicted"})
		if err != nil {
			t.Fatal(err)
		}
		survivor, err := registry.Create(context.Background(), CreateSessionRequest{Cmd: "/bin/sh", Cwd: root, Name: "survivor"})
		if err != nil {
			t.Fatal(err)
		}
		session, ok := registry.Get(evicted.ID)
		if !ok {
			t.Fatal("created session is not registered")
		}
		// Hold the session lock so List() parks inside Info() after it has
		// already released the registry lock and snapshotted both sessions.
		session.mu.Lock()
		diagnostics := make(chan []map[string]any, 1)
		go func() { diagnostics <- registry.DeepDiagnostics() }()
		time.Sleep(50 * time.Millisecond)
		// Evict exactly the way the post-exit grace timer does.
		registry.mu.Lock()
		delete(registry.sessions, evicted.ID)
		registry.removeOrderLocked(evicted.ID)
		registry.mu.Unlock()
		session.mu.Unlock()

		result := <-diagnostics
		byID := make(map[string]map[string]any, len(result))
		for _, entry := range result {
			byID[entry["id"].(string)] = entry
		}
		if _, listed := byID[evicted.ID]; !listed {
			continue
		}
		staged = true
		if byID[evicted.ID]["claudeEvents"] != int64(0) {
			t.Fatalf("evicted session diagnostics = %#v, want a zero claude event count", byID[evicted.ID])
		}
		if _, listed := byID[survivor.ID]; !listed {
			t.Fatalf("deep diagnostics dropped the surviving session: %#v", result)
		}
	}
	if !staged {
		t.Fatal("could not stage a diagnostics request racing the post-exit eviction")
	}
}

func mustTestMirror(t *testing.T) *mirror.Mirror {
	t.Helper()
	terminal, err := mirror.NewSize(80, 24)
	if err != nil {
		t.Fatal(err)
	}
	return terminal
}

// Sessions gives a fresh Claude session a stable --session-id. A request that
// already names a conversation must be left alone in every real spelling, or
// Sessions launches `claude -r <uuid> --session-id <fresh uuid>` and the
// caller's resume silently becomes a different conversation.
func TestClaudeSessionIDIsInjectedOnlyWhenNoConversationIsNamed(t *testing.T) {
	const fresh = "99999999-9999-4999-8999-999999999999"
	const provider = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	untouched := [][]string{
		{"--session-id", provider},
		{"--resume", provider},
		{"-r", provider},
		{"--resume=" + provider},
		{"--session-id=" + provider},
		{"--model", "opus", "-r", provider, "--verbose"},
	}
	for _, args := range untouched {
		got := appendClaudeSessionID("claude", args, fresh)
		if !reflect.DeepEqual(got, args) {
			t.Fatalf("appendClaudeSessionID(%q) = %q, want the request unchanged", args, got)
		}
	}
	injected := appendClaudeSessionID("claude", []string{"--model", "opus"}, fresh)
	if !reflect.DeepEqual(injected, []string{"--model", "opus", "--session-id", fresh}) {
		t.Fatalf("fresh Claude session args = %q", injected)
	}
	if got := appendClaudeSessionID("/bin/bash", []string{"-i"}, fresh); !reflect.DeepEqual(got, []string{"-i"}) {
		t.Fatalf("non-Claude command args = %q", got)
	}
}

// A resumed provider conversation is one durable conversation even when it has
// had several runner processes. The live SessionInfo and its metadata must
// therefore carry the provider id from the launch argv; otherwise the UI can
// only see unrelated runtime ids and renders the ended predecessor beside the
// live successor as two conversations.
func TestCreateBindsProviderConversationIdentityFromArgs(t *testing.T) {
	root := t.TempDir()
	config := Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}
	const providerID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	tests := []struct {
		name             string
		cmd              string
		args             []string
		wantClaudeID     string
		wantConversation string
	}{
		{name: "Claude resume", cmd: "claude", args: []string{"--resume", providerID}, wantClaudeID: providerID, wantConversation: providerID},
		{name: "Claude short resume", cmd: "claude", args: []string{"-r", providerID}, wantClaudeID: providerID, wantConversation: providerID},
		{name: "Claude joined resume", cmd: "claude", args: []string{"--resume=" + providerID}, wantClaudeID: providerID, wantConversation: providerID},
		{name: "Codex resume", cmd: "codex", args: []string{"resume", providerID}, wantConversation: providerID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := NewRegistry(config, prototest.NewLauncher())
			created, err := registry.Create(context.Background(), CreateSessionRequest{
				Cmd: test.cmd, Args: test.args, Cwd: root,
			})
			if err != nil {
				t.Fatal(err)
			}
			if created.ConversationID != test.wantConversation || created.ClaudeSessionID != test.wantClaudeID {
				t.Fatalf("live provider identity = conversation %q, Claude %q; want %q, %q", created.ConversationID, created.ClaudeSessionID, test.wantConversation, test.wantClaudeID)
			}
			metadata, err := ReadRunnerMetadata(filepath.Join(config.RunnerStateDir, created.ID+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Info.ConversationID != test.wantConversation || metadata.Info.ClaudeSessionID != test.wantClaudeID {
				t.Fatalf("persisted provider identity = conversation %q, Claude %q; want %q, %q", metadata.Info.ConversationID, metadata.Info.ClaudeSessionID, test.wantConversation, test.wantClaudeID)
			}
		})
	}
}

func TestRegisterMetadataRestoresIdentityOmittedByOlderRunnerHello(t *testing.T) {
	root := t.TempDir()
	registry := NewRegistry(Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		RunnerStateDir: filepath.Join(root, "runners"), LaunchAgentsDir: filepath.Join(root, "agents"),
	}, nil)
	const providerID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	runner := prototest.NewRunner(proto.RunnerInfo{
		ID: "runtime-id", Cmd: "claude", Args: []string{"--resume", providerID}, Cwd: root,
		Cols: 300, Rows: 50, CreatedAt: 1, ProtocolVersion: proto.ProtocolVersion,
	})
	session, err := registry.RegisterMetadata(context.Background(), runner, RunnerMetadata{
		Info: proto.RunnerInfo{
			ID: "runtime-id", ConversationID: providerID, ClaudeSessionID: providerID,
		},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	info := session.Info()
	if info.ConversationID != providerID || info.ClaudeSessionID != providerID {
		t.Fatalf("discovered provider identity = conversation %q, Claude %q", info.ConversationID, info.ClaudeSessionID)
	}
}
