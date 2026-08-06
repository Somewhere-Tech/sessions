package api

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
	"github.com/somewhere-tech/sessions/runtime/internal/migrate"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func (s *Server) handleMoveRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/migrate/export" && request.URL.Path != "/api/migrate/complete" && request.URL.Path != "/api/migrate/receive" && request.URL.Path != "/api/migrate/create" {
		return false
	}
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	if request.URL.Path == "/api/migrate/export" {
		var body migrate.ExportRequest
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		body.SessionID = strings.TrimSpace(body.SessionID)
		if body.SessionID == "" {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "session_id is required"}, corsOrigin)
			return true
		}
		var source *state.SessionInfo
		for _, candidate := range s.registry.List(true) {
			if candidate.ID == body.SessionID {
				value := candidate
				source = &value
				break
			}
		}
		if source == nil {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "session not found"}, corsOrigin)
			return true
		}
		if !source.Exited {
			s.sendJSON(response, http.StatusConflict, map[string]any{"error": "session is still live; end it before moving"}, corsOrigin)
			return true
		}
		if source.Tool != state.ToolClaude && source.Tool != state.ToolCodex {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "only Claude and Codex conversations can move"}, corsOrigin)
			return true
		}
		if source.Profile != "" || source.ConfigDir != "" {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "isolated provider profiles cannot move between machines yet"}, corsOrigin)
			return true
		}
		store, err := ledger.Open(request.Context(), ledger.Options{})
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": "open source ledger: " + err.Error()}, corsOrigin)
			return true
		}
		defer store.Close()
		handoff, err := migrate.ResolveSource(request.Context(), store, migrate.SourceSession{
			ID: source.ID, Name: source.Name, Tool: string(source.Tool), Cmd: source.Cmd,
			Args: source.Args, Cwd: source.Cwd, CreatedAt: source.CreatedAt,
		})
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		workspace, err := migrate.PrepareWorkspace(request.Context(), source.Cwd, source.ID, migrate.WorkspaceOptions{
			AllowDirty: body.AllowDirty, DryRun: body.DryRun,
		})
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		handoff.Workspace = workspace
		handoff.SourceEndpoint = strings.TrimSpace(body.SourceEndpoint)
		if body.RuntimeMode != "" {
			if body.RuntimeMode != migrate.RuntimeRich && body.RuntimeMode != migrate.RuntimeTerminal {
				s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "runtime_mode must be rich or terminal"}, corsOrigin)
				return true
			}
			handoff.RuntimeMode = body.RuntimeMode
		}
		result := migrate.ExportResult{Request: handoff, Plan: migrate.MoveResult{
			SourceID: source.ID, Tool: handoff.Tool, Cwd: handoff.Cwd,
			ResumeRecipe: append([]string(nil), handoff.ResumeRecipe...), RuntimeMode: handoff.RuntimeMode,
			Workspace: workspace, ConversationSize: len(handoff.ConversationBytes), DryRun: body.DryRun,
		}}
		s.sendJSON(response, http.StatusOK, result, corsOrigin)
		return true
	}
	if request.URL.Path == "/api/migrate/complete" {
		var body migrate.CompleteRequest
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		if strings.TrimSpace(body.SourceID) == "" || strings.TrimSpace(body.TargetID) == "" || strings.TrimSpace(body.TargetEndpoint) == "" {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "source_id, target_id, and target_endpoint are required"}, corsOrigin)
			return true
		}
		if _, err := migrate.NewClient(body.TargetEndpoint, ""); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		var source *state.SessionInfo
		for _, candidate := range s.registry.List(true) {
			if candidate.ID == body.SourceID {
				value := candidate
				source = &value
				break
			}
		}
		if source == nil {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "source session not found"}, corsOrigin)
			return true
		}
		if !source.Exited {
			s.sendJSON(response, http.StatusConflict, map[string]any{"error": "source session is still live; no move was recorded"}, corsOrigin)
			return true
		}
		store, err := ledger.Open(request.Context(), ledger.Options{})
		if err == nil {
			defer store.Close()
			err = store.Migrations().RecordMovedTo(request.Context(), ledger.MovedTo{
				Meta: ledger.Meta{LaneID: body.SourceID}, TargetEndpoint: body.TargetEndpoint,
				NewLaneID: body.TargetID, CheckpointRef: body.CheckpointRef,
			})
		}
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"ok": true}, corsOrigin)
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
	// /api/migrate/receive decodes with migrate.DecodeReceive rather than
	// readJSON because it accepts a much larger body and rejects unknown
	// fields. It still owes callers the same request guards every other POST
	// gets, so apply them here instead of letting this one route skip them.
	if err := checkJSONContentType(request); err != nil {
		s.sendJSON(response, http.StatusUnsupportedMediaType, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	request.Body = http.MaxBytesReader(response, request.Body, migrate.MaxReceiveBodyBytes)
	encoded, err := io.ReadAll(request.Body)
	if err != nil {
		status := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			status = http.StatusRequestEntityTooLarge
		}
		s.sendJSON(response, status, map[string]any{"error": jsonRequestError(err).Error()}, corsOrigin)
		return true
	}
	if !utf8.Valid(encoded) {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "request body must be valid UTF-8"}, corsOrigin)
		return true
	}
	var body migrate.ReceiveRequest
	if err := migrate.DecodeReceive(bytes.NewReader(encoded), &body); err != nil {
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
