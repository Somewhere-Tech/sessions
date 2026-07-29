package api

import (
	"errors"
	"net/http"

	"github.com/somewhere-tech/sessions/runtime/internal/verdict"
)

func (s *Server) handleVerdictRoute(
	response http.ResponseWriter,
	request *http.Request,
	id, suffix, corsOrigin string,
) bool {
	if suffix != "/verdict" {
		return false
	}
	if err := verdict.ValidateID(id); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	store, err := verdict.NewStore(verdict.Options{StateDir: s.config.RunnerStateDir})
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	switch request.Method {
	case http.MethodGet:
		latest, err := store.Latest(id)
		if errors.Is(err, verdict.ErrNotFound) {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "no verdict", "id": id}, corsOrigin)
			return true
		}
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, latest, corsOrigin)
		return true
	case http.MethodPost:
		reader := http.MaxBytesReader(response, request.Body, maxJSONBody)
		document, err := verdict.Decode(reader)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		record, err := store.Emit(request.Context(), id, document)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusCreated, record, corsOrigin)
		return true
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
}
