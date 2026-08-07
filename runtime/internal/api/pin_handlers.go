package api

import (
	"errors"
	"net/http"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// handlePinRoute exposes the user's workbench mark. A pinned session sorts
// first in every listing and the automatic machinery leaves it alone; the
// runner is not started, stopped, or otherwise touched by this route.
//
// It is its own route family rather than another branch of handleSessionRoute
// so that a wrong verb answers 405 like /wait does. Falling through to the
// session router's catch-all 404 would tell a caller its session was gone when
// only its method was wrong.
func (s *Server) handlePinRoute(response http.ResponseWriter, request *http.Request, id, suffix, corsOrigin string) bool {
	if suffix != "/pin" {
		return false
	}
	if request.Method != http.MethodPut {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	var body struct {
		Pinned bool `json:"pinned"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	pins, supported := s.registry.(interface {
		UpdatePinned(string, bool) (bool, error)
	})
	if !supported {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "session pinning is not available on this runtime"}, corsOrigin)
		return true
	}
	pinned, err := pins.UpdatePinned(id, body.Pinned)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, state.ErrSessionNotFound):
			status = http.StatusNotFound
		case errors.Is(err, state.ErrSessionEnded):
			status = http.StatusConflict
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"pinned": pinned}, corsOrigin)
	return true
}
