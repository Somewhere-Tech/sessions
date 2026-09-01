package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestPendingRestoreAppearsAsUnavailableInsteadOfIdle(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	lastHuman := int64(250)
	paths := state.For(dir, id)
	if err := state.WriteMetadata(paths.Meta, state.Metadata{
		ID: id, Name: "PM", Cmd: "claude", Cwd: "/work", Cols: 100, Rows: 40,
		CreatedAt: 100, PID: 9988, LastHumanMessageAt: &lastHuman,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteRestorePending(paths.RestorePending, id, "paused by bounded boot recovery"); err != nil {
		t.Fatal(err)
	}

	manager := &Manager{config: state.Config{RunnerStateDir: dir}}
	listed := manager.withPendingRestores(nil)
	if len(listed) != 1 {
		t.Fatalf("pending list = %+v", listed)
	}
	got := listed[0]
	if got.ID != id || got.Name != "PM" || got.PID != 0 || got.Working || got.Exited ||
		!got.Unreachable || got.UnreachableReason != restartRestorePendingReason ||
		got.IdleReason != "needs-recovery" || got.LastDataAt != lastHuman {
		t.Fatalf("pending session = %+v", got)
	}
}

func TestUnreadablePendingMarkerStillBlocksSilentReads(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	path := state.For(dir, id).RestorePending
	if err := os.WriteFile(path, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{config: state.Config{RunnerStateDir: dir}}
	pending, ok := manager.PendingRestore(id)
	if !ok || pending.SessionID != id || pending.Reason == "" {
		t.Fatalf("PendingRestore() = %+v, %v", pending, ok)
	}
	if _, ok := manager.PendingRestore(filepath.Join("..", id)); ok {
		t.Fatal("PendingRestore accepted a path instead of an id")
	}
	listed := manager.withPendingRestores(nil)
	if len(listed) != 1 || listed[0].ID != id || !listed[0].Unreachable ||
		listed[0].UnreachableReason != restartRestorePendingReason {
		t.Fatalf("unreadable recovery evidence disappeared from the list: %+v", listed)
	}
}

func TestPendingRestoreOverridesOlderLostRecord(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-555555555555"
	paths := state.For(dir, id)
	if err := state.WriteMetadata(paths.Meta, state.Metadata{
		ID: id, Name: "PM", Cmd: "claude", Cwd: "/work", Cols: 100, Rows: 40,
		CreatedAt: 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := state.WriteRestorePending(paths.RestorePending, id, "paused by bounded boot recovery"); err != nil {
		t.Fatal(err)
	}
	older := []state.SessionInfo{{
		ID: id, Name: "PM", PID: 0, Unreachable: true, UnreachableReason: "runner-lost",
	}}
	manager := &Manager{config: state.Config{RunnerStateDir: dir}}
	listed := manager.withPendingRestores(older)
	if len(listed) != 1 || listed[0].UnreachableReason != restartRestorePendingReason ||
		listed[0].IdleReason != "needs-recovery" {
		t.Fatalf("pending restore did not replace stale lost record: %+v", listed)
	}
}
