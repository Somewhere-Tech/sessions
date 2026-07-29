package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/usage"
)

func (s *Server) handleUsageRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/usage" {
		return false
	}
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	options, err := usageOptions(request)
	if err != nil {
		s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	report, err := s.usage.Report(request.Context(), options)
	if err != nil {
		s.sendJSON(response, http.StatusInternalServerError, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, report, corsOrigin)
	return true
}

func usageOptions(request *http.Request) (usage.ReportOptions, error) {
	query := request.URL.Query()
	options := usage.ReportOptions{
		Group:     strings.ToLower(strings.TrimSpace(query.Get("group"))),
		Mode:      strings.ToLower(strings.TrimSpace(query.Get("mode"))),
		Provider:  strings.ToLower(strings.TrimSpace(query.Get("provider"))),
		Dimension: strings.ToLower(strings.TrimSpace(query.Get("dimension"))),
	}
	if options.Group != "" && !usageValueAllowed(options.Group, "daily", "weekly", "monthly", "session", "tag", "provider", "model") {
		return usage.ReportOptions{}, &usageQueryError{"group must be daily, weekly, monthly, session, tag, provider, or model"}
	}
	if options.Mode != "" && !usageValueAllowed(options.Mode, usage.ModeAuto, usage.ModeCalculate, usage.ModeDisplay) {
		return usage.ReportOptions{}, &usageQueryError{"mode must be auto, calculate, or display"}
	}
	if options.Group == "tag" && options.Dimension == "" {
		return usage.ReportOptions{}, &usageQueryError{"tag reports need a dimension"}
	}
	if options.Provider != "" && options.Provider != "claude" && options.Provider != "codex" {
		return usage.ReportOptions{}, &usageQueryError{"provider must be claude or codex"}
	}
	var err error
	if raw := strings.TrimSpace(query.Get("since")); raw != "" {
		options.Since, err = time.ParseInLocation("2006-01-02", raw, time.Local)
		if err != nil {
			return usage.ReportOptions{}, &usageQueryError{"since must use YYYY-MM-DD"}
		}
	}
	if raw := strings.TrimSpace(query.Get("until")); raw != "" {
		options.Until, err = time.ParseInLocation("2006-01-02", raw, time.Local)
		if err != nil {
			return usage.ReportOptions{}, &usageQueryError{"until must use YYYY-MM-DD"}
		}
		options.Until = options.Until.AddDate(0, 0, 1)
	}
	if !options.Since.IsZero() && !options.Until.IsZero() && !options.Since.Before(options.Until) {
		return usage.ReportOptions{}, &usageQueryError{"since must be on or before until"}
	}
	return options, nil
}

type usageQueryError struct{ message string }

func (e *usageQueryError) Error() string { return e.message }

func usageValueAllowed(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
