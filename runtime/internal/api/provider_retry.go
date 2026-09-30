package api

import (
	"errors"
	"net/http"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func (s *Server) handleRichControlRoute(
	response http.ResponseWriter,
	request *http.Request,
	id, suffix, corsOrigin string,
) bool {
	return s.handleProviderRetryRoute(response, request, id, suffix, corsOrigin) ||
		s.handleVerdictRoute(response, request, id, suffix, corsOrigin)
}

func (s *Server) handleProviderRetryRoute(
	response http.ResponseWriter,
	request *http.Request,
	id, suffix, corsOrigin string,
) bool {
	if suffix != "/retry" && suffix != "/retry/stop" {
		return false
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	service, ok := s.registry.(providerRetryService)
	if !ok {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "provider retry is unavailable on this runtime"}, corsOrigin)
		return true
	}
	if suffix == "/retry/stop" {
		err := service.StopProviderRetry(request.Context(), id)
		if err != nil {
			s.sendProviderRetryError(response, err, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusNoContent, nil, corsOrigin)
		return true
	}
	info, err := service.RetryProvider(request.Context(), id)
	if err != nil {
		s.sendProviderRetryError(response, err, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, info, corsOrigin)
	return true
}

func (s *Server) sendProviderRetryError(response http.ResponseWriter, err error, corsOrigin string) {
	status := http.StatusBadGateway
	switch {
	case errors.Is(err, state.ErrSessionNotFound):
		status = http.StatusNotFound
	case errors.Is(err, state.ErrRetryUnsupported),
		errors.Is(err, state.ErrNoFailedTurn),
		errors.Is(err, state.ErrNoRetryScheduled),
		errors.Is(err, state.ErrSessionWorking),
		errors.Is(err, state.ErrSessionEnded),
		errors.Is(err, state.ErrRunnerProtocol):
		status = http.StatusConflict
	}
	s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
}
