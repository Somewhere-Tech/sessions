package api

import (
	"context"
	"net/http"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

type worktreeService interface {
	Worktrees(context.Context) ([]sessionruntime.WorktreeStatus, error)
	CleanWorktrees(context.Context, bool) ([]sessionruntime.WorktreeCleanResult, error)
}

func (s *Server) handleWorktreesRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/worktrees" && request.URL.Path != "/api/worktrees/clean" {
		return false
	}
	service, ok := s.registry.(worktreeService)
	if !ok {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "worktree management is unavailable"}, corsOrigin)
		return true
	}
	if request.URL.Path == "/api/worktrees" && request.Method == http.MethodGet {
		worktrees, err := service.Worktrees(request.Context())
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"worktrees": worktrees}, corsOrigin)
		return true
	}
	if request.URL.Path == "/api/worktrees/clean" && request.Method == http.MethodPost {
		var body struct {
			DryRun bool `json:"dry_run"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		results, err := service.CleanWorktrees(request.Context(), body.DryRun)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"results": results, "dry_run": body.DryRun}, corsOrigin)
		return true
	}
	return false
}
