package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type stalledStartLauncher struct {
	*prototest.Launcher
	started chan proto.LaunchRequest
	release chan struct{}
}

func (l *stalledStartLauncher) Launch(ctx context.Context, request proto.LaunchRequest) (proto.Runner, error) {
	if request.Info.Cmd != "/bin/sh" {
		return l.Launcher.Launch(ctx, request)
	}
	l.started <- request
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.release:
		return l.Launcher.Launch(ctx, request)
	}
}

func TestDiscoveryDoesNotDeclareAnExecutingLaunchLost(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SESSIONS_STATE_DIR", filepath.Join(root, "runners"))
	t.Setenv("SESSIONS_LEDGER_PATH", filepath.Join(root, "ledger", "lanes.sqlite3"))
	t.Setenv("SESSIONS_PORT", "8899")
	store := openStartLedger(t, root)
	launcher := &stalledStartLauncher{Launcher: prototest.NewLauncher(), started: make(chan proto.LaunchRequest, 1), release: make(chan struct{})}
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := manager.Create(ctx, state.CreateSessionRequest{Name: "blocked", Kind: state.KindLane, Cmd: "/bin/sh", Cwd: root, OperationID: testStartOperation})
		done <- err
	}()
	var request proto.LaunchRequest
	select {
	case request = <-launcher.started:
	case <-ctx.Done():
		t.Fatal("create did not reach launch")
	}
	for i := 0; i < 2; i++ {
		if err := manager.Discover(ctx); err != nil {
			t.Fatal(err)
		}
		lanes, err := store.CurrentStates(ctx)
		if err != nil || len(lanes) != 1 || lanes[0].RunnerLost || lanes[0].RunnerReady {
			t.Fatalf("an executing launch was declared lost or ready: %+v, %v", lanes, err)
		}
		listed := manager.List(false)
		if len(listed) != 1 || listed[0].ID != request.Info.ID || !listed[0].Launching ||
			listed[0].Working || listed[0].Unreachable || listed[0].Exited ||
			listed[0].Start == nil || listed[0].Start.OperationID != testStartOperation {
			t.Fatalf("recorded launch is missing or misclassified in listing: %+v", listed)
		}
	}
	close(launcher.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, tracked := manager.starting.Load(request.Info.ID); tracked {
		t.Fatal("completed launch still tracked as in flight")
	}
	listed := manager.List(false)
	if len(listed) != 1 || listed[0].Launching {
		t.Fatalf("settled launch still looks starting: %+v", listed)
	}
	lanes, err := store.CurrentStates(ctx)
	if err != nil || len(lanes) != 1 || !lanes[0].RunnerReady || lanes[0].RunnerLost {
		t.Fatalf("ready launch state = %+v, %v", lanes, err)
	}
}

func TestStalledLaunchDoesNotBlockUnrelatedCreationOrDuplicateItsOperation(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SESSIONS_STATE_DIR", filepath.Join(root, "runners"))
	t.Setenv("SESSIONS_LEDGER_PATH", filepath.Join(root, "ledger", "lanes.sqlite3"))
	t.Setenv("SESSIONS_PORT", "8899")
	store := openStartLedger(t, root)
	launcher := &stalledStartLauncher{Launcher: prototest.NewLauncher(), started: make(chan proto.LaunchRequest, 1), release: make(chan struct{})}
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	request := state.CreateSessionRequest{Name: "blocked", Cmd: "/bin/sh", Cwd: root, OperationID: testStartOperation}
	go func() { _, err := manager.Create(ctx, request); done <- err }()
	var launch proto.LaunchRequest
	select {
	case launch = <-launcher.started:
	case <-ctx.Done():
		t.Fatal("first creation never reached launch")
	}
	short, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	other, err := manager.Create(short, state.CreateSessionRequest{Cmd: "/usr/bin/true", Cwd: root, OperationID: testPromptOperation})
	if err != nil || other.ID == launch.Info.ID {
		t.Fatalf("unrelated create blocked behind launch: %+v %v", other, err)
	}
	_, err = manager.Create(short, request)
	var replay *state.StartCreateReplayError
	if !errors.As(err, &replay) || replay.SessionID != launch.Info.ID || !strings.Contains(replay.Reason, "still starting") {
		t.Fatalf("in-flight replay did not return its recorded session: %v", err)
	}
	close(launcher.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if len(launcher.Launches) != 2 {
		t.Fatalf("duplicate runtime launched: %d launches", len(launcher.Launches))
	}
}

func TestCreationQueueHonorsCancellation(t *testing.T) {
	manager := &Manager{bindGate: make(chan struct{}, 1)}
	release, err := manager.acquireCreation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := manager.acquireCreation(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("creation queue ignored deadline: %v", err)
	}
}

func TestFailedLaunchStopsBeingTrackedButKeepsItsRecordedIdentity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("SESSIONS_STATE_DIR", filepath.Join(root, "runners"))
	t.Setenv("SESSIONS_LEDGER_PATH", filepath.Join(root, "ledger", "lanes.sqlite3"))
	t.Setenv("SESSIONS_PORT", "8899")
	store := openStartLedger(t, root)
	launcher := prototest.NewLauncher()
	launcher.Err = errors.New("runner did not create socket")
	manager := startReplayManager(t, root, launcher, store)
	_, err := manager.Create(context.Background(), state.CreateSessionRequest{Kind: state.KindLane, Cmd: "/bin/sh", Cwd: root, OperationID: testStartOperation})
	var failed *state.StartCreateFailedError
	if !errors.As(err, &failed) || failed.SessionID == "" || failed.OperationID != testStartOperation {
		t.Fatalf("failed launch lost its recovery identity: %v", err)
	}
	if _, tracked := manager.starting.Load(failed.SessionID); tracked {
		t.Fatal("failed launch still tracked as in flight")
	}
	manager.reconcileLedger(context.Background())
	lanes, err := store.CurrentStates(context.Background())
	if err != nil || len(lanes) != 1 || !lanes[0].RunnerLost || lanes[0].RunnerReady {
		t.Fatalf("failed launch state = %+v, %v", lanes, err)
	}
}
