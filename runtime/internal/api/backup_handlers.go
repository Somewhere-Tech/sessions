package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
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

// backupHome resolves the home directory whose Sessions configuration owns
// backup settings.
//
// The home comes from the operating system rather than being reverse-derived
// from the state root's spelling. The previous derivation asserted the literal
// Unix ".../.local/state/sessions" shape, so on Windows — where the user state
// root ends in "state" — it always reported false, server.backups was never
// constructed, and every backup route answered 503 with no explanation.
//
// What remains is a containment check: this daemon reads and writes backup
// configuration for a home only when its state root is that home's state root,
// either by living inside the home or by being exactly the platform location
// for it (a redirected %LOCALAPPDATA% can sit outside the profile directory).
func backupHome(userStateRoot string) (string, bool) {
	root := strings.TrimSpace(userStateRoot)
	if root == "" {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	home = filepath.Clean(home)
	if home == "" || home == "." {
		return "", false
	}
	root = filepath.Clean(root)
	if root == filepath.Clean(state.UserStateRootFor(home)) {
		return home, true
	}
	if !pathWithinBase(canonicalPath(root), canonicalPath(home)) {
		return "", false
	}
	return home, true
}
