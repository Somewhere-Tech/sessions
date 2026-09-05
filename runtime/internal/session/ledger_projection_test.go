package session

import (
	"context"
	"errors"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

type projectedLedgerReader struct {
	LedgerReader
	err     error
	replays int
}

func (r *projectedLedgerReader) CurrentStates(context.Context) ([]ledger.LaneState, error) {
	if r.err != nil {
		return nil, r.err
	}
	return []ledger.LaneState{{LaneID: "fixture", UserKillRequested: true}}, nil
}

func (r *projectedLedgerReader) Events(context.Context, string) ([]ledger.Event, error) {
	r.replays++
	return nil, errors.New("unexpected full replay")
}

func TestManagerUsesCurrentLedgerStateAndPropagatesReadFailure(t *testing.T) {
	reader := &projectedLedgerReader{}
	manager := &Manager{ledgerReader: reader}
	states, err := manager.ledgerStates(context.Background())
	if err != nil || len(states) != 1 || !states[0].UserKillRequested || reader.replays != 0 {
		t.Fatalf("states=%+v err=%v replays=%d", states, err, reader.replays)
	}
	reader.err = errors.New("database unavailable")
	states, err = manager.ledgerStates(context.Background())
	if !errors.Is(err, reader.err) || states != nil || reader.replays != 0 {
		t.Fatalf("read failure masked: states=%+v err=%v replays=%d", states, err, reader.replays)
	}
}
