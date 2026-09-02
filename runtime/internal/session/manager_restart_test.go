package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
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

type createdOnlyLedger struct {
	created ledger.Created
	at      int64
}

func (l createdOnlyLedger) Events(_ context.Context, laneID string) ([]ledger.Event, error) {
	if laneID != l.created.LaneID {
		return nil, nil
	}
	payload, err := json.Marshal(l.created)
	if err != nil {
		return nil, err
	}
	return []ledger.Event{{LaneID: laneID, Type: ledger.EventCreated, AtMS: l.at, Payload: payload}}, nil
}

func (createdOnlyLedger) LiveBindingFor(context.Context, string) (*ledger.LiveBinding, error) {
	return nil, nil
}

func (createdOnlyLedger) MovedBinding(context.Context, string) (*ledger.MovedConversation, error) {
	return nil, nil
}

// A paused runner that left no metadata behind is still named and placed
// from its creation record, so the inbox never shows a blank row for it.
func TestPausedSessionWithoutMetadataIsDescribedFromTheLedger(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-666666666666"
	if err := state.WriteRestorePending(state.For(dir, id).RestorePending, id, "paused after restart"); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		config: state.Config{RunnerStateDir: dir},
		ledgerReader: createdOnlyLedger{
			created: ledger.Created{Meta: ledger.Meta{LaneID: id}, Name: "night shift", Tool: "claude", Cwd: "/work/app", Kind: "claude-structured"},
			at:      1_700_000_000_000,
		},
	}
	listed := manager.withPendingRestores(nil)
	if len(listed) != 1 {
		t.Fatalf("pending list = %+v", listed)
	}
	got := listed[0]
	if got.Name != "night shift" || got.Cwd != "/work/app" || got.CreatedAt != 1_700_000_000_000 || got.Tool != state.ToolClaude ||
		!got.Unreachable || got.UnreachableReason != restartRestorePendingReason {
		t.Fatalf("paused session from ledger = %+v", got)
	}
}
