package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestKillClosesDurableLostLaneWithoutPretendingToSignalRunner(t *testing.T) {
	root := t.TempDir()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger", "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	manager := NewManager(testConfig(root), prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		ProcessAlive: func(int) bool { return false }, ProcessCommand: func(int) string { return "" },
	})
	t.Cleanup(manager.Close)

	const id = "43000000-0000-4000-8000-000000000001"
	if err := store.Boundaries().RecordCreated(context.Background(), ledger.Created{
		Meta: ledger.Meta{LaneID: id, AtMS: 100}, LaneUUID: id, Name: "sleeper",
		Tool: string(state.ToolLane), Cwd: root, CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Observations().RecordRunnerLost(context.Background(), ledger.Observation{
		Meta: ledger.Meta{LaneID: id, AtMS: 200},
	}); err != nil {
		t.Fatal(err)
	}
	active := manager.List(false)
	if len(active) != 1 || !active[0].RunnerGone || active[0].Exited {
		t.Fatalf("lost runner reality = %#v", active)
	}

	if err := manager.RequestKill(context.Background(), id, false); err != nil {
		t.Fatalf("close lost lane: %v", err)
	}
	states := ledger.Fold(lostLaneEvents(t, store, id))
	if len(states) != 1 || !states[0].UserKillRequested {
		t.Fatalf("lost lane was not durably closed: %#v", states)
	}
	if active = manager.List(false); len(active) != 0 {
		t.Fatalf("closed lost lane stayed active: %#v", active)
	}
	closed := manager.List(true)
	if len(closed) != 1 || !closed[0].Exited || closed[0].ExitReason != "ended-by-user" {
		t.Fatalf("closed lost lane listing = %#v", closed)
	}
}

func lostLaneEvents(t *testing.T, store *ledger.Store, id string) []ledger.Event {
	t.Helper()
	events, err := store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return events
}
