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
	s.sendDeliveryRecord(response, record, true, corsOrigin)
	return true
}

func (s *Server) sendDeliveryRecord(response http.ResponseWriter, record delivery.Record, duplicate bool, corsOrigin string) {
	status := record.Status
	reason := record.Reason
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
		"delivered":     record.Delivered,
		"retry":         record.Retry,
		"reason":        reason,
		"duplicate":     duplicate,
		"created_at_ms": record.CreatedAtMS,
		"updated_at_ms": record.UpdatedAtMS,
	}, corsOrigin)
}
