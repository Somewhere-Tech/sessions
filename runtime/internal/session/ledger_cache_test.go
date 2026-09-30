package session

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Measured on the installed Mini with 593 sessions: a listing spent 660 ms
// folding the ledger and another 746 ms deciding which lanes were archived —
// the same fold again, for one boolean per lane. Both answers are pure
// functions of an append-only log, so they are worth exactly as long as the
// log's own sequence has not moved.
func TestRepeatedListingsDoNotRefoldAnUnchangedLedger(t *testing.T) {
	manager, store := scaleManager(t, 600)
	defer manager.Close()
	ctx := context.Background()

	coldStart := time.Now()
	first, cached, err := manager.ledgerStatesCached(ctx)
	cold := time.Since(coldStart)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("the first fold of a 600-lane ledger took %s", cold)
	if cached {
		t.Fatal("the first read claimed to come from a cache that had never been filled")
	}
	if len(first) != 600 {
		t.Fatalf("folded %d lanes, want 600", len(first))
	}

	started := time.Now()
	for range 20 {
		states, hit, readErr := manager.ledgerStatesCached(ctx)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if !hit {
			t.Fatal("a listing re-folded a ledger that had not moved")
		}
		if len(states) != 600 {
			t.Fatalf("cached fold has %d lanes, want 600", len(states))
		}
	}
	average := time.Since(started) / 20
	t.Logf("20 repeated folds of a 600-lane ledger averaged %s each", average)
	// The stage the Mini paid 660 ms for, twenty times over, has to be too
	// cheap to see. A single indexed row read is microseconds; this leaves
	// three orders of magnitude of headroom for a loaded machine.
	if average > 2*time.Millisecond {
		t.Fatalf("a cached fold averaged %s, which is not free enough to fix what this exists to fix", average)
	}

	// One event, and the answer is recomputed — exactly, not on a timer.
	id := "00000000-0000-4000-8000-000000000042"
	if err := store.Observations().RecordIdle(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: id}}); err != nil {
		t.Fatal(err)
	}
	if _, hit, readErr := manager.ledgerStatesCached(ctx); readErr != nil || hit {
		t.Fatalf("the cache survived an appended event (hit=%v err=%v)", hit, readErr)
	}
	if _, hit, _ := manager.ledgerStatesCached(ctx); !hit {
		t.Fatal("the cache did not refill after recomputing")
	}
}

// The archived set is the other half of the same fold. It used to cost a
// second one.
func TestArchivedIDsComeFromTheSameFold(t *testing.T) {
	manager, store := scaleManager(t, 600)
	defer manager.Close()
	ctx := context.Background()
	archivedID := "00000000-0000-4000-8000-000000000007"
	if err := store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{Meta: ledger.Meta{LaneID: archivedID}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ArchiveClosed(ctx, []string{archivedID}); err != nil {
		t.Fatal(err)
	}

	ids, err := manager.ArchivedSessionIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != archivedID {
		t.Fatalf("archived ids = %#v, want exactly the archived lane", ids)
	}

	started := time.Now()
	for range 20 {
		if _, err := manager.ArchivedSessionIDs(ctx); err != nil {
			t.Fatal(err)
		}
	}
	average := time.Since(started) / 20
	t.Logf("20 repeated archived-id reads over 600 lanes averaged %s each", average)
	if average > 2*time.Millisecond {
		t.Fatalf("the archived set averaged %s; it is meant to be the same fold, not another one", average)
	}

	// Archiving is an event, so the answer moves with it.
	second := "00000000-0000-4000-8000-000000000008"
	if err := store.Observations().RecordRunnerExited(ctx, ledger.RunnerExit{Meta: ledger.Meta{LaneID: second}}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ArchiveClosed(ctx, []string{second}); err != nil {
		t.Fatal(err)
	}
	ids, err = manager.ArchivedSessionIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("archived ids = %#v, want both lanes: the set must follow the log", ids)
	}
}

// A ledger that cannot say where it is gets no cache entry rather than a stale
// one. Correctness is not the thing being optimized here.
func TestAReaderWithoutAHighWaterMarkIsNeverCached(t *testing.T) {
	manager, _ := scaleManager(t, 4)
	defer manager.Close()
	manager.ledgerReader = unmarkedReader{LedgerReader: manager.ledgerReader}

	for range 3 {
		if _, cached, err := manager.ledgerStatesCached(context.Background()); err != nil || cached {
			t.Fatalf("a reader that cannot report its sequence was cached (hit=%v err=%v)", cached, err)
		}
	}
}

// unmarkedReader is a ledger reader that folds but cannot report its sequence —
// an older store, or a test double.
type unmarkedReader struct{ LedgerReader }

func (unmarkedReader) CurrentStates(ctx context.Context) ([]ledger.LaneState, error) {
	return nil, nil
}

func scaleManager(t *testing.T, lanes int) (*Manager, *ledger.Store) {
	t.Helper()
	root := t.TempDir()
	store, err := ledger.Open(context.Background(), ledger.Options{Path: filepath.Join(root, "lanes.sqlite3")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for index := range lanes {
		id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index)
		if err := store.Boundaries().RecordCreated(context.Background(), ledger.Created{
			Meta: ledger.Meta{LaneID: id}, LaneUUID: id, Name: fmt.Sprintf("lane-%03d", index),
			Tool: "codex", Cwd: root, CreatorKind: ledger.CreatorExternal, CreatorID: "test:local-user",
		}); err != nil {
			t.Fatal(err)
		}
	}
	manager := NewManager(state.Config{
		StateRoot: root, UserStateRoot: root, RunnerStateDir: filepath.Join(root, "runners"),
		LaunchAgentsDir: filepath.Join(root, "agents"),
	}, prototest.NewLauncher(), ManagerOptions{
		DisableWatchers: true, ActivityInterval: time.Hour,
		Boundaries: store.Boundaries(), Observations: store.Observations(),
		LedgerReader: store, Retention: store.Retention(),
	})
	t.Cleanup(manager.Close)
	return manager, store
}
