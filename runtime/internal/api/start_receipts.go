package api

import (
	"errors"
	"net/http"
	"os"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// Start receipts are projected here because this is where the first request's
// delivery receipt lives. The session layer carries the recorded operation ids
// from the ledger; this layer adds what the durable receipt and the live
// submit route know, and state.ProjectStart turns both into one phase.

// beginDeliveryInFlight marks an operation as executing on this daemon until
// the returned function runs. A pending receipt is ambiguous on disk -- it is
// what a crash leaves behind too -- so only this process can say "being
// delivered right now" rather than "unknown".
func (s *Server) beginDeliveryInFlight(operationID string) func() {
	s.deliveriesInFlight.Store(operationID, struct{}{})
	return func() { s.deliveriesInFlight.Delete(operationID) }
}

// withStartReceipts projects the start receipt of every session that has one.
// Sessions created without an operation id are returned untouched.
func (s *Server) withStartReceipts(infos []state.SessionInfo) []state.SessionInfo {
	for index := range infos {
		if infos[index].Start == nil {
			continue
		}
		projected := state.ProjectStart(infos[index], *infos[index].Start, s.startPrompt(infos[index].Start.PromptOperationID))
		infos[index].Start = &projected
	}
	return infos
}

func (s *Server) withStartReceipt(info state.SessionInfo) state.SessionInfo {
	return s.withStartReceipts([]state.SessionInfo{info})[0]
}

// startPrompt reads the first request's delivery exactly as
// GET /api/message-deliveries reports it. A missing receipt proves nothing was
// submitted under that id; an unreadable one proves nothing at all and says so
// instead of being mistaken for either.
func (s *Server) startPrompt(operationID string) *state.StartPrompt {
	if operationID == "" {
		return nil
	}
	if s.deliveries == nil {
		return &state.StartPrompt{Status: state.StartPromptUnreadable, Reason: "Delivery records are unavailable; inspect the conversation before retrying."}
	}
	if _, running := s.deliveriesInFlight.Load(operationID); running {
		return &state.StartPrompt{Status: state.StartPromptSending}
	}
	record, err := s.deliveries.Get(operationID)
	if errors.Is(err, os.ErrNotExist) {
		return &state.StartPrompt{Status: state.StartPromptNotSent, Retry: true}
	}
	if err != nil {
		return &state.StartPrompt{Status: state.StartPromptUnreadable,
			Reason: "Sessions could not read the first request's delivery receipt: " + err.Error()}
	}
	body, status := s.deliveryReceiptBody(s.reconcileLateAcceptance(record), false)
	prompt := &state.StartPrompt{Status: string(status), At: record.UpdatedAtMS}
	prompt.Acceptance, _ = body["acceptance"].(string)
	prompt.Retry, _ = body["retry"].(bool)
	prompt.Reason, _ = body["reason"].(string)
	if status == delivery.StatusAccepted && prompt.Acceptance == "" {
		// An accepted generic-terminal write has no provider boundary; keep the
		// receipt's own word rather than inventing one.
		prompt.Acceptance = "terminal"
	}
	return prompt
}

// sendCreatedSession answers a create. A replayed operation returns the
// session the first request created with 200, so a caller can tell "created
// now" (201) from "already created" without parsing the body.
func (s *Server) sendCreatedSession(response http.ResponseWriter, info state.SessionInfo, corsOrigin string) {
	status := http.StatusCreated
	if info.Start != nil && info.Start.Replayed {
		status = http.StatusOK
	}
	s.sendJSON(response, status, s.withStartReceipt(info), corsOrigin)
}

// sendStartFailure reports a create whose session exists even though the
// request failed: an operation id whose session cannot be returned as live
// (409), or a launch that failed after the session was recorded (500). Both
// carry the session id, so the caller can inspect that session instead of
// starting the same work again.
func (s *Server) sendStartFailure(response http.ResponseWriter, err error, corsOrigin string) bool {
	var replay *state.StartCreateReplayError
	if errors.As(err, &replay) {
		s.sendJSON(response, http.StatusConflict, map[string]any{
			"error": err.Error(), "operation_id": replay.OperationID, "session_id": replay.SessionID,
		}, corsOrigin)
		return true
	}
	var failed *state.StartCreateFailedError
	if !errors.As(err, &failed) {
		return false
	}
	body := map[string]any{
		"error": err.Error(), "session_id": failed.SessionID,
		"recovery": state.StartRecovery{Action: state.StartRecoveryInspect, Command: "sessions status " + failed.SessionID,
			Detail: "The session was recorded before its launch failed, and a runner may still have started. Inspect it before creating another."},
	}
	if failed.OperationID != "" {
		body["operation_id"] = failed.OperationID
	}
	s.sendJSON(response, http.StatusInternalServerError, body, corsOrigin)
	return true
}

// handleCreateSession serves POST /api/sessions. A resumed conversation that is
// live or moved elsewhere is a 409 guard, as is an operation id whose session
// exists but is no longer running; everything else is bad input.
func (s *Server) handleCreateSession(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	var body state.CreateSessionRequest
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	if err := captureCreatorHeaders(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	info, err := s.registry.Create(request.Context(), body)
	if err != nil && s.sendStartFailure(response, err, corsOrigin) {
		return
	}
	if err != nil {
		status := http.StatusBadRequest
		var live *sessionruntime.ConversationLiveError
		var moved *sessionruntime.ConversationMovedError
		if errors.As(err, &live) || errors.As(err, &moved) {
			status = http.StatusConflict
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendCreatedSession(response, info, corsOrigin)
}
