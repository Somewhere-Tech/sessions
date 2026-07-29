package api

import (
	"context"
	"net/http"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

type profileService interface {
	Profiles(context.Context) ([]sessionruntime.ProfileStatus, error)
}

func (s *Server) handleProfilesRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/profiles" || request.Method != http.MethodGet {
		return false
	}
	service, ok := s.registry.(profileService)
	if !ok {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "profile listing is unavailable"}, corsOrigin)
		return true
	}
	profiles, err := service.Profiles(request.Context())
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"profiles": profiles}, corsOrigin)
	return true
}
