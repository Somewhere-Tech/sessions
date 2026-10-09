package session

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/proto/prototest"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// failingLedgerReader is a ledger that cannot be read at all.
type failingLedgerReader struct {
	LedgerReader
}

func (failingLedgerReader) HighWaterMark(context.Context) (int64, error) {
	return 0, errors.New("ledger unreadable")
}

func (failingLedgerReader) CurrentStates(context.Context) ([]ledger.LaneState, error) {
	return nil, errors.New("ledger unreadable")
}

// patientLedgerReader blocks where it is told to until its caller's context
// ends, and then says so — a reader that honors its context, which is the
// only kind ListContext can bound. Calls are counted.
type patientLedgerReader struct {
	LedgerReader
	blockMark  atomic.Bool
	blockFolds atomic.Bool
	marks      atomic.Int64
	folds      atomic.Int64
}

func (p *patientLedgerReader) HighWaterMark(ctx context.Context) (int64, error) {
	p.marks.Add(1)
	if p.blockMark.Load() {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	return p.LedgerReader.(ledger.HighWaterReader).HighWaterMark(ctx)
}

func (p *patientLedgerReader) CurrentStates(ctx context.Context) ([]ledger.LaneState, error) {
	p.folds.Add(1)
	if p.blockFolds.Load() {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.LedgerReader.(ledger.CurrentStateReader).CurrentStates(ctx)
}

func recordedShellSession(t *testing.T) (*Manager, state.SessionInfo) {
	t.Helper()
	root := t.TempDir()
	store := openStartLedger(t, root)
	manager := startReplayManager(t, root, prototest.NewLauncher(), store)
	created, err := manager.Create(context.Background(), state.CreateSessionRequest{
		Cmd: "/bin/sh", Cwd: root, OperationID: testStartOperation, PromptOperationID: testPromptOperation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, created
}

func TestListContextRefusesAListingWithoutDurableState(t *testing.T) {
	manager, created := recordedShellSession(t)
	if listed, err := manager.ListContext(context.Background(), true); err != nil || len(listed) != 1 || listed[0].Start == nil {
		t.Fatalf("healthy listing = %#v, %v; want the session with its start receipt", listed, err)
	}
	manager.ledgerReader = failingLedgerReader{LedgerReader: manager.ledgerReader}

	listed, err := manager.ListContext(context.Background(), true)
	if err == nil || listed != nil {
		t.Fatalf("listing without durable state = %#v, %v; want an error and no sessions", listed, err)
	}
	// What ListContext refuses to present: the best-effort listing still
	// answers, and has lost the start receipt only the ledger holds.
	partial, _ := manager.ListTimed(true)
	if len(partial) != 1 || partial[0].ID != created.ID || partial[0].Start != nil {
		t.Fatalf("best-effort listing = %#v; this test documents that it lacks durable state", partial)
	}
}

func TestListContextDoesNotFoldAfterACancelledHighWaterRead(t *testing.T) {
	manager, _ := recordedShellSession(t)
	patient := &patientLedgerReader{LedgerReader: manager.ledgerReader}
	patient.blockMark.Store(true)
	manager.ledgerReader = patient

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := manager.ListContext(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listing with a cancelled high-water read = %v, want the deadline", err)
	}
	if folds := patient.folds.Load(); folds != 0 {
		t.Fatalf("a cancelled listing went on to fold the ledger %d times", folds)
	}
}

func TestListContextStopsWaitingForAnotherCallersFold(t *testing.T) {
	manager, store := scaleManager(t, 20)
	ctx := context.Background()
	if _, err := manager.ListContext(ctx, true); err != nil {
		t.Fatal(err)
	}
	// Move the ledger so the next read misses, then hold a fold open with a
	// reader that ignores its context: the holder is deliberately stuck.
	blocking := newBlockingLedgerReader(manager.ledgerReader)
	manager.ledgerReader = blocking
	if err := store.Observations().RecordIdle(ctx, ledger.Observation{
		Meta: ledger.Meta{LaneID: "00000000-0000-4000-8000-000000000003"},
	}); err != nil {
		t.Fatal(err)
	}
	holderDone := make(chan error, 1)
	go func() {
		_, err := manager.ledgerStates(ctx)
		holderDone <- err
	}()
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		close(blocking.release)
		t.Fatal("the holder never started folding")
	}

	waiter, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	answered := make(chan error, 1)
	go func() {
		_, err := manager.ListContext(waiter, true)
		answered <- err
	}()
	select {
	case err := <-answered:
		if !errors.Is(err, context.DeadlineExceeded) {
			close(blocking.release)
			<-holderDone
			t.Fatalf("waiter answered %v, want its deadline", err)
		}
	case <-time.After(3 * time.Second):
		close(blocking.release)
		<-holderDone
		t.Fatal("a listing with a deadline kept waiting for another caller's fold")
	}

	// The holder finishes, its answer is cached, and the waiter that gave up
	// left no admission behind: the next listing is a cache hit, not a second
	// fold and not a wait.
	close(blocking.release)
	if err := <-holderDone; err != nil {
		t.Fatal(err)
	}
	next, cancelNext := context.WithTimeout(ctx, 5*time.Second)
	defer cancelNext()
	if _, err := manager.ListContext(next, true); err != nil {
		t.Fatalf("listing after the fold = %v", err)
	}
	if folds := blocking.folds.Load(); folds != 1 {
		t.Fatalf("folds = %d, want the holder's one fold reused", folds)
	}
}

func TestACancelledFoldPublishesNothingAndFreesTheSlot(t *testing.T) {
	manager, _ := recordedShellSession(t)
	patient := &patientLedgerReader{LedgerReader: manager.ledgerReader}
	manager.ledgerReader = patient
	// Forget the cached fold so the next listing has to fold again.
	manager.ledgerCache.mu.Lock()
	manager.ledgerCache.ready = false
	manager.ledgerCache.mu.Unlock()
	patient.blockFolds.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := manager.ListContext(ctx, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("listing whose fold was cancelled = %v, want the deadline", err)
	}
	manager.ledgerCache.mu.RLock()
	published := manager.ledgerCache.ready
	manager.ledgerCache.mu.RUnlock()
	if published {
		t.Fatal("a cancelled fold published a cache entry")
	}

	patient.blockFolds.Store(false)
	next, cancelNext := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelNext()
	listed, err := manager.ListContext(next, true)
	if err != nil || len(listed) != 1 || listed[0].Start == nil {
		t.Fatalf("listing after a cancelled fold = %#v, %v; want the session with its receipt", listed, err)
	}
	if again, err := manager.ListContext(next, true); err != nil || len(again) != 1 {
		t.Fatalf("repeat listing = %#v, %v", again, err)
	}
	// One cancelled fold, one real fold, and the repeat was a cache hit.
	if folds := patient.folds.Load(); folds != 2 {
		t.Fatalf("folds = %d, want 2", folds)
	}
}
