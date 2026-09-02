package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// handleApprove answers the permission a Rich Codex session is holding open.
// The decision goes to the runner as an Approve frame; the runner's own
// approval_resolved event is what clears the pending prompt, so the response
// here reports what was answered rather than a state the daemon guessed.
func (s *Server) handleApprove(response http.ResponseWriter, request *http.Request, session *state.Session, ok bool, id, corsOrigin string) {
	if !ok {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "unknown session", "id": id}, corsOrigin)
		return
	}
	var body struct {
		ID       string `json:"id,omitempty"`
		Decision string `json:"decision"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	service, supported := s.registry.(approvalService)
	if !supported {
		s.sendJSON(response, http.StatusNotImplemented, map[string]any{"error": "this daemon does not route approvals"}, corsOrigin)
		return
	}
	waiting := session.Info().PendingApproval
	info, err := service.Approve(request.Context(), id, proto.ApprovalControl{
		ID: strings.TrimSpace(body.ID), Decision: strings.TrimSpace(body.Decision),
		By: strings.TrimSpace(request.Header.Get("X-Sessions-Creator-Session")),
	})
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, state.ErrNoPendingApproval), errors.Is(err, state.ErrSessionEnded):
			status = http.StatusConflict
		case errors.Is(err, state.ErrSessionNotFound):
			status = http.StatusNotFound
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error(), "id": id}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, map[string]any{
		"ok": true, "id": info.ID, "decision": strings.TrimSpace(body.Decision), "approval": waiting,
	}, corsOrigin)
	return
}
