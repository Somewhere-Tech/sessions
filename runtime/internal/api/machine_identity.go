package api

import (
	"net/http"
	"strings"
)

func (s *Server) handleMachineIdentity(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	if s.identityError != nil || s.identity.ID == "" {
		detail := "machine identity is unavailable"
		if s.identityError != nil {
			detail = s.identityError.Error()
		}
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": detail}, corsOrigin)
		return
	}
	body := map[string]any{"machine_id": s.identity.ID, "name": s.identity.Name}
	principal, ok := request.Context().Value(authPrincipalContextKey{}).(authPrincipal)
	// Only the authenticated caller's paired identity is returned. Administrator
	// and loopback access must not acquire a fabricated pairing identity.
	if ok && !principal.Local && strings.HasPrefix(principal.ID, "device:") {
		body["device_id"] = strings.TrimPrefix(principal.ID, "device:")
	}
	s.sendJSON(response, http.StatusOK, body, corsOrigin)
}
