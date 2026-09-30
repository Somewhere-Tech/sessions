package session

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

// Reported from the Mini, 11 September, after the cache landed: listings came
// back with `ledger_cached: true` and `ledger_ms` of 205, 677 and 654, plus
// `archived_ms` of 377, 599 and 176. A hit folds nothing, so a hit that costs
// two thirds of a second is measuring something else — and the timing object
// could not say what. These are the sub-timings that can.
func TestACacheHitFoldsNothingAndSaysWhatItDidPayFor(t *testing.T) {
	manager, _ := scaleManager(t, 600)
	defer manager.Close()
	ctx := context.Background()

	if _, err := manager.readLedger(ctx); err != nil {
		t.Fatal(err)
	}
	var worst, marks, waits time.Duration
	for range 20 {
		answer, err := manager.readLedger(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !answer.cached {
			t.Fatal("a read of an unchanged ledger missed the cache")
		}
		if answer.timing.Fold != 0 {
			t.Fatalf("a cache hit folded the ledger for %s", answer.timing.Fold)
		}
		marks += answer.timing.HighWater
		waits += answer.timing.Wait
		if total := answer.timing.HighWater + answer.timing.Wait; total > worst {
			worst = total
		}
	}
	t.Logf("20 hits over 600 lanes: high-water %s total, wait %s total, worst hit %s",
		marks, waits, worst)
	// The whole point of the mark is that asking costs nothing to speak of.
	// This is three orders of magnitude below what the Mini reported, which is
	// the headroom a loaded machine needs.
	if average := marks / 20; average > time.Millisecond {
		t.Fatalf("the high-water read averaged %s on an idle ledger", average)
	}
}

// The Mini's numbers came from a daemon that was also writing. One SQLite
// connection is shared by every reader and writer in the process, so a listing
// that arrives mid-burst queues for it — and that has to read as a queue, not
// as a cache that failed.
func TestAWriterBurstShowsUpAsHighWaterAndConnectionWaitNotAsAMiss(t *testing.T) {
	manager, store := scaleManager(t, 200)
	defer manager.Close()
	ctx := context.Background()
	if _, err := manager.readLedger(ctx); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var writes atomic.Int64
	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for index := 0; ; index++ {
			select {
			case <-stop:
				return
			default:
			}
			id := fmt.Sprintf("00000000-0000-4000-8000-%012d", index%200)
			if err := store.Observations().RecordIdle(ctx, ledger.Observation{Meta: ledger.Meta{LaneID: id}}); err != nil {
				return
			}
			writes.Add(1)
		}
	}()

	var slowest ledgerTiming
	var reads, hits int
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		answer, err := manager.readLedger(ctx)
		if err != nil {
			t.Fatal(err)
		}
		reads++
		if answer.cached {
			hits++
		}
		if answer.timing.HighWater+answer.timing.Wait+answer.timing.Fold >
			slowest.HighWater+slowest.Wait+slowest.Fold {
			slowest = answer.timing
		}
	}
	close(stop)
	writer.Wait()

	t.Logf("%d reads (%d hits) against %d concurrent writes; slowest read: high-water %s, wait %s, fold %s, connection wait %s",
		reads, hits, writes.Load(), slowest.HighWater, slowest.Wait, slowest.Fold, slowest.ConnWait)
	if reads == 0 || writes.Load() == 0 {
		t.Fatal("the fixture did not exercise a reader against a writer")
	}
	// Whatever the slowest read cost, the breakdown accounts for it. That is
	// the property this exists to keep: no unexplained milliseconds.
	if slowest.HighWater == 0 && slowest.Wait == 0 && slowest.Fold == 0 {
		t.Fatal("the slowest read under contention reported no time anywhere")
	}
}

// A listing asks the ledger two questions. It used to read the high-water mark
// for each of them — two queries on the connection the writers are using, for
// one listing — and the two answers could straddle an event. Now the second
// question is asked about the snapshot the first one read.
func TestAListingReadsTheHighWaterMarkOnce(t *testing.T) {
	manager, _ := scaleManager(t, 50)
	defer manager.Close()
	counting := &countingLedgerReader{LedgerReader: manager.ledgerReader}
	manager.ledgerReader = counting
	ctx := context.Background()

	_, timing := manager.ListTimed(true)
	if !timing.Mark.Known {
		t.Fatal("the listing did not learn which ledger snapshot it read")
	}
	ids, reused, err := manager.ArchivedSessionIDsAt(ctx, timing.Mark)
	if err != nil {
		t.Fatal(err)
	}
	if !reused {
		t.Fatal("the archived set re-read the ledger instead of answering for the listing's own snapshot")
	}
	if len(ids) != 0 {
		t.Fatalf("archived ids = %#v on a ledger where nothing was archived", ids)
	}
	if reads := counting.marks.Load(); reads != 1 {
		t.Fatalf("one listing read the high-water mark %d times, want 1", reads)
	}

	// A caller without a mark — anything that is not mid-listing — still gets a
	// correct answer, by reading the mark itself.
	if _, reusedWithout, err := manager.ArchivedSessionIDsAt(ctx, LedgerMark{}); err != nil || reusedWithout {
		t.Fatalf("a caller with no snapshot claimed to reuse one (reused=%v err=%v)", reusedWithout, err)
	}
	if counting.marks.Load() != 2 {
		t.Fatalf("the fallback did not read the mark itself: %d reads", counting.marks.Load())
	}
}

// countingLedgerReader is the store, counting the question the cache is built
// around: how many times a piece of work asked where the ledger is.
type countingLedgerReader struct {
	LedgerReader
	marks atomic.Int64
}

func (c *countingLedgerReader) HighWaterMark(ctx context.Context) (int64, error) {
	c.marks.Add(1)
	reader, ok := c.LedgerReader.(ledger.HighWaterReader)
	if !ok {
		return 0, nil
	}
	return reader.HighWaterMark(ctx)
}

func (c *countingLedgerReader) CurrentStates(ctx context.Context) ([]ledger.LaneState, error) {
	reader, ok := c.LedgerReader.(ledger.CurrentStateReader)
	if !ok {
		return nil, nil
	}
	return reader.CurrentStates(ctx)
}

func (c *countingLedgerReader) ConnectionWait() time.Duration {
	reader, ok := c.LedgerReader.(ledger.ConnectionWaitReader)
	if !ok {
		return 0
	}
	return reader.ConnectionWait()
}

// A listing that has already read its snapshot must not queue behind somebody
// else's fold. On the Mini the fold after a restart took 1.1 s; every request
// that arrived during it waited the same 1.1 s to be told a boolean per lane,
// and reported the wait as if the ledger stage itself were slow.
func TestAnAnsweredSnapshotDoesNotWaitForSomebodyElsesFold(t *testing.T) {
	manager, store := scaleManager(t, 100)
	defer manager.Close()
	ctx := context.Background()

	// The listing reads its snapshot, the way ListTimed does.
	_, timing := manager.ListTimed(true)
	if !timing.Mark.Known {
		t.Fatal("the listing did not learn its ledger snapshot")
	}

	// Something happens, and a second caller starts folding — slowly.
	blocking := newBlockingLedgerReader(manager.ledgerReader)
	manager.ledgerReader = blocking
	if err := store.Observations().RecordIdle(ctx, ledger.Observation{
		Meta: ledger.Meta{LaneID: "00000000-0000-4000-8000-000000000003"},
	}); err != nil {
		t.Fatal(err)
	}
	folding := make(chan struct{})
	go func() {
		defer close(folding)
		_, _ = manager.ledgerStates(ctx)
	}()
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the second caller never started folding")
	}

	// The first caller's question is about a snapshot that is already known.
	answered := make(chan time.Duration, 1)
	go func() {
		started := time.Now()
		_, reused, err := manager.ArchivedSessionIDsAt(ctx, timing.Mark)
		if err != nil || !reused {
			answered <- -1
			return
		}
		answered <- time.Since(started)
	}()
	select {
	case took := <-answered:
		if took < 0 {
			t.Fatal("the archived set did not answer for the listing's own snapshot")
		}
		t.Logf("the archived set answered in %s while a fold was in flight", took)
	case <-time.After(3 * time.Second):
		close(blocking.release)
		<-folding
		t.Fatal("a snapshot that was already cached waited for another caller's fold")
	}
	close(blocking.release)
	<-folding
}

// Two callers that miss together read the events once between them, not twice
// on the connection the writers are also using.
func TestConcurrentMissesFoldOnce(t *testing.T) {
	manager, _ := scaleManager(t, 50)
	defer manager.Close()
	counting := &countingLedgerReader{LedgerReader: manager.ledgerReader}
	blocking := newBlockingLedgerReader(counting)
	manager.ledgerReader = blocking

	var readers sync.WaitGroup
	for range 4 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			_, _ = manager.ledgerStates(context.Background())
		}()
	}
	select {
	case <-blocking.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("nobody folded")
	}
	close(blocking.release)
	readers.Wait()
	if folds := blocking.folds.Load(); folds != 1 {
		t.Fatalf("four concurrent readers folded the ledger %d times, want 1", folds)
	}
}

func newBlockingLedgerReader(inner LedgerReader) *blockingLedgerReader {
	return &blockingLedgerReader{
		LedgerReader: inner, entered: make(chan struct{}), release: make(chan struct{}),
	}
}

// blockingLedgerReader holds a fold open, which is what a cold ledger does to
// everything else for as long as it takes.
type blockingLedgerReader struct {
	LedgerReader
	once    sync.Once
	entered chan struct{}
	release chan struct{}
	folds   atomic.Int64
}

func (b *blockingLedgerReader) CurrentStates(ctx context.Context) ([]ledger.LaneState, error) {
	b.folds.Add(1)
	b.once.Do(func() { close(b.entered) })
	<-b.release
	reader, ok := b.LedgerReader.(ledger.CurrentStateReader)
	if !ok {
		return nil, nil
	}
	return reader.CurrentStates(ctx)
}

func (b *blockingLedgerReader) HighWaterMark(ctx context.Context) (int64, error) {
	reader, ok := b.LedgerReader.(ledger.HighWaterReader)
	if !ok {
		return 0, nil
	}
	return reader.HighWaterMark(ctx)
}

func (b *blockingLedgerReader) ConnectionWait() time.Duration {
	reader, ok := b.LedgerReader.(ledger.ConnectionWaitReader)
	if !ok {
		return 0
	}
	return reader.ConnectionWait()
}
