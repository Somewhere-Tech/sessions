package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// The startup sweep is the only thing in Sessions that unlinks a launch agent
// without a session asking it to, so what it declines to touch matters more
// than what it removes. Every id below is a different reason to decline.
func TestSweepRemovesOnlyDeadAndClosedArtifacts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config.LaunchAgentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const (
		endedDeadPID  = "00000000-0000-4000-8000-0000000000a1"
		endedLivePID  = "00000000-0000-4000-8000-0000000000a2"
		lostOnly      = "00000000-0000-4000-8000-0000000000a3"
		stillRunning  = "00000000-0000-4000-8000-0000000000a4"
		endedNoRecord = "00000000-0000-4000-8000-0000000000a5"
		endedStarting = "00000000-0000-4000-8000-0000000000a6"
	)
	// The live pid belongs to endedLivePID alone; every other session's
	// metadata records a pid that is not running.
	const livePID = 4242
	const deadPID = 4243

	for _, id := range []string{endedDeadPID, endedLivePID, lostOnly, stillRunning, endedStarting} {
		if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
			Meta: ledger.Meta{LaneID: id, AtMS: 10}, LaneUUID: id,
			Tool: string(state.ToolLane), Cwd: root,
			CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{endedDeadPID, endedLivePID, endedStarting} {
		if err := store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{
			Meta: ledger.Meta{LaneID: id, AtMS: 100},
		}); err != nil {
			t.Fatal(err)
		}
	}
	// A lost socket is not an ending, so this session's artifacts are not the
	// sweep's business however old they are.
	if err := store.Observations().RecordRunnerLost(ctx, ledger.Observation{
		Meta: ledger.Meta{LaneID: lostOnly, AtMS: 100},
	}); err != nil {
		t.Fatal(err)
	}

	pids := map[string]int{
		endedDeadPID: deadPID, endedLivePID: livePID, lostOnly: deadPID,
		stillRunning: deadPID, endedStarting: deadPID,
	}
	for id, pid := range pids {
		writeSweepArtifacts(t, config, id, pid)
	}
	// endedNoRecord has artifacts but no ledger record at all: unknown, so
	// untouched.
	writeSweepArtifacts(t, config, endedNoRecord, deadPID)
	// Age every artifact past the launch grace except the one session that is
	// deliberately mid-launch.
	for id := range pids {
		if id != endedStarting {
			ageSweepArtifacts(t, config, id)
		}
	}
	ageSweepArtifacts(t, config, endedNoRecord)

	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		Retention: store.Retention(), LedgerReader: store,
		Notify:         func(PushPayload) {},
		ProcessAlive:   func(pid int) bool { return pid == livePID },
		ProcessCommand: func(int) string { return "/opt/sessions/sessions-runner" },
	})
	t.Cleanup(manager.Close)

	removed := manager.SweepStaleRunnerArtifacts(ctx)

	sweptIDs := map[string]struct{}{}
	for _, artifact := range removed {
		sweptIDs[artifact.SessionID] = struct{}{}
	}
	if _, swept := sweptIDs[endedDeadPID]; !swept {
		t.Fatalf("sweep left a closed session's dead artifacts in place: %#v", removed)
	}
	delete(sweptIDs, endedDeadPID)
	if len(sweptIDs) != 0 {
		t.Fatalf("sweep touched sessions it had no business touching: %#v", sweptIDs)
	}

	assertSweepArtifactsGone(t, config, endedDeadPID)
	for _, id := range []string{endedLivePID, lostOnly, stillRunning, endedNoRecord, endedStarting} {
		assertSweepArtifactsPresent(t, config, id)
	}
	// Durable evidence is never runner coordination state.
	if _, err := os.Stat(filepath.Join(config.RunnerStateDir, endedDeadPID+".json")); err != nil {
		t.Fatalf("sweep removed runner metadata for %s: %v", endedDeadPID, err)
	}
}

// A closed session whose recorded pid is alive but running something else
// entirely is PID reuse, not a live runner, and its artifacts are stale.
func TestSweepTreatsRecycledPIDAsDead(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(config.LaunchAgentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const id = "00000000-0000-4000-8000-0000000000b1"
	if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
		Meta: ledger.Meta{LaneID: id, AtMS: 10}, LaneUUID: id,
		Tool: string(state.ToolLane), Cwd: root,
		CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{
		Meta: ledger.Meta{LaneID: id, AtMS: 100},
	}); err != nil {
		t.Fatal(err)
	}
	writeSweepArtifacts(t, config, id, 5150)
	ageSweepArtifacts(t, config, id)

	manager := NewManager(config, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		Retention: store.Retention(), LedgerReader: store,
		Notify:       func(PushPayload) {},
		ProcessAlive: func(int) bool { return true },
		// A live process at that pid, but not this session's runner.
		ProcessCommand: func(int) string { return "/Applications/Xcode.app/Contents/MacOS/Xcode" },
	})
	t.Cleanup(manager.Close)

	if removed := manager.SweepStaleRunnerArtifacts(ctx); len(removed) == 0 {
		t.Fatal("a recycled pid kept an ended session's artifacts alive")
	}
	assertSweepArtifactsGone(t, config, id)
}

func writeSweepArtifacts(t *testing.T, config state.Config, id string, pid int) {
	t.Helper()
	paths := state.For(config.RunnerStateDir, id)
	if err := os.WriteFile(paths.Socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteMetadata(paths.Meta, state.Metadata{
		ID: id, Cmd: "/bin/sh", Cwd: config.DefaultCwd, Cols: 300, Rows: 50,
		CreatedAt: 1, PID: pid, SockPath: paths.Socket,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.RunnerPlistPath(config.LaunchAgentsDir, id), []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func ageSweepArtifacts(t *testing.T, config state.Config, id string) {
	t.Helper()
	old := time.Now().Add(-10 * orphanStartingGrace)
	paths := state.For(config.RunnerStateDir, id)
	for _, path := range []string{
		paths.Socket, paths.Meta, state.RunnerPlistPath(config.LaunchAgentsDir, id),
	} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
}

func assertSweepArtifactsGone(t *testing.T, config state.Config, id string) {
	t.Helper()
	for _, path := range []string{
		state.RunnerPlistPath(config.LaunchAgentsDir, id),
		state.For(config.RunnerStateDir, id).Socket,
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("sweep left stale artifact %s: %v", path, err)
		}
	}
}

func assertSweepArtifactsPresent(t *testing.T, config state.Config, id string) {
	t.Helper()
	for _, path := range []string{
		state.RunnerPlistPath(config.LaunchAgentsDir, id),
		state.For(config.RunnerStateDir, id).Socket,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("sweep removed %s, which it had no authority to remove: %v", path, err)
		}
	}
}
