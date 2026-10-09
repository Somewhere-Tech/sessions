package api

import (
	"context"
	"net/http"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// sessionListLedgerWait bounds how long one listing waits for durable session
// state: the ledger's single connection, another caller's fold, and the read
// itself as far as the ledger reader honors its context. It does not bound
// the in-memory registry, per-session locks, or the runner-reality probe.
const sessionListLedgerWait = 15 * time.Second

// contextSessionLister is a runtime whose listing can fail instead of
// answering without durable state.
type contextSessionLister interface {
	ListContext(context.Context, bool) ([]state.SessionInfo, error)
}

// loadedSessionCounter counts loaded sessions without the ledger, a session
// lock, or a runner.
type loadedSessionCounter interface {
	LoadedSessionCount() int
}

// handleListSessions serves GET /api/sessions. A listing that could not read
// durable session state is a 503, never a 200 that quietly lacks the ended
// sessions, start receipts and provenance only the ledger holds.
func (s *Server) handleListSessions(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	includeExited := request.URL.Query().Get("include_exited") == "1"
	lister, ok := s.registry.(contextSessionLister)
	if !ok {
		// A runtime without durable state lists its registry, as it always did.
		s.sendJSON(response, http.StatusOK, map[string]any{"sessions": s.withStartReceipts(s.registry.List(includeExited))}, corsOrigin)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), sessionListLedgerWait)
	defer cancel()
	infos, err := lister.ListContext(ctx, includeExited)
	if err != nil {
		s.sendJSON(response, http.StatusServiceUnavailable, map[string]any{
			"error": "Sessions could not read its durable session record (" + err.Error() +
				"), so it returned no listing rather than one missing ended sessions and start receipts. Nothing was changed. Retry; if it keeps failing, run `sessions doctor`.",
			"code":   "SESSION_STATE_UNAVAILABLE",
			"action": "retry",
		}, corsOrigin)
		return
	}
	if infos == nil {
		infos = []state.SessionInfo{}
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"sessions": s.withStartReceipts(infos)}, corsOrigin)
}

// loadedSessionCount is health's sessionsLoaded: what the registry holds now,
// not durable history and not proof that any session is working.
func (s *Server) loadedSessionCount() int {
	if counter, ok := s.registry.(loadedSessionCounter); ok {
		return counter.LoadedSessionCount()
	}
	return len(s.registry.List(true))
}
