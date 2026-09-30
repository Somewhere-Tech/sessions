package api

import (
	"context"
	"sync"
)

// sessionMutexes hands out one mutex per session id.
//
// A submit is two writes — the message text and the Enter that sends it — and
// nothing else may write to THAT session in between, or one agent's Enter
// commits another agent's half-typed line. That invariant is per session, not
// per daemon: a single process-wide mutex also made ten agents submitting to
// ten different sessions wait in line behind each other's settle delay, and let
// one busy mux client stall every other client's HTTP submit.
//
// The map is bounded by the number of writes in flight, not by the number of
// sessions the daemon has ever seen: an entry is created when the first caller
// asks for it and removed when the last one releases it, so a session that
// finishes submitting leaves nothing behind.
type sessionMutexes struct {
	mu      sync.Mutex
	entries map[string]*sessionMutex
}

type sessionMutex struct {
	token chan struct{}
	// users counts holders plus waiters, so the entry survives handoff
	// between two concurrent submits to the same session and is deleted only
	// once nobody is interested in it.
	users int
}

func newSessionMutexes() *sessionMutexes {
	return &sessionMutexes{entries: make(map[string]*sessionMutex)}
}

// lock blocks until this session's mutex is held and returns the release
// function. The release function must be called exactly once.
func (m *sessionMutexes) lock(id string) func() {
	unlock, _ := m.lockContext(context.Background(), id)
	return unlock
}

// lockContext lets a disconnected socket abandon a pending session write
// without leaving a waiter behind or later typing after its connection ends.
func (m *sessionMutexes) lockContext(ctx context.Context, id string) (func(), error) {
	m.mu.Lock()
	entry, exists := m.entries[id]
	if !exists {
		entry = &sessionMutex{token: make(chan struct{}, 1)}
		entry.token <- struct{}{}
		m.entries[id] = entry
	}
	entry.users++
	m.mu.Unlock()

	select {
	case <-ctx.Done():
		m.releaseReference(id, entry)
		return nil, ctx.Err()
	case <-entry.token:
	}
	var once sync.Once
	unlock := func() { once.Do(func() { entry.token <- struct{}{}; m.releaseReference(id, entry) }) }
	if err := ctx.Err(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (m *sessionMutexes) releaseReference(id string, entry *sessionMutex) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry.users--
	if entry.users == 0 {
		delete(m.entries, id)
	}
}

// tracked reports how many per-session mutexes are currently retained. It
// exists so tests can prove the map does not grow with the number of sessions
// that have ever submitted.
func (m *sessionMutexes) tracked() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}
