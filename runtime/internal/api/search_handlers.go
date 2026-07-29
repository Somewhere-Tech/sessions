package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
	"github.com/somewhere-tech/sessions/runtime/internal/smartsearch"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func (s *Server) handleSearchRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	switch request.URL.Path {
	case "/api/ai/settings":
		s.handleAISettings(response, request, corsOrigin)
		return true
	case "/api/search/plan":
		s.handleSmartSearchPlan(response, request, corsOrigin)
		return true
	case "/api/search":
		// Continue below to the local index.
	default:
		return false
	}
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	options, err := searchOptions(request)
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	indexRoot := s.config.UserStateRoot
	if indexRoot == "" {
		indexRoot = s.config.StateRoot
	}
	indexPath := filepath.Join(indexRoot, "search-index.db")
	result, err := historysearch.Run(request.Context(), s.integrationEndpoints, s.registry.List(true), options, indexPath)
	if err != nil {
		status := http.StatusInternalServerError
		if historysearch.IsOptionError(err) {
			status = http.StatusBadRequest
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, result, corsOrigin)
	return true
}

func (s *Server) handleAISettings(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	switch request.Method {
	case http.MethodGet:
		settings, err := s.loadAISettings()
		if err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, settings, corsOrigin)
	case http.MethodPut:
		var requested state.AISettings
		if err := readJSON(request, &requested); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		normalized, err := state.NormalizeAISettings(requested)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		if err := state.UpdateSettings(s.lan.settingsPath, func(settings *state.Settings) error {
			settings.AI = &normalized
			return nil
		}); err != nil {
			s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, normalized, corsOrigin)
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
	}
}

func (s *Server) handleSmartSearchPlan(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	if request.Method != http.MethodPost {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return
	}
	var body struct {
		Query string `json:"query"`
	}
	if err := readJSON(request, &body); err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	settings, err := s.loadAISettings()
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	ctx, cancel := context.WithTimeout(request.Context(), 2*time.Minute)
	defer cancel()
	plan, err := s.smartSearch.Plan(ctx, settings, body.Query)
	if err != nil {
		status := http.StatusBadGateway
		if smartsearch.IsBusy(err) {
			status = http.StatusTooManyRequests
			response.Header().Set("Retry-After", "2")
		} else if strings.TrimSpace(body.Query) == "" || len(body.Query) > 4*1024 {
			status = http.StatusBadRequest
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return
	}
	s.sendJSON(response, http.StatusOK, plan, corsOrigin)
}

func (s *Server) loadAISettings() (state.AISettings, error) {
	settings, err := state.LoadSettings(s.lan.settingsPath)
	if err != nil {
		return state.AISettings{}, err
	}
	return state.NormalizeAISettings(settings.EffectiveAI())
}

func searchOptions(request *http.Request) (historysearch.Options, error) {
	query := request.URL.Query()
	options := historysearch.Options{
		Query: query.Get("q"), SessionID: query.Get("session"),
		Role: query.Get("role"), Tool: query.Get("tool"),
		NameGlob: query.Get("name"), CWD: query.Get("cwd"),
	}
	if raw := query.Get("regex"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return historysearch.Options{}, &searchQueryError{message: "regex must be true or false"}
		}
		options.Regex = value
	}
	if raw := query.Get("ranked"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return historysearch.Options{}, &searchQueryError{message: "ranked must be true or false"}
		}
		options.Ranked = value
	}
	if raw := query.Get("limit"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return historysearch.Options{}, &searchQueryError{message: "limit must be an integer"}
		}
		if value < 1 || value > historysearch.MaxLimit {
			return historysearch.Options{}, &searchQueryError{message: "limit must be between 1 and " + strconv.Itoa(historysearch.MaxLimit)}
		}
		options.Limit = value
	}
	if raw := query.Get("context"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			return historysearch.Options{}, &searchQueryError{message: "context must be an integer"}
		}
		if value < 0 || value > historysearch.MaxContext {
			return historysearch.Options{}, &searchQueryError{message: "context must be between 0 and " + strconv.Itoa(historysearch.MaxContext)}
		}
		options.Context = value
	}
	if raw := query.Get("timeline"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return historysearch.Options{}, &searchQueryError{message: "timeline must be true or false"}
		}
		options.Timeline = value
	}
	if raw := query.Get("since"); raw != "" {
		value, err := parseSearchTime(raw, false)
		if err != nil {
			return historysearch.Options{}, err
		}
		options.SinceMS = value
	}
	if raw := query.Get("until"); raw != "" {
		value, err := parseSearchTime(raw, true)
		if err != nil {
			return historysearch.Options{}, err
		}
		options.UntilMS = value
	}
	return options, nil
}

func parseSearchTime(raw string, endOfDate bool) (int64, error) {
	raw = strings.TrimSpace(raw)
	if parsed, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return parsed.UnixMilli(), nil
	}
	parsed, err := time.ParseInLocation("2006-01-02", raw, time.Local)
	if err != nil {
		return 0, &searchQueryError{message: "date filters must be YYYY-MM-DD or RFC3339"}
	}
	if endOfDate {
		parsed = parsed.AddDate(0, 0, 1)
	}
	return parsed.UnixMilli(), nil
}

type searchQueryError struct{ message string }

func (e *searchQueryError) Error() string { return e.message }
