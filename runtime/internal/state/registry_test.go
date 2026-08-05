package state

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
		DefaultShell: "/bin/sh", DefaultCwd: root, DefaultCols: 80, DefaultRows: 24,
		RunnerStateDir: filepath.Join(root, "runners"),
	}
	registry := NewRegistry(config, nil)
	environment := registry.runnerEnvironment(proto.RunnerInfo{
		ID: "11111111-1111-4111-8111-111111111111", Cmd: "/bin/sh", Cwd: root, Cols: 80, Rows: 24,
	}, map[string]string{
		"runner_id":    "forged",
		"codex_home":   filepath.Join(root, "forged-codex"),
		"node_options": "--require=forged.js",
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
