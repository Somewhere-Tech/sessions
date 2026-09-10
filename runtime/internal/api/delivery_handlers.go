package api

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
)

const deliveryRoutePrefix = "/api/message-deliveries/"

func (s *Server) handleDeliveryRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if !strings.HasPrefix(request.URL.Path, deliveryRoutePrefix) {
		return false
	}
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	operationID := strings.TrimPrefix(request.URL.Path, deliveryRoutePrefix)
	if operationID == "" || strings.Contains(operationID, "/") {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "invalid delivery operation id"}, corsOrigin)
		return true
	}
	record, err := s.deliveries.Get(operationID)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error(), "operation_id": operationID}, corsOrigin)
		return true
	}
	s.sendDeliveryRecord(response, s.reconcileLateAcceptance(record), true, corsOrigin)
	return true
}

// reconcileLateAcceptance settles an operation whose acknowledgment reached this
// daemon after the request that started it was already gone. The runner keeps
// its own answer, correlated by the durable operation id, so asking again can
// recover it instead of leaving a message that was genuinely delivered as
// permanent uncertainty. It only resolves unknown into accepted: a late refusal
// stays unknown rather than becoming an after-the-fact invitation to resend.
func (s *Server) reconcileLateAcceptance(record delivery.Record) delivery.Record {
	if record.Status != delivery.StatusUnknown || record.Acceptance != "" {
		return record
	}
	current, ok := s.registry.Get(record.SessionID)
	if !ok {
		return record
	}
	result, answered := current.LateMessageResult(record.OperationID)
	if !answered || !result.Accepted || result.Boundary == "" {
		return record
	}
	confirmed, err := s.deliveries.ConfirmAccepted(record.OperationID, result.Boundary,
		"the runner acknowledged this operation after the first caller stopped waiting")
	if err != nil {
		return record
	}
	return confirmed
}

func (s *Server) sendDeliveryRecord(response http.ResponseWriter, record delivery.Record, duplicate bool, corsOrigin string) {
	status := record.Status
	reason := record.Reason
	delivered, retry := record.Delivered, record.Retry
	if status == delivery.StatusAccepted && record.Acceptance == "" {
		if current, ok := s.registry.Get(record.SessionID); ok && legacyProvider(current.Info()) {
			status, delivered, retry = delivery.StatusUnknown, false, false
			reason = "this legacy receipt confirms only terminal input writes, not the complete provider message; inspect history and do not automatically resend"
		}
	}
	if status == delivery.StatusPending {
		status = delivery.StatusUnknown
		if reason == "" {
			reason = "the request was recorded, but Sessions cannot prove whether runner input happened before the previous caller disconnected"
		}
	}
	httpStatus := http.StatusOK
	if status == delivery.StatusNotDelivered {
		httpStatus = http.StatusNotFound
	}
	s.sendJSON(response, httpStatus, map[string]any{
		"operation_id":  record.OperationID,
		"session_id":    record.SessionID,
		"status":        status,
		"delivered":     delivered,
		"retry":         retry,
		"reason":        reason,
		"acceptance":    record.Acceptance,
		"duplicate":     duplicate,
		"created_at_ms": record.CreatedAtMS,
		"updated_at_ms": record.UpdatedAtMS,
	}, corsOrigin)
}
