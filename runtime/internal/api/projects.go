package api

import (
	"net/http"
	"sort"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/project"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// projectView is one section of the inbox: a stored project or an implicit one
// derived from a folder nobody has named yet, plus the ids of the sessions
// that belong to it. Sessions carry their own state; the view only says which
// project each one falls under.
type projectView struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Implicit   bool     `json:"implicit"`
	Roots      []string `json:"roots"`
	GitHub     string   `json:"github,omitempty"`
	Somewhere  string   `json:"somewhere,omitempty"`
	Pinned     bool     `json:"pinned,omitempty"`
	SessionIDs []string `json:"session_ids"`
	Live       int      `json:"live"`
	NeedsInput int      `json:"needs_input"`
	UpdatedAt  int64    `json:"updated_at"`
}

// projectViews groups sessions by resolved project. Stored projects appear
// even when they have no sessions right now; implicit ones appear only while
// a session sits in their folder.
func projectViews(store *project.Store, sessions []state.SessionInfo) ([]projectView, error) {
	stored, err := store.List()
	if err != nil {
		return nil, err
	}
	views := make(map[string]*projectView, len(stored)+8)
	order := make([]string, 0, len(stored)+8)
	for _, item := range stored {
		views[item.ID] = &projectView{
			ID: item.ID, Name: item.Name, Roots: item.Roots, GitHub: item.GitHub,
			Somewhere: item.Somewhere, Pinned: item.Pinned, SessionIDs: []string{}, UpdatedAt: item.UpdatedAt,
		}
		order = append(order, item.ID)
	}
	for _, info := range sessions {
		if strings.TrimSpace(info.Cwd) == "" {
			continue
		}
		resolved, err := store.Resolve(info.Cwd)
		if err != nil {
			return nil, err
		}
		view := views[resolved.ProjectID]
		if view == nil {
			view = &projectView{
				ID: resolved.ProjectID, Name: resolved.Name, Implicit: true,
				Roots: []string{resolved.TopLevel}, GitHub: resolved.GitHub, Somewhere: resolved.Somewhere,
				SessionIDs: []string{},
			}
			views[resolved.ProjectID] = view
			order = append(order, resolved.ProjectID)
		}
		view.SessionIDs = append(view.SessionIDs, info.ID)
		if !info.Exited {
			view.Live++
			if info.IdleReason == state.IdleReasonNeedsInput {
				view.NeedsInput++
			}
		}
		if info.LastDataAt > view.UpdatedAt {
			view.UpdatedAt = info.LastDataAt
		}
	}
	result := make([]projectView, 0, len(order))
	for _, id := range order {
		result = append(result, *views[id])
	}
	sort.SliceStable(result, func(i, j int) bool {
		if result[i].Pinned != result[j].Pinned {
			return result[i].Pinned
		}
		if result[i].Implicit != result[j].Implicit {
			return !result[i].Implicit
		}
		return result[i].UpdatedAt > result[j].UpdatedAt
	})
	return result, nil
}

// handleProjectsRoute serves the project surface:
//
//	GET    /api/projects                 stored and implicit projects with their sessions
//	GET    /api/projects/suggest?cwd=P   what "Name this project" should start from
//	PUT    /api/projects                 create or update a stored project (JSON body)
//	DELETE /api/projects/{id}            forget a stored project; its sessions become implicit
func (s *Server) handleProjectsRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	path := request.URL.Path
	if path != "/api/projects" && !strings.HasPrefix(path, "/api/projects/") {
		return false
	}
	if s.projects == nil {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "projects are not available on this runtime"}, corsOrigin)
		return true
	}
	switch {
	case path == "/api/projects" && request.Method == http.MethodGet:
		views, err := projectViews(s.projects, s.registry.List(true))
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"projects": views}, corsOrigin)
	case path == "/api/projects/suggest" && request.Method == http.MethodGet:
		cwd := strings.TrimSpace(request.URL.Query().Get("cwd"))
		if cwd == "" {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "cwd is required"}, corsOrigin)
			return true
		}
		suggestion, err := s.projects.Suggest(cwd)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": "Sessions could not suggest this project: " + err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, suggestion, corsOrigin)
	case path == "/api/projects" && request.Method == http.MethodPut:
		var body project.Project
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		saved, err := s.projects.Upsert(body)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, saved, corsOrigin)
	case strings.HasPrefix(path, "/api/projects/") && request.Method == http.MethodDelete:
		id := strings.TrimPrefix(path, "/api/projects/")
		if id == "" || strings.Contains(id, "/") {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": path}, corsOrigin)
			return true
		}
		if err := s.projects.Delete(id); err != nil {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true}, corsOrigin)
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed", "path": path}, corsOrigin)
	}
	return true
}
