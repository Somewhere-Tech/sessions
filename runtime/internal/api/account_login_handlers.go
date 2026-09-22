package api

import (
	"encoding/json"
	"net/http"
	"strings"

	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
)

type accountLoginService interface {
	StartAccountLogin(tool, name string) (sessionruntime.AccountLoginStatus, error)
	AccountLogin(id, code string, cancel bool) (sessionruntime.AccountLoginStatus, error)
}

func (s *Server) handleAccountLoginRoute(w http.ResponseWriter, r *http.Request, origin string) bool {
	const prefix = "/api/account-logins"
	if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
		return false
	}
	// Login URLs and device codes grant account access; open-access peers must
	// not start, observe, cancel or supply codes to these operations.
	principal, ok := r.Context().Value(authPrincipalContextKey{}).(authPrincipal)
	if !ok || !principalMayUpdateProvider(principal) {
		s.sendJSON(w, http.StatusForbidden, map[string]string{"error": "sign-in requires a local or paired Sessions client"}, origin)
		return true
	}
	w.Header().Set("Cache-Control", "no-store")
	service, ok := s.registry.(accountLoginService)
	if !ok {
		s.sendJSON(w, http.StatusNotImplemented, map[string]string{"error": "update Sessions on this computer to sign in"}, origin)
		return true
	}
	id := strings.TrimPrefix(r.URL.Path, prefix)
	if id == "" && r.Method == http.MethodPost {
		s.startAccountLogin(w, r, origin, service)
		return true
	}
	if !strings.HasPrefix(id, "/") || strings.Contains(id[1:], "/") || len(id) < 2 {
		s.sendJSON(w, http.StatusNotFound, map[string]string{"error": "unknown sign-in"}, origin)
		return true
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodDelete {
		s.sendJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"}, origin)
		return true
	}
	var body struct {
		Code string `json:"code"`
	}
	if r.Method == http.MethodPost && json.NewDecoder(r.Body).Decode(&body) != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "paste the provider confirmation code"}, origin)
		return true
	}
	status, err := service.AccountLogin(id[1:], strings.TrimSpace(body.Code), r.Method == http.MethodDelete)
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()}, origin)
	} else {
		s.sendJSON(w, http.StatusOK, status, origin)
	}
	return true
}

func (s *Server) startAccountLogin(w http.ResponseWriter, r *http.Request, origin string, service accountLoginService) {
	var body struct {
		Tool    string `json:"tool"`
		Profile string `json:"profile"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": "choose an account to sign in"}, origin)
		return
	}
	status, err := service.StartAccountLogin(body.Tool, body.Profile)
	if err != nil {
		s.sendJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()}, origin)
		return
	}
	s.sendJSON(w, http.StatusOK, status, origin)
}
