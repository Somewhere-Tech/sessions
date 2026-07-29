package api

import (
	"net/http"
	"path/filepath"
)

func (s *Server) handleBackupRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/backup/status" &&
		request.URL.Path != "/api/backup/now" &&
		request.URL.Path != "/api/backup/reload" {
		return false
	}
	if s.backups == nil {
		s.sendJSON(response, http.StatusServiceUnavailable, map[string]any{"error": "backup home is unavailable"}, corsOrigin)
		return true
	}
	switch {
	case request.URL.Path == "/api/backup/status" && request.Method == http.MethodGet:
		status, err := s.backups.Status()
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, status, corsOrigin)
		return true
	case request.URL.Path == "/api/backup/now" && request.Method == http.MethodPost:
		result, err := s.backups.Push(request.Context())
		if err != nil {
			s.sendJSON(response, http.StatusBadGateway, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, result, corsOrigin)
		return true
	case request.URL.Path == "/api/backup/reload" && request.Method == http.MethodPost:
		if err := s.backups.ReloadPeriodic(); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true}, corsOrigin)
		return true
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
}

func backupHome(userStateRoot string) (string, bool) {
	root := filepath.Clean(userStateRoot)
	if filepath.Base(root) != "sessions" {
		return "", false
	}
	stateDir := filepath.Dir(root)
	localDir := filepath.Dir(stateDir)
	if filepath.Base(stateDir) != "state" || filepath.Base(localDir) != ".local" {
		return "", false
	}
	return filepath.Dir(localDir), true
}
