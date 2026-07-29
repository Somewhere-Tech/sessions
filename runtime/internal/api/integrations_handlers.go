package api

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

const (
	transcriptPreviewMaxBytes    = 2 * 1024 * 1024
	transcriptPreviewMaxMessages = 400
)

func (s *Server) handleIntegrationsRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	path := request.URL.Path
	matched := path == "/api/history" || strings.HasPrefix(path, "/api/history/") || path == "/api/errors"
	if !matched {
		return false
	}
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	for _, info := range s.registry.List(true) {
		if session, ok := s.registry.Get(info.ID); ok {
			if err := s.integrationEndpoints.TrackSession(session); err != nil {
				log.Printf("[integrations] track runner %s: %v", info.ID, err)
			}
		}
	}
	if err := s.integrationEndpoints.ObserveFailures(s.registry.List(true)); err != nil {
		log.Printf("[integrations] observe runner failures: %v", err)
	}

	switch {
	case path == "/api/history":
		if request.URL.Query().Get("summary") == "true" {
			sessions, err := s.integrationEndpoints.SearchSessions(s.registry.List(true))
			if err != nil {
				s.integrationError(response, corsOrigin, "history summary failed", err)
				return true
			}
			s.sendJSON(response, http.StatusOK, integrations.HistoryResponse{
				SchemaVersion: integrations.SchemaVersion, Sessions: sessions,
			}, corsOrigin)
			return true
		}
		history, err := s.integrationEndpoints.History(s.registry.List(true))
		if err != nil {
			s.integrationError(response, corsOrigin, "history list failed", err)
			return true
		}
		s.sendJSON(response, http.StatusOK, history, corsOrigin)
		return true
	case path == "/api/errors":
		since, err := errorsSince(request)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		feed, err := s.integrationEndpoints.ErrorFeed(since)
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, feed, corsOrigin)
		return true
	}

	id, variant, ok := historyPath(path)
	if !ok {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "not found", "path": path}, corsOrigin)
		return true
	}
	if variant == "raw" {
		encoded, err := s.integrationEndpoints.Raw(s.registry.List(true), id)
		if errors.Is(err, integrations.ErrHistoryNotFound) {
			s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "history session not found", "id": id}, corsOrigin)
			return true
		}
		if err != nil {
			s.integrationError(response, corsOrigin, "raw history read failed", err)
			return true
		}
		s.sendIntegrationBytes(response, http.StatusOK, "application/octet-stream", encoded, corsOrigin)
		return true
	}

	format := request.URL.Query().Get("format")
	if format == "" {
		format = "json"
	}
	if format != "json" && format != "text" {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": "format must be json or text"}, corsOrigin)
		return true
	}
	var transcript integrations.TranscriptResponse
	var err error
	switch variant {
	case "preview":
		transcript, err = s.integrationEndpoints.TranscriptPreview(
			s.registry.List(true), id, transcriptPreviewMaxBytes, transcriptPreviewMaxMessages,
		)
	case "window":
		var options integrations.TranscriptWindowOptions
		options, err = transcriptWindowOptions(request)
		if err == nil {
			transcript, err = s.integrationEndpoints.TranscriptWindow(s.registry.List(true), id, options)
		}
	default:
		transcript, err = s.integrationEndpoints.Transcript(s.registry.List(true), id)
	}
	if err != nil && variant == "window" {
		var queryError *historyQueryError
		if errors.As(err, &queryError) {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
	}
	if errors.Is(err, integrations.ErrHistoryNotFound) {
		s.sendJSON(response, http.StatusNotFound, map[string]any{"error": "history session not found", "id": id}, corsOrigin)
		return true
	}
	if errors.Is(err, integrations.ErrHistoryChanged) {
		s.sendJSON(response, http.StatusConflict, map[string]any{
			"error": "This conversation changed after the search result was created. Run the search again to refresh its bookmark.",
			"id":    id,
		}, corsOrigin)
		return true
	}
	if err != nil {
		s.integrationError(response, corsOrigin, "history transcript failed", err)
		return true
	}
	if err := s.annotateTranscript(request.Context(), &transcript); err != nil {
		log.Printf("[attribution] annotate transcript for %s: %v", transcript.Session.ID, err)
	}
	if format == "text" {
		response.Header().Set("X-Sessions-Schema-Version", strconv.Itoa(integrations.SchemaVersion))
		s.sendIntegrationBytes(response, http.StatusOK, "text/plain; charset=utf-8", []byte(formatTranscriptText(transcript)), corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, transcript, corsOrigin)
	return true
}

func historyPath(path string) (id, variant string, ok bool) {
	const prefix = "/api/history/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	for _, candidate := range []string{"raw", "preview", "window"} {
		if strings.HasSuffix(rest, "/"+candidate) {
			rest = strings.TrimSuffix(rest, "/"+candidate)
			variant = candidate
			break
		}
	}
	if rest == "" || strings.Contains(rest, "/") {
		return "", "", false
	}
	return rest, variant, true
}

type historyQueryError struct{ message string }

func (e *historyQueryError) Error() string { return e.message }

func transcriptWindowOptions(request *http.Request) (integrations.TranscriptWindowOptions, error) {
	query := request.URL.Query()
	options := integrations.TranscriptWindowOptions{End: -1}
	if raw := query.Get("start"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "start must be a non-negative message index"}
		}
		options.Start = value
	}
	if raw := query.Get("end"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "end must be a non-negative message index"}
		}
		options.End = value
	}
	if options.End >= 0 && options.End < options.Start {
		return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "end must be greater than or equal to start"}
	}
	if options.End >= 0 && options.End-options.Start > integrations.MaxTranscriptWindowSpan {
		return integrations.TranscriptWindowOptions{}, &historyQueryError{
			message: "a transcript window can contain at most " + strconv.Itoa(integrations.MaxTranscriptWindowSpan) + " message positions",
		}
	}
	options.Role = strings.ToLower(strings.TrimSpace(query.Get("role")))
	if options.Role != "" && options.Role != "user" && options.Role != "assistant" && options.Role != "tool" {
		return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "role must be user, assistant, or tool"}
	}
	options.ExpectedMessage = strings.TrimSpace(query.Get("message_id"))
	if options.ExpectedMessage != "" {
		value, err := strconv.Atoi(query.Get("anchor"))
		if err != nil || value < 0 {
			return integrations.TranscriptWindowOptions{}, &historyQueryError{message: "anchor must be a non-negative message index when message_id is set"}
		}
		options.ExpectedIndex = value
	}
	return options, nil
}

func errorsSince(request *http.Request) (uint64, error) {
	raw := request.URL.Query().Get("since")
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, errors.New("since must be a non-negative integer sequence")
	}
	return value, nil
}

func (s *Server) integrationError(response http.ResponseWriter, corsOrigin, summary string, err error) {
	if _, recordErr := s.integrationEndpoints.Emit(integrations.ErrorInput{
		Kind: "daemon_error", Summary: summary, Detail: err.Error(),
	}); recordErr != nil {
		log.Printf("[integrations] record daemon error: %v", recordErr)
	}
	s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
}

func (s *Server) sendIntegrationBytes(
	response http.ResponseWriter,
	status int,
	contentType string,
	body []byte,
	corsOrigin string,
) {
	response.Header().Set("Content-Type", contentType)
	if corsOrigin != "" {
		response.Header().Set("Access-Control-Allow-Origin", corsOrigin)
	}
	response.Header().Set("Vary", "Origin")
	response.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
	response.Header().Set("Access-Control-Allow-Headers", "content-type, authorization")
	response.WriteHeader(status)
	_, _ = response.Write(body)
}

func formatTranscriptText(transcript integrations.TranscriptResponse) string {
	var output strings.Builder
	for index, message := range transcript.Messages {
		if index > 0 {
			output.WriteByte('\n')
		}
		fmt.Fprintf(&output, "[%s", message.Role)
		if message.Timestamp != nil {
			fmt.Fprintf(&output, " %s", *message.Timestamp)
		}
		output.WriteString("]\n")
		output.WriteString(message.Text)
		output.WriteByte('\n')
	}
	return output.String()
}
