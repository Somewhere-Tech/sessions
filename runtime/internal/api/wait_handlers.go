package api

import (
	"net/http"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// handleWaitRoute exposes only the daemon facts needed by observational
// waits. "structured" means the Codex/Claude event classifier supplied the
// working flag; "heuristic" means it came from raw terminal activity. Neither
// label claims knowledge beyond that evidence.
func (s *Server) handleWaitRoute(response http.ResponseWriter, request *http.Request, id, suffix, corsOrigin string) bool {
	if (suffix != "/wait" && suffix != "/wait-state") || request.Method != http.MethodGet {
		return false
	}
	session, ok := s.registry.Get(id)
	if !ok {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown session", "id": id}, corsOrigin)
		return true
	}
	info := session.Info()
	if info.Exited {
		s.sendJSON(response, http.StatusConflict, map[string]any{"error": "session exited", "id": id}, corsOrigin)
		return true
	}
	source := "heuristic"
	if info.Tool == state.ToolClaude || info.Tool == state.ToolCodex {
		source = "structured"
	}
	s.sendJSON(response, http.StatusOK, map[string]any{
		"session": info.ID,
		"cwd":     info.Cwd,
		"working": info.Working,
		"source":  source,
	}, corsOrigin)
	return true
}
