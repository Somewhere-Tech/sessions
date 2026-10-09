package api

import (
	"github.com/somewhere-tech/sessions/runtime/internal/delivery"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// This is a read-only recovery of the original submit, not a retry. If the
// runtime or retained history changed beyond verification, uncertainty stays.
func (s *Server) reconcileTranscriptAcceptance(record delivery.Record, current *state.Session) delivery.Record {
	intent := record.Transcript
	if !legacyProvider(current.Info()) || current.Info().CreatedAt != intent.RuntimeCreatedAt {
		return record
	}
	start := intent.Cursor
	if start > 0 {
		start--
	}
	window := current.EventsWindow(&start, nil, nil)
	if window.StartIndex != start || window.NextIndex < intent.Cursor {
		return record
	}
	events := window.Events
	if intent.Cursor > 0 {
		if len(events) == 0 || delivery.EventHash(events[0]) != intent.Anchor {
			return record
		}
		events = events[1:]
	}
	for _, event := range events {
		if !delivery.MatchesLateUserEvent(event, intent.MessageHash, record.CreatedAtMS) {
			continue
		}
		confirmed, err := s.deliveries.ConfirmAccepted(record.OperationID, "transcript", "complete message observed in provider history after the first caller stopped waiting")
		if err == nil {
			return confirmed
		}
		return record
	}
	return record
}
