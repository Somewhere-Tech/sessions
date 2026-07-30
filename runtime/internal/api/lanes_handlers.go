package api

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"unicode"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type laneView struct {
	state.SessionInfo
	Manifest *state.CompletionManifest `json:"manifest,omitempty"`
}

func (s *Server) handleLanesRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path == "/api/lanes" {
		switch request.Method {
		case http.MethodGet:
			listed := s.registry.List(true)
			lanes := make([]laneView, 0, len(listed))
			for _, info := range listed {
				if info.Kind != state.KindLane {
					continue
				}
				view := laneView{SessionInfo: info}
				if manifest, err := state.ReadCompletionManifest(state.For(s.config.RunnerStateDir, info.ID).Manifest); err == nil {
					view.Manifest = &manifest
				}
				lanes = append(lanes, view)
			}
			userCreatorID, err := ledger.LocalUserCreatorID()
			if err != nil {
				s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
				return true
			}
			s.sendJSON(response, http.StatusOK, map[string]any{
				"lanes": lanes, "user_creator_id": userCreatorID,
			}, corsOrigin)
			return true
		case http.MethodPost:
			var body state.CreateSessionRequest
			if err := readJSON(request, &body); err != nil {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
				return true
			}
			if err := captureCreatorHeaders(request, &body); err != nil {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
				return true
			}
			if body.Kind != "" && body.Kind != state.KindLane {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "lane kind must be \"lane\""}, corsOrigin)
				return true
			}
			body.Kind = state.KindLane
			info, err := s.registry.Create(request.Context(), body)
			if err != nil {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
				return true
			}
			s.sendJSON(response, http.StatusCreated, info, corsOrigin)
			return true
		default:
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
	}

	const prefix = "/api/lanes/"
	if !strings.HasPrefix(request.URL.Path, prefix) {
		return false
	}
	rest := strings.TrimPrefix(request.URL.Path, prefix)
	id, suffix, found := strings.Cut(rest, "/")
	if !found || suffix != "manifest" || request.Method != http.MethodGet || !validLaneID(id) {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": request.URL.Path}, corsOrigin)
		return true
	}
	if session, ok := s.registry.Get(id); ok && session.Info().Kind == state.KindLane && !session.Info().Exited {
		s.sendJSON(response, http.StatusConflict, map[string]any{"error": "lane is still running", "id": id}, corsOrigin)
		return true
	}
	manifest, err := state.ReadCompletionManifest(state.For(s.config.RunnerStateDir, id).Manifest)
	if err == nil {
		s.sendJSON(response, http.StatusOK, manifest, corsOrigin)
		return true
	}
	if !errors.Is(err, os.ErrNotExist) {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown lane", "id": id}, corsOrigin)
	return true
}

func validLaneID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	for index, value := range id {
		if index == 8 || index == 13 || index == 18 || index == 23 {
			continue
		}
		if !unicode.IsDigit(value) && (value < 'a' || value > 'f') && (value < 'A' || value > 'F') {
			return false
		}
	}
	return true
}
