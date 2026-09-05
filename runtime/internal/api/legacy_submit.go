package api

import (
	"context"
	"net/http"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func legacyProvider(info state.SessionInfo) bool {
	return (info.Tool == state.ToolClaude || info.Tool == state.ToolCodex) &&
		info.Kind != state.KindClaudeStructured && info.Kind != state.KindCodexAppServer
}

func (s *Server) submitLegacyProvider(w http.ResponseWriter, request *http.Request, id, data, operationID, origin string, attribution state.InputAttribution) {
	status, delivered, reason := s.deliverLegacyProvider(request.Context(), id, data, attribution)
	acceptance := ""
	if delivered {
		acceptance = "transcript"
	}
	record, err := s.deliveries.Complete(operationID, status, delivered, status == delivery.StatusNotDelivered, reason, acceptance)
	if err != nil {
		s.sendJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not record delivery outcome; inspect the conversation and do not automatically resend", "operation_id": operationID}, origin)
		return
	}
	s.sendDeliveryRecord(w, record, false, origin)
}

func (s *Server) deliverLegacyProvider(ctx context.Context, id, data string, attribution state.InputAttribution) (delivery.Status, bool, string) {
	paste, err := delivery.LegacyPaste(data)
	if err != nil {
		return delivery.StatusNotDelivered, false, err.Error()
	}
	current, ok := s.registry.Get(id)
	if !ok {
		return delivery.StatusNotDelivered, false, "session is unavailable; no message was sent"
	}
	// Capture the absolute stream cursor before any input. Historical identical
	// messages cannot confirm this operation, and subscribers keep their stream.
	baseline := current.ClaudeEventCount()
	if err := s.writeSessionInput(ctx, id, paste, attribution, attribution.SourceSessionID != ""); err != nil {
		return delivery.StatusUnknown, false, "terminal input was not acknowledged: " + err.Error() + "; inspect the conversation before resending"
	}
	timer := time.NewTimer(submitSettleDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return delivery.StatusUnknown, false, "sending stopped before Enter; inspect the composer and do not automatically resend"
	case <-timer.C:
	}
	if !s.registry.Input(ctx, id, "\r") {
		return delivery.StatusUnknown, false, "Enter was not acknowledged; inspect the composer and do not automatically resend"
	}
	confirmation, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if waitForCompleteLegacyMessage(confirmation, current, baseline, data) {
		return delivery.StatusAccepted, true, "complete message observed in provider history"
	}
	return delivery.StatusUnknown, false, "complete message was not observed in provider history; input may be partial or still pending. Inspect the conversation and do not automatically resend"
}

func waitForCompleteLegacyMessage(ctx context.Context, current *state.Session, cursor int64, intended string) bool {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		window := current.EventsWindow(&cursor, nil, nil)
		for _, event := range window.Events {
			if _, matches := delivery.MatchingUserText(event, intended); matches {
				return true
			}
		}
		cursor = window.NextIndex
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}

func (s *Server) submitMuxInput(ctx context.Context, id, data string) (bool, string) {
	if current, ok := s.registry.Get(id); ok && legacyProvider(current.Info()) {
		if reason, blocked := semanticSubmitRefusal(current.Info()); blocked {
			return false, reason
		}
		_, delivered, reason := s.deliverLegacyProvider(ctx, id, data, state.InputAttribution{})
		return delivered, reason
	}
	if !s.registry.Input(ctx, id, data) {
		return false, "session input is unavailable"
	}
	timer := time.NewTimer(submitSettleDelay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false, "sending stopped before Enter; inspect the composer before resending"
	case <-timer.C:
	}
	return s.registry.Input(ctx, id, "\r"), ""
}
