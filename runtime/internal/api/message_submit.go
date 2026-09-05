package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

type messageSubmitService interface {
	SubmitMessage(context.Context, string, proto.MessageControl, state.InputAttribution) (proto.MessageResult, error)
}

func (s *Server) handleSubmitControl(response http.ResponseWriter, request *http.Request, info state.SessionInfo, data, operationID, mode, corsOrigin string, attribution state.InputAttribution) bool {
	if info.MessageSubmit {
		s.submitStructuredMessage(response, request, info.ID, data, operationID, mode, corsOrigin, attribution)
		return true
	}
	reason, blocked := semanticSubmitRefusal(info)
	if mode != "" && mode != "auto" {
		reason, blocked = "Steer now requires an updated structured Codex runner. This message was not sent.", true
	}
	if !blocked {
		return false
	}
	record, err := s.deliveries.Complete(operationID, delivery.StatusNotDelivered, false, true, reason)
	if err != nil {
		s.sendInputError(response, err, corsOrigin)
	} else {
		s.sendDeliveryRecord(response, record, false, corsOrigin)
	}
	return true
}

func (s *Server) submitStructuredMessage(response http.ResponseWriter, request *http.Request, id, data, operationID, mode, corsOrigin string, attribution state.InputAttribution) {
	service, ok := s.registry.(messageSubmitService)
	control := proto.MessageControl{OperationID: operationID, Text: composerMessageText(data), Mode: mode}
	status, delivered, retry, reason, acceptance := delivery.StatusNotDelivered, false, true, "", ""
	if err := proto.ValidateMessage(control); err != nil {
		reason = err.Error()
	} else if !ok {
		reason = "acknowledged message control is unavailable"
	} else {
		result, err := service.SubmitMessage(request.Context(), id, control, attribution)
		reason, acceptance = result.Error, result.Boundary
		if err != nil || result.Boundary == "unknown" {
			status, retry = delivery.StatusUnknown, false
			if err != nil {
				reason = err.Error()
			}
		} else if result.Accepted {
			status, delivered, retry = delivery.StatusAccepted, true, false
		}
	}
	record, err := s.deliveries.Complete(operationID, status, delivered, retry, reason, acceptance)
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": "could not record message outcome; do not automatically resend: " + err.Error(), "operation_id": operationID}, corsOrigin)
		return
	}
	s.sendDeliveryRecord(response, record, false, corsOrigin)
}

func composerMessageText(data string) string {
	// Strip only the legacy composer's outer paste envelope. Embedded CRs
	// and escape sequences are text, never additional submit/interrupt keys.
	if strings.HasPrefix(data, "\x1b[200~") && strings.HasSuffix(data, "\x1b[201~") {
		return strings.TrimSuffix(strings.TrimPrefix(data, "\x1b[200~"), "\x1b[201~")
	}
	return data
}
