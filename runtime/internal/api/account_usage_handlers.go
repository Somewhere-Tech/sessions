package api

import (
	"context"
	"net/http"
	"time"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

// accountUsageService reads each account's allowance from its provider. A read
// that cannot finish in time is reported as such, never left out.
type accountUsageService interface {
	AccountUsage(ctx context.Context, tool, name string, refresh bool) ([]sessionruntime.AccountUsage, error)
}

// accountUsageWait is how long one request waits for provider answers before
// reporting the unanswered accounts as not answered yet.
const accountUsageWait = 20 * time.Second

// handleAccountUsageRoute serves GET /api/account-usage[?tool=&name=&refresh=1].
func (s *Server) handleAccountUsageRoute(w http.ResponseWriter, r *http.Request, origin string) bool {
	if r.URL.Path != "/api/account-usage" {
		return false
	}
	if r.Method != http.MethodGet {
		s.sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}, origin)
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	service, ok := s.registry.(accountUsageService)
	if !ok {
		s.sendJSON(w, http.StatusNotImplemented, map[string]string{"error": "update Sessions on this computer to read account usage"}, origin)
		return true
	}
	query := r.URL.Query()
	refresh := query.Get("refresh") == "1" || query.Get("refresh") == "true"
	// A refresh starts provider processes on demand; everyone else is answered
	// from readings no older than the daemon's short cache.
	if principal, ok := r.Context().Value(authPrincipalContextKey{}).(authPrincipal); refresh && (!ok || !principalMayUpdateProvider(principal)) {
		s.sendJSON(w, http.StatusForbidden, map[string]string{"error": "refreshing account usage requires a local or paired Sessions client; omit refresh to read the latest reading"}, origin)
		return true
	}
	tool := query.Get("tool")
	if tool != "" && tool != "claude" && tool != "codex" {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "tool must be claude or codex"}, origin)
		return true
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountUsageWait)
	defer cancel()
	accounts, err := service.AccountUsage(ctx, tool, query.Get("name"), refresh)
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()}, origin)
		return true
	}
	s.sendJSON(w, http.StatusOK, map[string]any{
		"accounts": accounts, "checked_at": time.Now().UnixMilli(), "ttl_seconds": 60,
	}, origin)
	return true
}
