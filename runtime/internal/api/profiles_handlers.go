package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

type profileService interface {
	Profiles(context.Context) ([]sessionruntime.ProfileStatus, error)
}

// accountService registers and unregisters the named provider homes a second
// subscription lives in. Creating one is a directory and a label; the login
// itself happens through a bounded provider-owned authentication helper.
type accountService interface {
	CreateAccount(tool, name, label string) (sessionruntime.ProfileStatus, error)
	ForgetAccount(tool, name string) error
	RenameAccount(tool, name, label string) (sessionruntime.ProfileStatus, error)
	AccountHomePath(tool, name string) string
}

func (s *Server) handleProfilesRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if s.handleAccountLoginRoute(response, request, corsOrigin) {
		return true
	}
	if strings.HasPrefix(request.URL.Path, "/api/profiles/") {
		return s.handleAccountRoute(response, request, corsOrigin)
	}
	if request.URL.Path != "/api/profiles" {
		return false
	}
	if request.Method == http.MethodPost {
		s.handleCreateAccount(response, request, corsOrigin)
		return true
	}
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	service, ok := s.registry.(profileService)
	if !ok {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "profile listing is unavailable"}, corsOrigin)
		return true
	}
	profiles, err := service.Profiles(request.Context())
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"profiles": profiles}, corsOrigin)
	return true
}

func (s *Server) handleCreateAccount(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	service, ok := s.registry.(accountService)
	if !ok {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "accounts are unavailable"}, corsOrigin)
		return
	}
	var body struct {
		Tool  string `json:"tool"`
		Name  string `json:"name"`
		Label string `json:"label"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "invalid account request"}, corsOrigin)
		return
	}
	profile, err := service.CreateAccount(strings.TrimSpace(body.Tool), strings.TrimSpace(body.Name), strings.TrimSpace(body.Label))
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"profile": profile}, corsOrigin)
}

// handleAccountRoute serves /api/profiles/<tool>/<name>.
func (s *Server) handleAccountRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/api/profiles/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown account"}, corsOrigin)
		return true
	}
	if request.Method != http.MethodDelete && request.Method != http.MethodPut {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	service, ok := s.registry.(accountService)
	if !ok {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "accounts are unavailable"}, corsOrigin)
		return true
	}
	if request.Method == http.MethodPut {
		s.handleRenameAccount(response, request, corsOrigin, service, parts[0], parts[1])
		return true
	}
	if err := service.ForgetAccount(parts[0], parts[1]); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	// The home is deliberately left in place: it holds a real subscription's
	// login and history, and unregistering is a decision about a list.
	s.sendJSON(response, http.StatusOK, map[string]any{
		"ok":        true,
		"forgotten": parts[0] + "/" + parts[1],
		"home":      service.AccountHomePath(parts[0], parts[1]),
		"note":      "the provider home was left in place for manual review",
	}, corsOrigin)
	return true
}

// handleRenameAccount serves PUT /api/profiles/<tool>/<name>: a new nickname
// for an existing account, and nothing else about it.
func (s *Server) handleRenameAccount(
	response http.ResponseWriter, request *http.Request, corsOrigin string,
	service accountService, tool, name string,
) {
	principal, ok := request.Context().Value(authPrincipalContextKey{}).(authPrincipal)
	if !ok || !principalMayUpdateProvider(principal) {
		s.sendJSON(response, http.StatusForbidden, map[string]any{"error": "renaming an account requires a local or paired Sessions client"}, corsOrigin)
		return
	}
	var body struct {
		Label *string `json:"label"`
	}
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.Label == nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": `send {"label":"<nickname>"}; an empty label clears it`}, corsOrigin)
		return
	}
	profile, err := service.RenameAccount(tool, name, strings.TrimSpace(*body.Label))
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, map[string]any{"profile": profile}, corsOrigin)
}
