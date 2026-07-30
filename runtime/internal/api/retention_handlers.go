package api

import (
	"context"
	"net/http"
	"time"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

type retentionService interface {
	GCClosed(context.Context, int64, bool) (sessionruntime.RetentionResult, error)
}

type explicitArchiveService interface {
	ArchiveClosed(context.Context, []string) (sessionruntime.RetentionResult, error)
}

func (s *Server) handleRetentionRoute(
	response http.ResponseWriter,
	request *http.Request,
	corsOrigin string,
) bool {
	if request.URL.Path != "/api/retention/gc" && request.URL.Path != "/api/retention/archive" {
		return false
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	if request.URL.Path == "/api/retention/archive" {
		manager, ok := s.registry.(explicitArchiveService)
		if !ok {
			s.sendJSON(response, http.StatusNotImplemented, map[string]any{
				"error": "explicit archive is unavailable on this runtime",
			}, corsOrigin)
			return true
		}
		var body struct {
			IDs []string `json:"ids"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		result, err := manager.ArchiveClosed(request.Context(), body.IDs)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, result, corsOrigin)
		return true
	}
	var body struct {
		OlderThanMS int64 `json:"older_than_ms"`
		DryRun      bool  `json:"dry_run"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	const (
		minimumRetention = time.Hour
		maximumRetention = 10 * 365 * 24 * time.Hour
	)
	if body.OlderThanMS < minimumRetention.Milliseconds() ||
		body.OlderThanMS > maximumRetention.Milliseconds() {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{
			"error": "older_than_ms must be between one hour and ten years",
		}, corsOrigin)
		return true
	}
	age := time.Duration(body.OlderThanMS) * time.Millisecond
	manager, ok := s.registry.(retentionService)
	if !ok {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{
			"error": "retention is unavailable on this runtime",
		}, corsOrigin)
		return true
	}
	result, err := manager.GCClosed(request.Context(), time.Now().Add(-age).UnixMilli(), body.DryRun)
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, result, corsOrigin)
	return true
}
