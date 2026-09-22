package api

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

func (s *Server) handleHistorySource(response http.ResponseWriter, request *http.Request, id, corsOrigin string) {
	source, err := s.integrationEndpoints.Source(s.registry.List(true), id)
	if errors.Is(err, integrations.ErrHistoryNotFound) {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "history session not found", "id": id}, corsOrigin)
		return
	}
	if err != nil {
		s.integrationError(response, corsOrigin, "history source lookup failed", err)
		return
	}
	s.sendJSON(response, http.StatusOK, source, corsOrigin)
}

func (s *Server) handleConversationRead(response http.ResponseWriter, request *http.Request, id, corsOrigin string) {
	limit := 20
	if raw := request.URL.Query().Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "limit must be between 1 and 100"}, corsOrigin)
			return
		}
		limit = value
	}
	page, err := s.integrationEndpoints.ReadConversation(request.Context(), s.registry.List(true), id, request.URL.Query().Get("cursor"), limit)
	if err == nil {
		s.sendJSON(response, http.StatusOK, page, corsOrigin)
		return
	}
	status, code := http.StatusInternalServerError, "CONVERSATION_READ_FAILED"
	switch {
	case errors.Is(err, integrations.ErrReadCursor):
		status, code = http.StatusBadRequest, "INVALID_CURSOR"
	case errors.Is(err, integrations.ErrReadChanged):
		status, code = http.StatusConflict, "HISTORY_CHANGED"
	case errors.Is(err, integrations.ErrHistoryNotFound):
		status, code = http.StatusNotFound, "CONVERSATION_UNAVAILABLE"
	case errors.Is(err, integrations.ErrReadRecordLarge):
		status, code = http.StatusRequestEntityTooLarge, "RECORD_TOO_LARGE"
	}
	s.sendJSON(response, status, map[string]any{"error": err.Error(), "code": code, "id": id}, corsOrigin)
}
