package codexapp

import (
	"context"
	"sync"
)

// Event is one structured app-server turn notification.
type Event interface {
	isCodexAppEvent()
}

type TurnStarted struct {
	ConversationID string `json:"conversationId"`
	TurnID         string `json:"turnId"`
}

func (TurnStarted) isCodexAppEvent() {}

type AgentMessageDelta struct {
	ConversationID string `json:"conversationId"`
	TurnID         string `json:"turnId"`
	ItemID         string `json:"itemId"`
	Delta          string `json:"delta"`
}

func (AgentMessageDelta) isCodexAppEvent() {}

type ItemStarted struct {
	ConversationID string     `json:"conversationId"`
	TurnID         string     `json:"turnId"`
	StartedAtMS    int64      `json:"startedAtMs"`
	Item           ThreadItem `json:"item"`
}

func (ItemStarted) isCodexAppEvent() {}

type ItemCompleted struct {
	ConversationID string     `json:"conversationId"`
	TurnID         string     `json:"turnId"`
	CompletedAtMS  int64      `json:"completedAtMs"`
	Item           ThreadItem `json:"item"`
}

func (ItemCompleted) isCodexAppEvent() {}

// TurnPlanStep is one app-server plan row. Status is intentionally kept as
// the provider string so new states remain forward-compatible in history and
// can still be rendered by newer Sessions clients.
type TurnPlanStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
}

// PlanUpdated carries the authoritative turn-level plan projection exposed by
// app-server. It is separate from plan ThreadItems: the latter are narrative
// plan content, while this event has per-step lifecycle state for a GUI.
type PlanUpdated struct {
	ConversationID string         `json:"conversationId"`
	TurnID         string         `json:"turnId"`
	Explanation    *string        `json:"explanation,omitempty"`
	Plan           []TurnPlanStep `json:"plan"`
}

func (PlanUpdated) isCodexAppEvent() {}

type TokenCount struct {
	ConversationID string     `json:"conversationId"`
	TurnID         string     `json:"turnId"`
	Usage          TokenUsage `json:"usage"`
}

func (TokenCount) isCodexAppEvent() {}

type TurnComplete struct {
	ConversationID string     `json:"conversationId"`
	TurnID         string     `json:"turnId"`
	Status         string     `json:"status"`
	Error          *TurnError `json:"error,omitempty"`
}

func (TurnComplete) isCodexAppEvent() {}

// TurnResult is the authoritative result accumulated from structured events.
type TurnResult struct {
	ConversationID string     `json:"conversationId"`
	TurnID         string     `json:"turnId"`
	Message        string     `json:"message"`
	TokenUsage     TokenUsage `json:"tokenUsage"`
	Status         string     `json:"status"`
	Error          *TurnError `json:"error,omitempty"`
}

// TurnStream delivers ordered events and an independently awaitable result.
// Callers should range Events until it closes.
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
	conversationID string

	mu          sync.Mutex
	turnID      string
	usage       TokenUsage
	message     string
	deltas      map[string]string
	lastAgentID string
	finished    bool
	result      TurnResult
	err         error
	done        chan struct{}
	events      *eventQueue
}

func newTurnState(conversationID string) *turnState {
	return &turnState{
		conversationID: conversationID,
		deltas:         make(map[string]string),
		done:           make(chan struct{}),
		events:         newEventQueue(),
	}
}

func (s *turnState) stream() *TurnStream {
	return &TurnStream{Events: s.events.out, state: s}
}

func (s *turnState) acceptTurnID(turnID string) bool {
	if turnID == "" {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turnID == "" {
		s.turnID = turnID
	}
	return s.turnID == turnID
}

func (s *turnState) currentTurnID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnID
}

func (s *turnState) emit(event Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.applyLocked(event)
	s.events.send(event)
}

func (s *turnState) started(turnID string) {
	s.emit(TurnStarted{ConversationID: s.conversationID, TurnID: turnID})
}

func (s *turnState) applyLocked(event Event) {
	switch event := event.(type) {
	case AgentMessageDelta:
		s.deltas[event.ItemID] += event.Delta
		s.lastAgentID = event.ItemID
	case ItemCompleted:
		if event.Item.Type != "agentMessage" {
			return
		}
		text := event.Item.Text
		if text == "" {
			text = s.deltas[event.Item.ID]
		}
		if event.Item.Phase == nil || *event.Item.Phase != "commentary" {
			s.message = text
		}
	case TokenCount:
		s.usage = event.Usage
	}
}

func (s *turnState) complete(event TurnComplete, items []ThreadItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	for _, item := range items {
		if item.Type != "agentMessage" {
			continue
		}
		if item.Phase == nil || *item.Phase != "commentary" {
			s.message = item.Text
		}
	}
	if s.message == "" {
		s.message = s.deltas[s.lastAgentID]
	}
	s.events.send(event)
	s.result = TurnResult{
		ConversationID: s.conversationID,
		TurnID:         event.TurnID,
		Message:        s.message,
		TokenUsage:     s.usage,
		Status:         event.Status,
		Error:          event.Error,
	}
	s.finished = true
	s.events.stop()
	close(s.done)
}

func (s *turnState) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return
	}
	s.err = err
	s.finished = true
	s.events.stop()
	close(s.done)
}

type queuedEvent struct {
	event Event
	stop  bool
}

// eventQueue is unbounded so Result can complete even when a caller waits for
// it before draining a long event stream.
type eventQueue struct {
	in  chan queuedEvent
	out chan Event
}

func newEventQueue() *eventQueue {
	queue := &eventQueue{
		in:  make(chan queuedEvent, 16),
		out: make(chan Event),
	}
	go queue.run()
	return queue
}

func (q *eventQueue) send(event Event) {
	q.in <- queuedEvent{event: event}
}

func (q *eventQueue) stop() {
	q.in <- queuedEvent{stop: true}
}

func (q *eventQueue) run() {
	var pending []Event
	stopping := false
	for !stopping || len(pending) > 0 {
		var output chan Event
		var next Event
		if len(pending) > 0 {
			output = q.out
			next = pending[0]
		}
		if stopping {
			output <- next
			pending = pending[1:]
			continue
		}
		select {
		case queued := <-q.in:
			if queued.stop {
				stopping = true
			} else {
				pending = append(pending, queued.event)
			}
		case output <- next:
			pending = pending[1:]
		}
	}
	close(q.out)
}
