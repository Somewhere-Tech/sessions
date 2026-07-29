package state_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type orderingLauncher struct {
	base       *prototest.Launcher
	store      *ledger.Store
	launched   bool
	killRunner *orderingRunner
}

func (l *orderingLauncher) ProgramArguments(request proto.LaunchRequest) []string {
	return l.base.ProgramArguments(request)
}

func (l *orderingLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	events, err := l.store.Events(ctx, request.Info.ID)
	if err != nil {
		return nil, err
	}
	if len(events) < 2 || events[0].Type != ledger.EventCreated || events[1].Type != ledger.EventLaunchStarted {
		return nil, fmt.Errorf("launch observed events %#v, want committed created then launch_started", events)
	}
	l.launched = true
	runner, err := l.base.Launch(ctx, request)
	if err != nil {
		return nil, err
	}
	l.killRunner = &orderingRunner{Runner: runner, store: l.store, laneID: request.Info.ID}
	return l.killRunner, nil
}

func (l *orderingLauncher) Attach(ctx context.Context, info proto.RunnerInfo) (proto.Runner, error) {
	return l.base.Attach(ctx, info)
}

type orderingRunner struct {
	proto.Runner
	store       *ledger.Store
	laneID      string
	killOrdered bool
}

func (r *orderingRunner) Kill(ctx context.Context) error {
	events, err := r.store.Events(ctx, r.laneID)
	if err != nil {
		return err
	}
	if len(events) == 0 || events[len(events)-1].Type != ledger.EventUserKillRequested {
		return fmt.Errorf("kill observed no committed tombstone: %#v", events)
	}
	r.killOrdered = true
	return r.Runner.Kill(ctx)
}

func ledgerTestConfig(t *testing.T) state.Config {
	t.Helper()
	root := t.TempDir()
	return state.Config{
		DefaultShell: "/bin/bash", DefaultCwd: root, DefaultCols: 300, DefaultRows: 50,
		StateRoot: filepath.Join(root, "state"), RunnerStateDir: filepath.Join(root, "state", "runners"),
		LaunchAgentsDir: filepath.Join(root, "agents"),
	}
}

func TestLedgerWriteAheadBoundariesAreCommittedBeforeLaunchAndKill(t *testing.T) {
	config := ledgerTestConfig(t)
	store, err := ledger.Open(context.Background(), ledger.Options{
		Path: filepath.Join(t.TempDir(), "ledger", "lanes.sqlite3"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	launcher := &orderingLauncher{base: prototest.NewLauncher(), store: store}
	manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
		DisableWatchers: true,
		Boundaries:      store.Boundaries(),
		Observations:    store.Observations(),
		LedgerReader:    store,
	})
	defer manager.Close()
	provider := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "claude", Cwd: config.DefaultCwd,
		Args: []string{"--resume", provider, "--append-system-prompt", "NEVER-PERSIST-THIS"},
		Name: "write ahead",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !launcher.launched {
		t.Fatal("launcher was not called")
	}
	events, err := store.Events(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 5 {
		t.Fatalf("lifecycle events=%#v", events)
	}
	if encoded := string(events[0].Payload); strings.Contains(encoded, "NEVER-PERSIST") ||
		!strings.Contains(encoded, `"argv":["claude","--resume","`+provider+`"]`) {
		t.Fatalf("unsafe or incomplete created payload: %s", encoded)
	}
	_, ok := manager.Get(created.ID)
	if !ok {
		t.Fatal("created session not registered")
	}
	if err := manager.RequestKill(context.Background(), created.ID, false); err != nil {
		t.Fatal(err)
	}
	if launcher.killRunner == nil || !launcher.killRunner.killOrdered {
		t.Fatal("runner kill happened without observing a committed tombstone")
	}
	waitForLedger(t, func() bool {
		events, readErr := store.Events(context.Background(), created.ID)
		if readErr != nil {
			return false
		}
		for _, event := range events {
			if event.Type == ledger.EventRunnerExited {
				return true
			}
		}
		return false
	})
	finalEvents, err := store.Events(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("lane=%s launch_saw_committed_created=%t kill_saw_committed_tombstone=%t lifecycle_events=%d", created.ID, launcher.launched, launcher.killRunner.killOrdered, len(finalEvents))
}

func waitForLedger(t *testing.T, condition func() bool) {
	t.Helper()
	for !condition() {
		select {
		case <-t.Context().Done():
			t.Fatal("test ended before ledger lifecycle event arrived")
		default:
			runtime.Gosched()
		}
	}
}

type failingBoundary struct {
	createErr error
	killErr   error
}

func (f failingBoundary) RecordCreated(context.Context, ledger.Created) error { return f.createErr }
func (f failingBoundary) RecordProviderRebound(context.Context, ledger.ProviderRebound) error {
	return nil
}
func (f failingBoundary) RecordUserKill(context.Context, ledger.UserKill) error { return f.killErr }

type countingLauncher struct {
	*prototest.Launcher
	launches int
}

func (l *countingLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	l.launches++
	return l.Launcher.Launch(ctx, request)
}

func TestLedgerBoundaryErrorsAbortTheirSideEffects(t *testing.T) {
	t.Run("created failure aborts launch", func(t *testing.T) {
		config := ledgerTestConfig(t)
		launcher := &countingLauncher{Launcher: prototest.NewLauncher()}
		manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
			DisableWatchers: true,
			Boundaries:      failingBoundary{createErr: errors.New("disk full")},
		})
		defer manager.Close()
		_, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: config.DefaultCwd})
		if err == nil || !strings.Contains(err.Error(), "before launch") {
			t.Fatalf("create err=%v", err)
		}
		if launcher.launches != 0 {
			t.Fatalf("launcher called %d times after failed RecordCreated", launcher.launches)
		}
	})

	t.Run("tombstone failure aborts kill", func(t *testing.T) {
		config := ledgerTestConfig(t)
		launcher := prototest.NewLauncher()
		boundary := failingBoundary{killErr: errors.New("read only")}
		manager := sessionruntime.NewManager(config, launcher, sessionruntime.ManagerOptions{
			DisableWatchers: true,
			Boundaries:      boundary,
		})
		defer manager.Close()
		created, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: config.DefaultCwd})
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.RequestKill(context.Background(), created.ID, false); err == nil || !strings.Contains(err.Error(), "before runner kill") {
			t.Fatalf("kill err=%v", err)
		}
		if !manager.Input(context.Background(), created.ID, "still alive") {
			t.Fatal("runner was killed despite failed tombstone")
		}
	})
}
