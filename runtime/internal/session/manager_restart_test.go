package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type launcherWithoutWake struct{}

func (launcherWithoutWake) ProgramArguments(proto.LaunchRequest) []string { return nil }
func (launcherWithoutWake) Launch(context.Context, proto.LaunchRequest) (proto.Runner, error) {
	return nil, nil
}
func (launcherWithoutWake) Attach(context.Context, proto.RunnerInfo) (proto.Runner, error) {
	return nil, nil
}

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
	manager.refreshPendingRestores()
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
	manager.refreshPendingRestores()
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
	manager.refreshPendingRestores()
	listed := manager.withPendingRestores(older)
	if len(listed) != 1 || listed[0].UnreachableReason != restartRestorePendingReason ||
		listed[0].IdleReason != "needs-recovery" {
		t.Fatalf("pending restore did not replace stale lost record: %+v", listed)
	}
}

func TestPausedSessionWithoutPlatformWakerNamesTheExactResumeCommand(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-777777777777"
	if err := state.WriteRestorePending(state.For(dir, id).RestorePending, id, "paused after restart"); err != nil {
		t.Fatal(err)
	}
	launcher := launcherWithoutWake{}
	manager := &Manager{
		config: state.Config{RunnerStateDir: dir}, launcher: launcher,
		registry: state.NewRegistry(state.Config{RunnerStateDir: dir}, launcher),
	}
	_, err := manager.WakePaused(context.Background(), id)
	want := "this machine cannot restart a paused session in place; resume it with `sessions resume " + id + "`"
	if err == nil || err.Error() != want {
		t.Fatalf("WakePaused() error = %q, want %q", err, want)
	}
}

type createdOnlyLedger struct {
	created ledger.Created
	at      int64
	calls   *atomic.Int64
}

func (l createdOnlyLedger) Events(_ context.Context, laneID string) ([]ledger.Event, error) {
	if l.calls != nil {
		l.calls.Add(1)
	}
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

type failingLedger struct{ err error }

func (l failingLedger) Events(context.Context, string) ([]ledger.Event, error) { return nil, l.err }
func (failingLedger) LiveBindingFor(context.Context, string) (*ledger.LiveBinding, error) {
	return nil, nil
}
func (failingLedger) MovedBinding(context.Context, string) (*ledger.MovedConversation, error) {
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
	manager.refreshPendingRestores()
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

func TestPausedIdentityCacheInvalidatesWithoutReadingLedgerFromList(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-666666666667"
	marker := state.For(dir, id).RestorePending
	if err := state.WriteRestorePending(marker, id, "paused after restart"); err != nil {
		t.Fatal(err)
	}
	calls := &atomic.Int64{}
	manager := &Manager{
		config: state.Config{RunnerStateDir: dir},
		ledgerReader: createdOnlyLedger{
			created: ledger.Created{Meta: ledger.Meta{LaneID: id}, Name: "cached identity", Tool: "codex"},
			calls:   calls,
		},
	}
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	manager.refreshPendingRestores()
	for range 100 {
		listed := manager.withPendingRestores(nil)
		if len(listed) != 1 || listed[0].Name != "cached identity" {
			t.Fatalf("cached paused identity = %+v", listed)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("ledger reads after repeated lists = %d, want 1 warm-up read", got)
	}
	if err := state.WriteRestorePending(marker, id, "paused marker changed after restart"); err != nil {
		t.Fatal(err)
	}
	manager.refreshPendingRestores()
	manager.refreshPendingRestores()
	if got := calls.Load(); got != 2 {
		t.Fatalf("ledger reads after marker invalidation = %d, want exactly 2", got)
	}
	if got := strings.Count(logs.String(), "read paused session "+id+" metadata"); got != 1 {
		t.Fatalf("missing metadata log count = %d, want 1; logs=%s", got, logs.String())
	}
}

func TestOrphanPendingRestoreIsRetiredAndCounted(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-666666666668"
	marker := state.For(dir, id).RestorePending
	if err := state.WriteRestorePending(marker, id, "paused after restart"); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		config:       state.Config{RunnerStateDir: dir},
		ledgerReader: createdOnlyLedger{created: ledger.Created{Meta: ledger.Meta{LaneID: "someone-else"}}},
	}
	manager.refreshPendingRestores()
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("active orphan marker still exists: %v", err)
	}
	retired, err := os.ReadDir(filepath.Join(dir, "retired"))
	if err != nil || len(retired) != 1 {
		t.Fatalf("retired markers = %v, %v", retired, err)
	}
	if manager.RestorePendingCount() != 0 || manager.RetiredRestoreCount() != 1 {
		t.Fatalf("restore counts = pending %d retired %d, want 0 and 1",
			manager.RestorePendingCount(), manager.RetiredRestoreCount())
	}
}

func TestPendingRestoreSurvivesAnUnavailableLedger(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-666666666670"
	marker := state.For(dir, id).RestorePending
	if err := state.WriteRestorePending(marker, id, "paused after restart"); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		config: state.Config{RunnerStateDir: dir}, ledgerReader: failingLedger{err: errors.New("ledger busy")},
	}
	manager.refreshPendingRestores()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("ledger failure retired recovery evidence: %v", err)
	}
	if manager.RestorePendingCount() != 1 || manager.RetiredRestoreCount() != 0 {
		t.Fatalf("restore counts = pending %d retired %d, want 1 and 0",
			manager.RestorePendingCount(), manager.RetiredRestoreCount())
	}
}

func TestPausedCacheWatcherRefreshesChangedMetadata(t *testing.T) {
	dir := t.TempDir()
	id := "11111111-2222-4333-8444-666666666669"
	paths := state.For(dir, id)
	if err := state.WriteRestorePending(paths.RestorePending, id, "paused after restart"); err != nil {
		t.Fatal(err)
	}
	manager := NewManager(state.Config{RunnerStateDir: dir}, nil, ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		LedgerReader: createdOnlyLedger{created: ledger.Created{Meta: ledger.Meta{LaneID: id}, Name: "from ledger"}},
	})
	t.Cleanup(manager.Close)
	if err := state.WriteMetadata(paths.Meta, state.Metadata{ID: id, Name: "from metadata", Cmd: "claude"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		listed := manager.withPendingRestores(nil)
		if len(listed) == 1 && listed[0].Name == "from metadata" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("paused cache did not observe metadata change: %+v", listed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
