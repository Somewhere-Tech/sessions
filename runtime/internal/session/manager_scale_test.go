package session

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

const scaleLostSessions = 180

func scaleFixture(t testing.TB) (*Manager, func() int) {
	t.Helper()
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 450; index++ {
		writeScaleRecord(t, store, config, index)
	}
	snapshots := 0
	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(), LedgerReader: store,
		ProcessAlive: func(int) bool { return true },
		ProcessCommand: func(int) string {
			time.Sleep(10 * time.Millisecond)
			return "/bin/unrelated"
		},
		ProcessSnapshot: func(context.Context) (map[int]string, error) {
			snapshots++
			return map[int]string{4242: "/bin/unrelated"}, nil
		},
		Notify: func(PushPayload) {},
	})
	return manager, func() int {
		manager.Close()
		_ = store.Close()
		return snapshots
	}
}

func writeScaleRecord(t testing.TB, store *ledger.Store, config state.Config, index int) {
	t.Helper()
	id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
	ctx := context.Background()
	created := ledger.Created{
		Meta: ledger.Meta{LaneID: id}, LaneUUID: id, Tool: "lane", Kind: "lane", Cwd: config.DefaultCwd,
		CreatorKind: ledger.CreatorExternal, CreatorID: "scale-test",
	}
	if err := store.Boundaries().RecordCreated(ctx, created); err != nil {
		t.Fatal(err)
	}
	if index >= scaleLostSessions {
		if err := store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{Meta: ledger.Meta{LaneID: id}}); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err := store.Observations().RecordRunnerLost(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: id}}); err != nil {
		t.Fatal(err)
	}
	paths := state.For(config.RunnerStateDir, id)
	if err := state.WriteMetadata(paths.Meta, state.Metadata{
		ID: id, Kind: "lane", Cmd: "/bin/session-command", Cwd: config.DefaultCwd,
		PID: 4242, SockPath: paths.Socket,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLargeLostSessionListStaysWithinBudget(t *testing.T) {
	manager, closeFixture := scaleFixture(t)
	started := time.Now()
	listed := manager.List(true)
	elapsed := time.Since(started)
	snapshots := closeFixture()
	if len(listed) != 450 || snapshots != 1 {
		t.Fatalf("large list = %d sessions, %d snapshots", len(listed), snapshots)
	}
	if elapsed > 750*time.Millisecond {
		t.Fatalf("large lost-session list took %s, budget 750ms", elapsed)
	}
}

func BenchmarkLargeLostSessionListBudget(b *testing.B) {
	manager, closeFixture := scaleFixture(b)
	b.ResetTimer()
	started := time.Now()
	for index := 0; index < b.N; index++ {
		if got := len(manager.List(true)); got != 450 {
			b.Fatalf("listed %d sessions", got)
		}
	}
	average := time.Since(started) / time.Duration(b.N)
	b.StopTimer()
	_ = closeFixture()
	if average > 250*time.Millisecond {
		b.Fatalf("large lost-session list averaged %s, budget 250ms/op", average)
	}
}

func TestScaleFixtureCreatesNoLaunchAgents(t *testing.T) {
	manager, closeFixture := scaleFixture(t)
	entries, err := os.ReadDir(manager.config.LaunchAgentsDir)
	_ = closeFixture()
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("scale fixture unexpectedly created %d launch agents", len(entries))
	}
}
