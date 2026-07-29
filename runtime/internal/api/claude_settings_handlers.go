package api

import (
	"net/http"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func (s *Server) handleClaudeSettingsRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/claude/settings" {
		return false
	}
	switch request.Method {
	case http.MethodGet:
		settings, err := s.loadClaudeSettings()
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, settings, corsOrigin)
	case http.MethodPut:
		var requested state.ClaudeSettings
		if err := readJSON(request, &requested); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		normalized, err := state.NormalizeClaudeSettings(requested)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		if err := state.UpdateSettings(s.lan.settingsPath, func(settings *state.Settings) error {
			settings.Claude = &normalized
			return nil
		}); err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, normalized, corsOrigin)
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
	}
	return true
}

func (s *Server) loadClaudeSettings() (state.ClaudeSettings, error) {
	settings, err := state.LoadSettings(s.lan.settingsPath)
	if err != nil {
		return state.ClaudeSettings{}, err
	}
	return state.NormalizeClaudeSettings(settings.EffectiveClaude())
}
