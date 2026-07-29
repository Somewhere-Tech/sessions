// Package claudep runs structured Claude Code print-mode turns.
package claudep

import (
	"context"
	"encoding/json"
	"sync"
)

// Event is one normalized Claude stream-json record. Raw is the canonical
// history representation consumed by Sessions.
type Event struct {
	Raw       json.RawMessage
	Type      string
	Subtype   string
	SessionID string
	Message   string
	Usage     json.RawMessage
}

// TurnResult is the authoritative completion extracted from Claude's result
// record after the print-mode process exits.
type TurnResult struct {
	SessionID string          `json:"sessionId"`
	Message   string          `json:"message"`
	Usage     json.RawMessage `json:"usage,omitempty"`
	Subtype   string          `json:"subtype,omitempty"`
	IsError   bool            `json:"isError"`
}

// TurnStream delivers ordered normalized records and an independently
// awaitable final result.
type TurnStream struct {
	Events <-chan Event
	state  *turnState
}

func (s *TurnStream) Result(ctx context.Context) (TurnResult, error) {
	select {
	case <-s.state.done:
		s.state.mu.Lock()
		defer s.state.mu.Unlock()
		return s.state.result, s.state.err
	case <-ctx.Done():
		return TurnResult{}, ctx.Err()
	}
}

type turnState struct {
	mu     sync.Mutex
	result TurnResult
	err    error
	done   chan struct{}
}

func newTurnState() *turnState { return &turnState{done: make(chan struct{})} }

func (s *turnState) finish(result TurnResult, err error) {
	s.mu.Lock()
	s.result = result
	s.err = err
	close(s.done)
	s.mu.Unlock()
}
