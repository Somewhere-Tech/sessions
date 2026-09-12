package session

import (
	"context"
	"sync"

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
// Memory is one entry per lane, which is what the projection underneath costs
// anyway; this holds the same states rather than a second copy of them.
type ledgerCache struct {
	mu sync.Mutex
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

// ledgerStates is the folded state of every lane. It is the same answer
// CurrentStates gives, served from the cache while the ledger has not moved.
func (m *Manager) ledgerStates(ctx context.Context) ([]ledger.LaneState, error) {
	states, _, err := m.ledgerStatesCached(ctx)
	return states, err
}

// ledgerStatesCached also reports whether the answer came from the cache, so a
// listing can say which it paid for.
func (m *Manager) ledgerStatesCached(ctx context.Context) ([]ledger.LaneState, bool, error) {
	if m.ledgerReader == nil {
		return nil, false, nil
	}
	mark, marked := m.ledgerMark(ctx)
	m.ledgerCache.mu.Lock()
	defer m.ledgerCache.mu.Unlock()
	if marked && m.ledgerCache.ready && m.ledgerCache.mark == mark {
		return m.ledgerCache.states, true, nil
	}
	states, err := m.readLedgerStates(ctx)
	if err != nil {
		return nil, false, err
	}
	// A ledger that cannot say where it is gets no cache entry rather than a
	// stale one: correctness is not the thing being optimized here.
	if !marked {
		m.ledgerCache.ready = false
		return states, false, nil
	}
	m.ledgerCache.mark = mark
	m.ledgerCache.ready = true
	m.ledgerCache.states = states
	m.ledgerCache.archived = archivedIDs(states)
	return states, false, nil
}

// archivedLaneIDs is the archived set for the current ledger, computed with the
// fold it comes from rather than by folding again. This is the stage that cost
// the Mini 746 ms per listing.
func (m *Manager) archivedLaneIDs(ctx context.Context) ([]string, error) {
	states, cached, err := m.ledgerStatesCached(ctx)
	if err != nil {
		return nil, err
	}
	if cached {
		m.ledgerCache.mu.Lock()
		defer m.ledgerCache.mu.Unlock()
		if m.ledgerCache.ready {
			return m.ledgerCache.archived, nil
		}
	}
	return archivedIDs(states), nil
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
