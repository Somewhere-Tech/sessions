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
	}}); len(listed) != 0 {
		t.Fatalf("resident archived record remained listed: %#v", listed)
	}
	if _, _, err := manager.resolveCreator(ctx, state.CreateSessionRequest{
		CreatorSessionID: parent,
	}); err == nil {
		t.Fatal("archived parent remained eligible as a creator session")
	}
}

func TestGCClosedConservativelySkipsRunnerArtifacts(t *testing.T) {
	ctx := context.Background()
	store, err := ledger.Open(ctx, ledger.Options{Path: filepath.Join(t.TempDir(), "ledger.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	id := "00000000-0000-4000-8000-000000000003"
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
	runnerDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(runnerDir, id+".sock"), []byte("stale-or-live"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(state.Config{
		StateRoot: runnerDir, UserStateRoot: t.TempDir(),
		RunnerStateDir: runnerDir, LaunchAgentsDir: t.TempDir(),
	}, nil, ManagerOptions{
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		Retention: store.Retention(), LedgerReader: store,
		ActivityInterval: time.Hour, Notify: func(PushPayload) {},
	})
	defer manager.Close()

	result, err := manager.GCClosed(ctx, 200, false)
	if err != nil {
		t.Fatal(err)
	}
	statuses := retentionStatuses(result.Items)
	if statuses[id] != "skipped:runner is still live" {
		t.Fatalf("artifact-backed tombstone status = %q", statuses[id])
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
