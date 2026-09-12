package ledger

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// CurrentStateReader returns a detached projection of committed ledger state.
// Each read observes a database snapshot; it never serves a timed stale cache.
type CurrentStateReader interface {
	CurrentStates(context.Context) ([]LaneState, error)
}

// HighWaterReader answers "has anything happened?" without reading what.
//
// The sequence is the append-only log's own cursor, so two reads that return
// the same number describe the same ledger — by construction, not by a timer.
// A caller holding something derived from the log can therefore keep it until
// the number moves, which is exact rather than a heuristic.
type HighWaterReader interface {
	HighWaterMark(context.Context) (int64, error)
}

// HighWaterMark is the sequence of the newest committed lane event, or zero for
// an empty ledger. It is one indexed row read: the primary key is the sequence,
// so this is a b-tree seek to the last leaf and nothing else.
func (s *Store) HighWaterMark(ctx context.Context) (int64, error) {
	var mark int64
	row := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM lane_events`)
	if err := row.Scan(&mark); err != nil {
		return 0, fmt.Errorf("read ledger high-water mark: %w", err)
	}
	return mark, nil
}

// ConnectionWaitReader reports how long callers have queued for the ledger's
// single connection. The store keeps one connection so its pragmas stay put,
// which means every read shares it with every writer: when a listing says its
// high-water read took half a second, this is how to tell "the query is slow"
// from "the writers had the connection".
type ConnectionWaitReader interface {
	ConnectionWait() time.Duration
}

// ConnectionWait is the pool's own cumulative wait, since the process started.
// Callers difference it around the work they are measuring.
func (s *Store) ConnectionWait() time.Duration { return s.db.Stats().WaitDuration }

type currentProjection struct {
	mu    sync.Mutex
	seq   int64
	lanes map[string]*LaneState
}

// CurrentStates streams only rows appended after the last successful read.
// The append-only database sequence, not timestamps or local write callbacks,
// is the cursor, so commits from other Store instances/processes are visible.
// Memory is proportional to lane state, never to retained event history.
func (s *Store) CurrentStates(ctx context.Context) ([]LaneState, error) {
	p := &s.projection
	p.mu.Lock()
	defer p.mu.Unlock()
	changes, seq, err := s.readCurrentChanges(ctx, p)
	if err != nil {
		return nil, err
	}
	if p.lanes == nil {
		p.lanes = make(map[string]*LaneState)
	}
	for id, state := range changes {
		p.lanes[id] = state
	}
	p.seq = seq
	return snapshotLaneStates(p.lanes), nil
}

func (s *Store) readCurrentChanges(ctx context.Context, p *currentProjection) (map[string]*LaneState, int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT seq, lane_id, type, at_ms, payload_json FROM lane_events WHERE seq > ? ORDER BY seq`, p.seq)
	if err != nil {
		return nil, 0, fmt.Errorf("read current ledger state: %w", err)
	}
	defer rows.Close()
	changes := make(map[string]*LaneState)
	seq := p.seq
	for rows.Next() {
		var event Event
		var kind, payload string
		if err := rows.Scan(&event.Seq, &event.LaneID, &kind, &event.AtMS, &payload); err != nil {
			return nil, 0, fmt.Errorf("scan current ledger event: %w", err)
		}
		event.Type, event.Payload = EventType(kind), []byte(payload)
		seq = event.Seq
		if event.LaneID == "" {
			continue
		}
		state := changes[event.LaneID]
		if state == nil {
			state = &LaneState{LaneID: event.LaneID}
			if prior := p.lanes[event.LaneID]; prior != nil {
				*state = cloneLaneState(*prior)
			}
			changes[event.LaneID] = state
		}
		applyLaneEvent(state, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read current ledger events: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, 0, fmt.Errorf("close current ledger events: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	// Publish both state and cursor only after the complete snapshot succeeds.
	// On error the old projection remains private and the next read retries.
	return changes, seq, nil
}
