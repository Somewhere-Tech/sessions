package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// A repeated operation id is answered from the ledger before any account,
// settings or provider work. That work belongs to a new launch; the recorded
// launch is already decided and must not depend on it, or be re-decided by it.

// fakeCatalog stands in for the live Codex model catalog and counts every
// request, so a test can prove a replay never asked.
type fakeCatalog struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (c *fakeCatalog) list(context.Context, string) ([]codexapp.Model, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.err != nil {
		return nil, c.err
	}
	return testCodexCatalog(), nil
}

func (c *fakeCatalog) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func (c *fakeCatalog) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

func replayManager(t *testing.T, root string, config state.Config, launcher *prototest.Launcher, store *ledger.Store, reader LedgerReader, catalog func(context.Context, string) ([]codexapp.Model, error)) *Manager {
	t.Helper()
	if reader == nil {
		reader = store
	}
	manager := NewManager(config, launcher, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour, Notify: func(PushPayload) {},
		Boundaries: store.Boundaries(), Observations: store.Observations(), Worktrees: store.Worktrees(),
		LedgerReader: reader, ListCodexModels: catalog,
	})
	t.Cleanup(manager.Close)
	return manager
}

func richCodexRequest(root string) state.CreateSessionRequest {
	return state.CreateSessionRequest{
		Cmd: "codex", Cwd: root, Kind: state.KindCodexAppServer, Args: []string{"--model", "alpha"},
		OperationID: testStartOperation, PromptOperationID: testPromptOperation,
	}
}

func TestReplayDoesNotReloadTheCodexModelCatalog(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	store := openStartLedger(t, root)
	launcher := prototest.NewLauncher()
	catalog := &fakeCatalog{}
	manager := replayManager(t, root, testConfig(root), launcher, store, nil, catalog.list)

	first, err := manager.Create(context.Background(), richCodexRequest(root))
	if err != nil {
		t.Fatal(err)
	}
	if catalog.count() != 1 {
		t.Fatalf("first create asked the catalog %d times, want 1", catalog.count())
	}
	// The provider is gone or no longer lists the model. The recorded
	// operation still answers, without asking it.
	catalog.fail(errors.New("catalog unavailable"))
	again, err := manager.Create(context.Background(), richCodexRequest(root))
	if err != nil {
		t.Fatalf("replay refused because of the catalog: %v", err)
	}
	if again.ID != first.ID || again.Start == nil || !again.Start.Replayed {
		t.Fatalf("replay = %s %#v, want %s replayed", again.ID, again.Start, first.ID)
	}
	if catalog.count() != 1 || len(launcher.Launches) != 1 {
		t.Fatalf("replay asked the catalog (%d calls) or launched (%d)", catalog.count(), len(launcher.Launches))
	}
}

func TestReplaySurvivesChangedClaudeSettings(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	config := testConfig(root)
	config.SettingsPath = filepath.Join(root, "config", "settings.json")
	store := openStartLedger(t, root)
	launcher := prototest.NewLauncher()
	manager := replayManager(t, root, config, launcher, store, nil, nil)
	request := state.CreateSessionRequest{Cmd: "claude", Cwd: root, OperationID: testStartOperation}

	first, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	// Settings that would now refuse a new Claude launch outright.
	if err := os.MkdirAll(filepath.Dir(config.SettingsPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.SettingsPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "claude", Cwd: root}); err == nil || !strings.Contains(err.Error(), "settings") {
		t.Fatalf("a new launch under corrupt settings = %v, want the settings refusal", err)
	}
	launches := len(launcher.Launches)
	again, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("replay refused because of changed settings: %v", err)
	}
	if again.ID != first.ID || again.Start == nil || !again.Start.Replayed || len(launcher.Launches) != launches {
		t.Fatalf("replay = %s %#v (launches %d->%d), want %s replayed", again.ID, again.Start, launches, len(launcher.Launches), first.ID)
	}
}

// parentHistoryReader is the real ledger, except that one lane's full event
// history can be made unreadable while the projection keeps answering.
type parentHistoryReader struct {
	*ledger.Store
	mu     sync.Mutex
	broken string
}

func (r *parentHistoryReader) Events(ctx context.Context, laneID string) ([]ledger.Event, error) {
	r.mu.Lock()
	broken := r.broken
	r.mu.Unlock()
	if broken != "" && laneID == broken {
		return nil, errors.New("parent history unavailable")
	}
	return r.Store.Events(ctx, laneID)
}

func (r *parentHistoryReader) breakLane(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.broken = id
}

func TestReplayOfInheritedChildIgnoresParentHistoryFailure(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	store := openStartLedger(t, root)
	reader := &parentHistoryReader{Store: store}
	launcher := prototest.NewLauncher()
	manager := replayManager(t, root, testConfig(root), launcher, store, reader, nil)

	parent, err := manager.Create(context.Background(), state.CreateSessionRequest{Cmd: "codex", Cwd: root, Profile: "work"})
	if err != nil {
		t.Fatal(err)
	}
	request := state.CreateSessionRequest{Cmd: "codex", Cwd: root, CreatorSessionID: parent.ID, OperationID: testStartOperation}
	child, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if child.Profile != "work" {
		t.Fatalf("child account = %q, want the manager's", child.Profile)
	}
	home := filepath.Join(manager.config.UserStateRoot, "profiles", "codex", "work")
	old := time.Unix(1_000_000, 0)
	if err := os.Chtimes(home, old, old); err != nil {
		t.Fatal(err)
	}
	reader.breakLane(parent.ID)

	// New work cannot settle its account, and says so.
	fresh := request
	fresh.OperationID = "5a000000-0000-4000-8000-000000000003"
	if _, err := manager.Create(context.Background(), fresh); err == nil || !strings.Contains(err.Error(), "delegating session's account") {
		t.Fatalf("new child with unreadable parent history = %v, want the account refusal", err)
	}
	launches := len(launcher.Launches)
	// The recorded child is still the recorded child.
	again, err := manager.Create(context.Background(), request)
	if err != nil {
		t.Fatalf("replay refused because of parent history: %v", err)
	}
	if again.ID != child.ID || again.Start == nil || !again.Start.Replayed || again.Profile != "work" || len(launcher.Launches) != launches {
		t.Fatalf("replay = %s %q %#v (launches %d->%d), want %s on work, replayed", again.ID, again.Profile, again.Start, launches, len(launcher.Launches), child.ID)
	}
	if info, err := os.Stat(home); err != nil || !info.ModTime().Equal(old) {
		t.Fatalf("replay touched the account home: %v %v", info, err)
	}
}

func TestMalformedCreateFieldsAreRefusedBeforeProviderWork(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	store := openStartLedger(t, root)
	launcher := prototest.NewLauncher()
	catalog := &fakeCatalog{}
	manager := replayManager(t, root, testConfig(root), launcher, store, nil, catalog.list)

	for name, change := range map[string]func(*state.CreateSessionRequest){
		"operation id":               func(r *state.CreateSessionRequest) { r.OperationID = "not-a-uuid" },
		"prompt operation id alone":  func(r *state.CreateSessionRequest) { r.OperationID, r.PromptOperationID = "", "NOT-A-UUID" },
		"identical operation ids":    func(r *state.CreateSessionRequest) { r.PromptOperationID = r.OperationID },
		"named and default accounts": func(r *state.CreateSessionRequest) { r.Profile, r.DefaultProfile = "work", true },
		"profile spelling":           func(r *state.CreateSessionRequest) { r.Profile = "Work_Home" },
	} {
		request := richCodexRequest(root)
		change(&request)
		if _, err := manager.Create(context.Background(), request); err == nil {
			t.Fatalf("%s: malformed request was accepted", name)
		}
	}
	if catalog.count() != 0 || len(launcher.Launches) != 0 {
		t.Fatalf("malformed requests asked the catalog %d times and launched %d", catalog.count(), len(launcher.Launches))
	}
	if _, err := os.Stat(filepath.Join(testConfig(root).UserStateRoot, "profiles")); !os.IsNotExist(err) {
		t.Fatalf("a malformed request created a profile home: %v", err)
	}
}

// Two first attempts with one operation id both miss the early lookup and
// prepare in parallel; the lookup under the creation lock still lets only one
// launch, and the other is answered with the session that launch recorded.
func TestConcurrentFirstAttemptsWithOneOperationLaunchOnce(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	store := openStartLedger(t, root)
	launcher := prototest.NewLauncher()
	var arrivedMu sync.Mutex
	arrived := 0
	bothPreparing := make(chan struct{})
	catalog := func(ctx context.Context, _ string) ([]codexapp.Model, error) {
		arrivedMu.Lock()
		arrived++
		if arrived == 2 {
			close(bothPreparing)
		}
		arrivedMu.Unlock()
		select {
		case <-bothPreparing:
			return testCodexCatalog(), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	manager := replayManager(t, root, testConfig(root), launcher, store, nil, catalog)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	type answer struct {
		info state.SessionInfo
		err  error
	}
	answers := make(chan answer, 2)
	for _, name := range []string{"first", "second"} {
		request := richCodexRequest(root)
		request.Name = name
		go func() {
			info, err := manager.Create(ctx, request)
			answers <- answer{info, err}
		}()
	}
	results := []answer{<-answers, <-answers}
	if len(launcher.Launches) != 1 {
		t.Fatalf("launches = %d, want one runtime for one operation", len(launcher.Launches))
	}
	launched := launcher.Launches[0]
	recordedName := launched.Env["RUNNER_NAME"]
	for _, result := range results {
		var replay *state.StartCreateReplayError
		switch {
		case result.err == nil:
			if result.info.ID != launched.Info.ID || result.info.Name != recordedName {
				t.Fatalf("answer %s %q, want the recorded %s %q", result.info.ID, result.info.Name, launched.Info.ID, recordedName)
			}
		case errors.As(result.err, &replay):
			if replay.SessionID != launched.Info.ID {
				t.Fatalf("replay receipt names %s, want %s", replay.SessionID, launched.Info.ID)
			}
		default:
			t.Fatalf("concurrent attempt failed: %v", result.err)
		}
	}
	if results[0].err != nil && results[1].err != nil {
		t.Fatalf("neither attempt returned the launched session: %v / %v", results[0].err, results[1].err)
	}
}
