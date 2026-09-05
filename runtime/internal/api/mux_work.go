package api

import (
	"context"
	"log"
	"sync"
)

const (
	muxWorkSessions = 8
	muxWorkMessages = 64
	muxWorkBytes    = 1024 * 1024
)

// muxWork owns a fixed worker pool with per-session FIFO scheduling for one
// connection. Admission never blocks the receive loop or starts a goroutine
// for each message. The pool starts lazily on the first admitted command.
// A command here is transport work, not evidence of provider queue acceptance.
type muxWork struct {
	ctx          context.Context
	cancel       context.CancelFunc
	execute      func(context.Context, clientMessage)
	mu           sync.Mutex
	wg           sync.WaitGroup
	closed       bool
	started      bool
	ready        chan string
	sessions     map[string][]clientMessage
	count, bytes int // includes executing commands
}

func newMuxWork(ctx context.Context, execute func(context.Context, clientMessage)) *muxWork {
	ctx, cancel := context.WithCancel(ctx)
	return &muxWork{ctx: ctx, cancel: cancel, execute: execute, sessions: make(map[string][]clientMessage), ready: make(chan string, muxWorkSessions)}
}

func muxMessageBytes(message clientMessage) int {
	return len(message.Type) + len(message.SessionID) + len(message.RequestID) + len(message.Data)
}

func (q *muxWork) enqueue(message clientMessage) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, exists := q.sessions[message.SessionID]
	if q.closed || q.ctx.Err() != nil || q.count >= muxWorkMessages ||
		q.bytes+muxMessageBytes(message) > muxWorkBytes || (!exists && len(q.sessions) >= muxWorkSessions) {
		return false
	}
	q.sessions[message.SessionID] = append(q.sessions[message.SessionID], message)
	q.count++
	q.bytes += muxMessageBytes(message)
	if !q.started {
		q.started = true
		for range muxWorkSessions {
			q.wg.Add(1)
			go q.run()
		}
	}
	if !exists {
		q.ready <- message.SessionID // at most one token per admitted session
	}
	return true
}

func (q *muxWork) run() {
	defer q.wg.Done()
	for {
		var id string
		select {
		case <-q.ctx.Done():
			return
		case id = <-q.ready:
		}
		q.mu.Lock()
		pending := q.sessions[id]
		if q.ctx.Err() != nil {
			q.mu.Unlock()
			return
		}
		message := pending[0]
		pending[0] = clientMessage{}
		q.sessions[id] = pending[1:]
		q.mu.Unlock()
		if q.ctx.Err() == nil {
			q.execute(q.ctx, message)
		}
		q.mu.Lock()
		q.count--
		q.bytes -= muxMessageBytes(message)
		if len(q.sessions[id]) == 0 {
			delete(q.sessions, id)
		} else if q.ctx.Err() == nil {
			q.ready <- id
		}
		q.mu.Unlock()
	}
}

func (q *muxWork) close() {
	q.mu.Lock()
	q.closed = true
	q.cancel()
	q.mu.Unlock()
	q.wg.Wait()
	q.mu.Lock()
	q.sessions = make(map[string][]clientMessage)
	q.count, q.bytes = 0, 0
	q.mu.Unlock()
}

func (s *Server) handleMuxWork(ctx context.Context, peer *wsPeer, message clientMessage, cancel context.CancelFunc) {
	// Work previously ran inside net/http's panic boundary. Moving it to a
	// worker must not turn a bad runner operation into a daemon-wide crash.
	defer func() {
		if failure := recover(); failure != nil {
			log.Printf("[ws] mux %s failed unexpectedly; closing connection (%T)", message.Type, failure)
			cancel()
			_ = peer.connection.CloseNow()
		}
	}()
	written, reason := s.applyMuxWork(ctx, message)
	if ctx.Err() != nil || message.RequestID == "" || message.Type == "resize" {
		return
	}
	_ = peer.send(ctx, map[string]any{"type": message.Type + "Ack", "requestId": message.RequestID,
		"sessionId": message.SessionID, "ok": written, "reason": reason})
}

func (s *Server) applyMuxWork(ctx context.Context, message clientMessage) (bool, string) {
	unlock, err := s.submits.lockContext(ctx, message.SessionID)
	if err != nil {
		return false, err.Error()
	}
	defer unlock()
	if ctx.Err() != nil {
		return false, ctx.Err().Error()
	}
	switch message.Type {
	case "submit":
		return s.submitMuxInput(ctx, message.SessionID, message.Data)
	case "input":
		return s.registry.Input(ctx, message.SessionID, message.Data), ""
	case "resize":
		if session, ok := s.registry.Get(message.SessionID); ok {
			return session.Resize(ctx, clampDimension(message.Cols, 40, 500), clampDimension(message.Rows, 10, 200)), ""
		}
	}
	return false, "session input is unavailable"
}

func rejectMuxWork(ctx context.Context, peer *wsPeer, message clientMessage) {
	const reason = "This connection has too much pending input. This command was not sent; wait for pending work to finish before trying again."
	if message.RequestID != "" && message.Type != "resize" {
		_ = peer.send(ctx, map[string]any{"type": message.Type + "Ack", "requestId": message.RequestID,
			"sessionId": message.SessionID, "ok": false, "reason": reason})
		return
	}
	_ = peer.send(ctx, map[string]any{"type": "error", "code": "input_overloaded", "message": reason, "sessionId": message.SessionID})
}
