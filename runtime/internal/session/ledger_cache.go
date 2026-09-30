package session

import (
	"context"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

// Reading the ledger once per change instead of once per question.
//
// Measured on the installed Mini, 11 September, with 593 sessions: a listing
// spent 660 ms folding the ledger and a further 746 ms deciding which lanes
// were archived — the same fold again, for one boolean per lane — against
// 860 ms in the history store. On the MacBook's 115 sessions both stages
// rounded to zero, so this is what a machine that is actually being used pays.
//
// Both answers are pure functions of the event log, so they are exactly as
// fresh as the log's own sequence. The cache is keyed on that sequence — the
// high-water mark, one indexed row read — and recomputed only when it moves.
// Nothing here is time-based: two reads at the same mark describe the same
// ledger by construction, and a mark that has moved is a miss even if it moved
// a microsecond ago.
//
// Then the Mini reported hits that still cost 650 ms. A hit does no folding, so
// that time was in the two things around it: the high-water query, which shares
// one SQLite connection with every writer in the process, and the wait for
// whichever caller is folding right now. Both are measured separately here, so
// "the cache missed" and "the cache hit and waited half a second for the
// database" can never again read as the same number.
//
// Memory is one entry per lane, which is what the projection underneath costs
// anyway; this holds the same states rather than a second copy of them.
type ledgerCache struct {
	// mu guards the cached answer. Readers take it for reading, so a caller
	// whose snapshot is already cached is never stuck behind one that is
	// folding: the fold runs outside this lock and only its publication holds
	// it.
	mu sync.RWMutex
	// foldMu admits one folder at a time. Two callers that both missed would
	// otherwise read the same events twice on the one connection the writers
	// are also using.
	foldMu sync.Mutex
	// mark is the ledger sequence these answers describe. ready distinguishes
	// "nothing cached" from "cached at an empty ledger", whose mark is zero.
	mark  int64
	ready bool
	// states is shared with every caller, and every caller reads it. Nothing in
	// this package writes through it — they all range by value — and nothing
	// should start: it is one slice handed to everybody who asks between two
	// ledger events.
	states   []ledger.LaneState
	archived []string
}

// LedgerMark identifies the ledger snapshot an answer describes. A caller that
// has one can ask a second question about the same snapshot without paying for
// a second high-water read — and gets an answer about the same ledger, rather
// than one that straddles an event that arrived in between.
type LedgerMark struct {
	Seq   int64
	Known bool
}

// ledgerTiming is where a ledger read went, down to the parts a cache hit is
// not supposed to pay for.
type ledgerTiming struct {
	// HighWater is the MAX(seq) query: the cost of learning whether anything
	// has happened. It includes waiting for the ledger's single connection.
	HighWater time.Duration
	// Wait is time spent waiting for another caller that is mid-fold.
	Wait time.Duration
	// Fold is the incremental projection itself, paid only on a miss.
	Fold time.Duration
	// ConnWait is how much of this read the database pool spent handing out its
	// one connection — the writers' share of a slow listing, in their own
	// accounting rather than ours.
	ConnWait time.Duration
}

// ledgerAnswer is one read of the ledger's derived state.
type ledgerAnswer struct {
	states   []ledger.LaneState
	archived []string
	cached   bool
	mark     LedgerMark
	timing   ledgerTiming
}

// ledgerStates is the folded state of every lane. It is the same answer
// CurrentStates gives, served from the cache while the ledger has not moved.
func (m *Manager) ledgerStates(ctx context.Context) ([]ledger.LaneState, error) {
	answer, err := m.readLedger(ctx)
	return answer.states, err
}

// ledgerStatesCached also reports whether the answer came from the cache, so a
// listing can say which it paid for.
func (m *Manager) ledgerStatesCached(ctx context.Context) ([]ledger.LaneState, bool, error) {
	answer, err := m.readLedger(ctx)
	return answer.states, answer.cached, err
}

func (m *Manager) readLedger(ctx context.Context) (answer ledgerAnswer, err error) {
	if m.ledgerReader == nil {
		return ledgerAnswer{}, nil
	}
	connBefore, hasConnWait := m.ledgerConnWait()

	markStart := time.Now()
	seq, marked := m.ledgerMark(ctx)
	answer.timing.HighWater = time.Since(markStart)
	answer.mark = LedgerMark{Seq: seq, Known: marked}
	defer func() {
		if connAfter, ok := m.ledgerConnWait(); ok && hasConnWait {
			answer.timing.ConnWait = connAfter - connBefore
		}
	}()

	waitStart := time.Now()
	states, archived, hit := m.cachedAt(seq, marked)
	answer.timing.Wait = time.Since(waitStart)
	if hit {
		answer.states, answer.archived, answer.cached = states, archived, true
		return answer, nil
	}

	// A miss folds, and only one caller folds at a time. The others wait here
	// and then find the answer already published, which is cheaper than each of
	// them reading the same events on the same connection.
	foldWait := time.Now()
	m.ledgerCache.foldMu.Lock()
	defer m.ledgerCache.foldMu.Unlock()
	answer.timing.Wait += time.Since(foldWait)
	if states, archived, hit := m.cachedAt(seq, marked); hit {
		answer.states, answer.archived, answer.cached = states, archived, true
		return answer, nil
	}

	foldStart := time.Now()
	folded, foldErr := m.readLedgerStates(ctx)
	answer.timing.Fold = time.Since(foldStart)
	if foldErr != nil {
		return answer, foldErr
	}
	answer.states = folded
	answer.archived = archivedIDs(folded)
	m.publishLedger(seq, marked, answer.states, answer.archived)
	return answer, nil
}

// cachedAt is the answer for one exact ledger snapshot, or nothing.
func (m *Manager) cachedAt(seq int64, marked bool) ([]ledger.LaneState, []string, bool) {
	if !marked {
		return nil, nil, false
	}
	m.ledgerCache.mu.RLock()
	defer m.ledgerCache.mu.RUnlock()
	if !m.ledgerCache.ready || m.ledgerCache.mark != seq {
		return nil, nil, false
	}
	return m.ledgerCache.states, m.ledgerCache.archived, true
}

// publishLedger stores a fold under the mark that was read before it ran. The
// fold may include events that arrived while it ran, which makes the entry
// newer than its label and never older: a later read at a later mark misses and
// folds again, and no read is ever answered with a ledger that went backwards.
//
// A ledger that cannot say where it is gets no cache entry rather than a stale
// one: correctness is not the thing being optimized here.
func (m *Manager) publishLedger(seq int64, marked bool, states []ledger.LaneState, archived []string) {
	m.ledgerCache.mu.Lock()
	defer m.ledgerCache.mu.Unlock()
	if !marked {
		m.ledgerCache.ready = false
		return
	}
	m.ledgerCache.mark = seq
	m.ledgerCache.ready = true
	m.ledgerCache.states = states
	m.ledgerCache.archived = archived
}

// archivedLaneIDs is the archived set for the current ledger, computed with the
// fold it comes from rather than by folding again. This is the stage that cost
// the Mini 746 ms per listing.
func (m *Manager) archivedLaneIDs(ctx context.Context) ([]string, error) {
	answer, err := m.readLedger(ctx)
	return answer.archived, err
}

// archivedLaneIDsAt answers for a snapshot the caller already has. A listing
// reads the high-water mark once and asks both its questions about that exact
// ledger: the second question then costs a map lookup instead of a second query
// on the connection every writer is also using.
func (m *Manager) archivedLaneIDsAt(at LedgerMark) ([]string, ledgerTiming, bool) {
	if m.ledgerReader == nil || !at.Known {
		return nil, ledgerTiming{}, false
	}
	waitStart := time.Now()
	_, archived, hit := m.cachedAt(at.Seq, at.Known)
	return archived, ledgerTiming{Wait: time.Since(waitStart)}, hit
}

func archivedIDs(states []ledger.LaneState) []string {
	ids := make([]string, 0, 8)
	for _, lane := range states {
		if lane.Archived {
			ids = append(ids, lane.LaneID)
		}
	}
	return ids
}

// ledgerMark is the ledger's own cursor. A reader that cannot answer it — an
// older store, a test double — simply never gets a cache hit.
func (m *Manager) ledgerMark(ctx context.Context) (int64, bool) {
	reader, ok := m.ledgerReader.(ledger.HighWaterReader)
	if !ok {
		return 0, false
	}
	mark, err := reader.HighWaterMark(ctx)
	if err != nil {
		return 0, false
	}
	return mark, true
}

// ledgerConnWait is the pool's own total wait for its single connection, which
// only a real store can report.
func (m *Manager) ledgerConnWait() (time.Duration, bool) {
	reader, ok := m.ledgerReader.(ledger.ConnectionWaitReader)
	if !ok {
		return 0, false
	}
	return reader.ConnectionWait(), true
}

func (m *Manager) readLedgerStates(ctx context.Context) ([]ledger.LaneState, error) {
	if reader, ok := m.ledgerReader.(ledger.CurrentStateReader); ok {
		return reader.CurrentStates(ctx)
	}
	events, err := m.ledgerReader.Events(ctx, "")
	if err != nil {
		return nil, err
	}
	return ledger.Fold(events), nil
}
