package api

import (
	"errors"
	"io"
	"net/http"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/migrate"
)

func (s *Server) handleMoveRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/migrate/receive" && request.URL.Path != "/api/migrate/create" {
		return false
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	if request.URL.Path == "/api/migrate/create" {
		var body migrate.CreateRequest
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		create, err := migrate.SessionRequest(body)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		session, err := s.registry.Create(request.Context(), create)
		if err != nil {
			s.sendJSON(response, http.StatusConflict, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		result := migrate.CreateResult{Session: session}
		store, err := ledger.Open(request.Context(), ledger.Options{})
		if err == nil {
			defer store.Close()
			err = store.Migrations().RecordMovedFrom(request.Context(), ledger.MovedFrom{
				Meta:           ledger.Meta{LaneID: session.ID},
				SourceEndpoint: body.SourceEndpoint, SourceLaneID: body.SourceID,
			})
		}
		if err != nil {
			result.Warning = "The conversation is live on this machine, but Sessions could not record its source-machine link: " + err.Error()
			s.sendJSON(response, http.StatusAccepted, result, corsOrigin)
			return true
		}
		result.LineageRecorded = true
		s.sendJSON(response, http.StatusCreated, result, corsOrigin)
		return true
	}
	request.Body = http.MaxBytesReader(response, request.Body, migrate.MaxReceiveBodyBytes)
	var body migrate.ReceiveRequest
	if err := migrate.DecodeReceive(request.Body, &body); err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, io.ErrUnexpectedEOF) {
			status = http.StatusRequestEntityTooLarge
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	result, err := migrate.Receive(request.Context(), body, migrate.ReceiveOptions{})
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusCreated, result, corsOrigin)
	return true
}
