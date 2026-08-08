package session

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestGCClosedPreservesRetainedDescendantsAndArchivesAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(t.TempDir(), "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	parent := "00000000-0000-4000-8000-000000000001"
	child := "00000000-0000-4000-8000-000000000002"
	if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
		Meta: ledger.Meta{LaneID: parent, AtMS: 10}, LaneUUID: parent,
		Tool: string(state.ToolLane), Cwd: t.TempDir(),
		CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
		Meta: ledger.Meta{LaneID: child, AtMS: 20}, LaneUUID: child,
		Tool: string(state.ToolLane), Cwd: t.TempDir(),
		CreatorKind: ledger.CreatorSession, CreatorID: parent,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{
		Meta: ledger.Meta{LaneID: parent, AtMS: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{
		Meta: ledger.Meta{LaneID: child, AtMS: 200},
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(state.Config{
		StateRoot:      t.TempDir(),
		UserStateRoot:  t.TempDir(),
		RunnerStateDir: t.TempDir(),
	}, nil, ManagerOptions{
		ActivityInterval: time.Hour,
		Boundaries:       store.Boundaries(),
		Observations:     store.Observations(),
		Retention:        store.Retention(),
		LedgerReader:     store,
		Notify:           func(PushPayload) {},
	})
	defer manager.Close()

	preview, err := manager.GCClosed(ctx, 150, true)
	if err != nil {
		t.Fatal(err)
	}
	statuses := retentionStatuses(preview.Items)
	if statuses[parent] != "skipped:has a retained descendant" {
		t.Fatalf("parent preview = %q", statuses[parent])
	}
	if statuses[child] != "skipped:newer than retention cutoff" {
		t.Fatalf("child preview = %q", statuses[child])
	}

	applied, err := manager.GCClosed(ctx, 250, false)
	if err != nil {
		t.Fatal(err)
	}
	statuses = retentionStatuses(applied.Items)
	if statuses[parent] != "archived:" || statuses[child] != "archived:" {
		t.Fatalf("applied statuses = %#v", statuses)
	}
	for _, folded := range ledger.Fold(mustLedgerEvents(t, store)) {
		if (folded.LaneID == parent || folded.LaneID == child) && !folded.Archived {
			t.Fatalf("lane was not archived: %#v", folded)
		}
	}
	if listed := manager.List(true); len(listed) != 0 {
		t.Fatalf("archived records remained listed: %#v", listed)
	}
	if listed := manager.withDurableClosed(ctx, []state.SessionInfo{{
		ID: parent, Exited: true,
	}}, true); len(listed) != 0 {
		t.Fatalf("resident archived record remained listed: %#v", listed)
	}
	if _, _, err := manager.resolveCreator(ctx, state.CreateSessionRequest{
		CreatorSessionID: parent,
	}); err == nil {
		t.Fatal("archived parent remained eligible as a creator session")
	}
}

// Retention is conservative about live runners, and a live runner is a running
// process. A socket file is not one: sockets outlive the process that bound
// them, so a user-killed session with a leftover socket and no runner is
// archivable, while the same session with a running runner is not.
//
// This replaces an assertion that a leftover socket alone meant "runner is
// still live". That assertion described the defect: on the owner's machine
// every session with any leftover artifact refused archiving, which is the
// reported "archive from list doesn't work".
func TestGCClosedSkipsRunningRunnersAndArchivesStaleArtifacts(t *testing.T) {
	ctx := context.Background()
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(t.TempDir(), "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	staleSocket := "00000000-0000-4000-8000-000000000003"
	liveRunner := "00000000-0000-4000-8000-00000000000a"
	for _, id := range []string{staleSocket, liveRunner} {
		if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
			Meta: ledger.Meta{LaneID: id, AtMS: 10}, LaneUUID: id,
			Tool: string(state.ToolLane), Cwd: t.TempDir(),
			CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Boundaries().RecordUserKill(ctx, ledger.UserKill{
			Meta: ledger.Meta{LaneID: id, AtMS: 100},
		}); err != nil {
			t.Fatal(err)
		}
	}
	runnerDir := t.TempDir()
	launchAgentsDir := t.TempDir()
	// A socket and a launch agent with nothing behind them.
	if err := os.WriteFile(filepath.Join(runnerDir, staleSocket+".sock"), []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(state.RunnerPlistPath(launchAgentsDir, staleSocket), []byte("<plist/>"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A session whose runner really is running.
	livePaths := state.For(runnerDir, liveRunner)
	if err := state.WriteMetadata(livePaths.Meta, state.Metadata{
		ID: liveRunner, Cmd: "/bin/sh", Cwd: runnerDir, Cols: 300, Rows: 50,
		CreatedAt: 1, PID: 4242, SockPath: livePaths.Socket,
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(state.Config{
		StateRoot: runnerDir, UserStateRoot: t.TempDir(),
		RunnerStateDir: runnerDir, LaunchAgentsDir: launchAgentsDir,
	}, nil, ManagerOptions{
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		Retention: store.Retention(), LedgerReader: store,
		ActivityInterval: time.Hour, Notify: func(PushPayload) {},
		ProcessAlive:   func(pid int) bool { return pid == 4242 },
		ProcessCommand: func(int) string { return "/opt/sessions/sessions-runner" },
	})
	defer manager.Close()

	result, err := manager.GCClosed(ctx, 200, false)
	if err != nil {
		t.Fatal(err)
	}
	statuses := retentionStatuses(result.Items)
	if statuses[staleSocket] != "archived:" {
		t.Fatalf("closed session with only stale artifacts = %q, want archived", statuses[staleSocket])
	}
	if statuses[liveRunner] != "skipped:runner is still live" {
		t.Fatalf("closed record whose runner is running = %q, want skipped", statuses[liveRunner])
	}
}

// The reported bug, end to end: a session that is over, whose launch agent and
// socket were never cleaned up, refused `POST /api/retention/archive` with
// "runner is still live" -- a claim about a process nobody had asked about.
// The recorded pid is dead, so there is no runner, so the archive proceeds.
func TestArchiveClosedSucceedsWhenOnlyArtifactsRemain(t *testing.T) {
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

	id := "00000000-0000-4000-8000-00000000000b"
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

	paths := state.For(config.RunnerStateDir, id)
	if err := os.WriteFile(paths.Socket, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteMetadata(paths.Meta, state.Metadata{
		ID: id, Cmd: "/bin/sh", Cwd: root, Cols: 300, Rows: 50,
		CreatedAt: 1, PID: 4243, SockPath: paths.Socket,
	}); err != nil {
		t.Fatal(err)
	}
	// Both the current and the legacy launch agent, which is what the owner's
	// machine actually looked like.
	for _, plist := range []string{
		state.RunnerPlistPath(config.LaunchAgentsDir, id),
		state.LegacyRunnerPlistPath(config.LaunchAgentsDir, id),
	} {
		if err := os.WriteFile(plist, []byte("<plist/>"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	manager := NewManager(config, nil, ManagerOptions{
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		Retention: store.Retention(), LedgerReader: store,
		ActivityInterval: time.Hour, Notify: func(PushPayload) {},
		ProcessAlive:   func(int) bool { return false },
		ProcessCommand: func(int) string { return "" },
	})
	defer manager.Close()

	result, err := manager.ArchiveClosed(ctx, []string{id})
	if err != nil {
		t.Fatal(err)
	}
	if status := retentionStatuses(result.Items)[id]; status != "archived:" {
		t.Fatalf("archive of a closed session with stale artifacts = %q, want archived", status)
	}
}

// A session the daemon only lost contact with is still archivable when the
// user picks it, because Sessions can check the one thing that would justify
// refusing: whether the runner is running. It is not.
func TestArchiveClosedAcceptsUnreachableSessionWithNoRunner(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	config := testConfig(root)
	if err := os.MkdirAll(config.RunnerStateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(root, "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	unreachable := "00000000-0000-4000-8000-00000000000c"
	alive := "00000000-0000-4000-8000-00000000000d"
	for _, id := range []string{unreachable, alive} {
		if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
			Meta: ledger.Meta{LaneID: id, AtMS: 10}, LaneUUID: id,
			Tool: string(state.ToolLane), Cwd: root,
			CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
		}); err != nil {
			t.Fatal(err)
		}
		if err := store.Observations().RecordRunnerLost(ctx, ledger.Observation{
			Meta: ledger.Meta{LaneID: id, AtMS: 100},
		}); err != nil {
			t.Fatal(err)
		}
	}
	alivePaths := state.For(config.RunnerStateDir, alive)
	if err := state.WriteMetadata(alivePaths.Meta, state.Metadata{
		ID: alive, Cmd: "/bin/sh", Cwd: root, Cols: 300, Rows: 50,
		CreatedAt: 1, PID: 4242, SockPath: alivePaths.Socket,
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(config, nil, ManagerOptions{
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		Retention: store.Retention(), LedgerReader: store,
		ActivityInterval: time.Hour, Notify: func(PushPayload) {},
		ProcessAlive:   func(pid int) bool { return pid == 4242 },
		ProcessCommand: func(int) string { return "/opt/sessions/sessions-runner" },
	})
	defer manager.Close()

	result, err := manager.ArchiveClosed(ctx, []string{unreachable, alive})
	if err != nil {
		t.Fatal(err)
	}
	statuses := retentionStatuses(result.Items)
	if statuses[unreachable] != "archived:" {
		t.Fatalf("unreachable session with no runner = %q, want archived", statuses[unreachable])
	}
	if statuses[alive] != "skipped:runner is still live" {
		t.Fatalf("unreachable session whose runner is running = %q, want skipped", statuses[alive])
	}
}

func TestArchiveClosedArchivesOnlyExplicitClosedRecords(t *testing.T) {
	ctx := context.Background()
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(t.TempDir(), "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	closed := "00000000-0000-4000-8000-000000000004"
	running := "00000000-0000-4000-8000-000000000005"
	for index, id := range []string{closed, running} {
		if err := store.Boundaries().RecordCreated(ctx, ledger.Created{
			Meta:     ledger.Meta{LaneID: id, AtMS: int64(index + 1)},
			LaneUUID: id, Tool: string(state.ToolLane), Cwd: t.TempDir(),
			CreatorKind: ledger.CreatorUser, CreatorID: "uid:501",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{
		Meta: ledger.Meta{LaneID: closed, AtMS: 100},
	}); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(state.Config{
		StateRoot: t.TempDir(), UserStateRoot: t.TempDir(),
		RunnerStateDir: t.TempDir(), LaunchAgentsDir: t.TempDir(),
	}, nil, ManagerOptions{
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		Retention: store.Retention(), LedgerReader: store,
		ActivityInterval: time.Hour, Notify: func(PushPayload) {},
	})
	defer manager.Close()

	result, err := manager.ArchiveClosed(ctx, []string{running, closed, closed})
	if err != nil {
		t.Fatal(err)
	}
	statuses := retentionStatuses(result.Items)
	if statuses[closed] != "archived:" {
		t.Fatalf("closed status = %q", statuses[closed])
	}
	if statuses[running] != "skipped:session is still running" {
		t.Fatalf("running status = %q", statuses[running])
	}
	for _, folded := range ledger.Fold(mustLedgerEvents(t, store)) {
		if folded.LaneID == closed && !folded.Archived {
			t.Fatal("explicitly selected closed record was not archived")
		}
		if folded.LaneID == running && folded.Archived {
			t.Fatal("running record was archived")
		}
	}
	if _, err := manager.ArchiveClosed(ctx, []string{"", ""}); err == nil {
		t.Fatal("blank archive selection was accepted")
	}
}

func retentionStatuses(items []RetentionItem) map[string]string {
	result := make(map[string]string, len(items))
	for _, item := range items {
		result[item.ID] = item.Status + ":" + item.Reason
	}
	return result
}

func mustLedgerEvents(t *testing.T, store *ledger.Store) []ledger.Event {
	t.Helper()
	events, err := store.Events(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	return events
}
