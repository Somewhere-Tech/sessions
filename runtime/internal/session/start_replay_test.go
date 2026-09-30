package session

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const (
	testStartOperation  = "5a000000-0000-4000-8000-000000000001"
	testPromptOperation = "5a000000-0000-4000-8000-000000000002"
)

func startReplayManager(t *testing.T, root string, launcher *prototest.Launcher, store *ledger.Store) *Manager {
	t.Helper()
	manager := NewManager(testConfig(root), launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
	})
	t.Cleanup(manager.Close)
	return manager
}

func openStartLedger(t *testing.T, root string) *ledger.Store {
	t.Helper()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestFinishCreateFailurePreservesKnownSessionID(t *testing.T) {
	root := t.TempDir()
	manager := startReplayManager(t, root, prototest.NewLauncher(), openStartLedger(t, root))
	_, err := manager.finishCreate(context.Background(), "known-session", state.CreateSessionRequest{OperationID: testStartOperation})
	var partial *state.StartCreateFailedError
	if !errors.As(err, &partial) || partial.SessionID != "known-session" || partial.OperationID != testStartOperation {
		t.Fatalf("lost created session identity: %v", err)
	}
}

// A caller that lost the create response retries with the same operation id.
// It must get the session the first request made, not a second runtime.
func TestCreateWithSameOperationIDReturnsTheFirstSession(t *testing.T) {
	root := t.TempDir()
	store := openStartLedger(t, root)
	launcher := prototest.NewLauncher()
	manager := startReplayManager(t, root, launcher, store)
	request := state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root, OperationID: testStartOperation, PromptOperationID: testPromptOperation}

	first, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.Start == nil || first.Start.OperationID != testStartOperation || first.Start.PromptOperationID != testPromptOperation || first.Start.Replayed {
		t.Fatalf("first create start ids = %#v", first.Start)
	}
	again, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID || again.Start == nil || !again.Start.Replayed {
		t.Fatalf("replayed create = id %s start %#v, want %s replayed", again.ID, again.Start, first.ID)
	}
	if launches := len(launcher.Launches); launches != 1 {
		t.Fatalf("launches = %d, want exactly one runtime for one operation", launches)
	}
	events, err := store.Events(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, event := range events {
		if event.Type == ledger.EventCreated {
			created++
			if !strings.Contains(string(event.Payload), testPromptOperation) {
				t.Fatalf("created payload lacks the prompt operation id: %s", event.Payload)
			}
		}
	}
	if created != 1 {
		t.Fatalf("created events = %d, want 1", created)
	}
}

// The operation ids live in the ledger, so a daemon restart neither forgets
// them nor turns a retry into a second session.
func TestStartOperationSurvivesDaemonRestart(t *testing.T) {
	root := t.TempDir()
	store := openStartLedger(t, root)
	launcher := prototest.NewLauncher()
	before := startReplayManager(t, root, launcher, store)
	request := state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root, OperationID: testStartOperation, PromptOperationID: testPromptOperation}
	created, err := before.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	before.Close()

	after := startReplayManager(t, root, launcher, store)
	if err := after.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	var listed *state.SessionInfo
	for _, info := range after.List(true) {
		if info.ID == created.ID {
			listed = &info
		}
	}
	if listed == nil || listed.Start == nil || listed.Start.PromptOperationID != testPromptOperation {
		t.Fatalf("restarted listing = %#v, want the recorded start ids", listed)
	}
	// This scratch daemon cannot re-attach the in-memory runner, which is
	// exactly the window a real restart has while discovery is still running.
	// The retry must name the recorded session, never launch a second one.
	_, err = after.Create(context.Background(), request)
	var replay *state.StartCreateReplayError
	if !errors.As(err, &replay) || replay.SessionID != created.ID || !strings.Contains(err.Error(), "not attached") || len(launcher.Launches) != 1 {
		t.Fatalf("replay after restart = %v (launches %d), want a conflict naming %s without a new launch", err, len(launcher.Launches), created.ID)
	}
}

// An operation whose session no longer runs cannot be answered with a live
// session, and starting another would duplicate the work. The caller is told
// which session the id belongs to.
func TestCreateReplayOfEndedSessionNamesItInsteadOfStartingAnother(t *testing.T) {
	root := t.TempDir()
	store := openStartLedger(t, root)
	launcher := prototest.NewLauncher()
	manager := startReplayManager(t, root, launcher, store)
	request := state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root, OperationID: testStartOperation}
	created, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !manager.Kill(context.Background(), created.ID, false) {
		t.Fatal("kill was not accepted")
	}
	waitForExit(t, manager, created.ID)
	_, err = manager.Create(context.Background(), request)
	var replay *state.StartCreateReplayError
	if !errors.As(err, &replay) || replay.SessionID != created.ID {
		t.Fatalf("replay of ended session = %v, want StartCreateReplayError naming %s", err, created.ID)
	}
	if len(launcher.Launches) != 1 {
		t.Fatalf("launches = %d, want 1", len(launcher.Launches))
	}
}

func TestCreateRejectsMalformedAndLedgerlessOperationIDs(t *testing.T) {
	root := t.TempDir()
	store := openStartLedger(t, root)
	manager := startReplayManager(t, root, prototest.NewLauncher(), store)
	if _, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root, OperationID: "not-a-uuid"}); err == nil {
		t.Fatal("malformed operation id was accepted")
	}
	if _, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root, OperationID: testStartOperation, PromptOperationID: testStartOperation}); err == nil {
		t.Fatal("identical create and prompt operation ids were accepted")
	}
	ledgerless := NewManager(testConfig(t.TempDir()), prototest.NewLauncher(), ManagerOptions{DisableWatchers: true, ActivityInterval: time.Hour})
	t.Cleanup(ledgerless.Close)
	_, err := ledgerless.Create(context.Background(), state.CreateSessionRequest{Cmd: "/bin/sh", Cwd: root, OperationID: testStartOperation})
	if err == nil || !strings.Contains(err.Error(), "ledger") {
		t.Fatalf("ledgerless operation id = %v, want an instructional refusal", err)
	}
}

// waitForExit holds until the killed session has actually exited: Kill only
// accepts the request, and a replay before the exit rightly returns the
// still-live session.
func waitForExit(t *testing.T, manager *Manager, id string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if current, ok := manager.Get(id); !ok || current.Info().Exited {
			return
		}
	}
	t.Fatalf("session %s never exited", id)
}
