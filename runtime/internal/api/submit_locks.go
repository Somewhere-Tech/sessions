package api

import "sync"

// sessionMutexes hands out one mutex per session id.
//
// A submit is two writes — the message text and the Enter that sends it — and
// nothing else may write to THAT session in between, or one agent's Enter
// commits another agent's half-typed line. That invariant is per session, not
// per daemon: a single process-wide mutex also made ten agents submitting to
// ten different sessions wait in line behind each other's settle delay, and let
// one busy mux client stall every other client's HTTP submit.
//
// The map is bounded by the number of submits in flight, not by the number of
// sessions the daemon has ever seen: an entry is created when the first caller
// asks for it and removed when the last one releases it, so a session that
// finishes submitting leaves nothing behind.
type sessionMutexes struct {
	mu      sync.Mutex
	entries map[string]*sessionMutex
}

type sessionMutex struct {
	mu sync.Mutex
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
	m.mu.Lock()
	entry, exists := m.entries[id]
	if !exists {
		entry = &sessionMutex{}
		m.entries[id] = entry
	}
	entry.users++
	m.mu.Unlock()

	entry.mu.Lock()
	released := false
	return func() {
		if released {
			return
		}
		released = true
		entry.mu.Unlock()
		m.mu.Lock()
		entry.users--
		if entry.users == 0 {
			delete(m.entries, id)
		}
		m.mu.Unlock()
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
