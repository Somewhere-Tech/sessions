package api

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/recap"
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/usage"
)

type recapDayResponse struct {
	Date          string                         `json:"date"`
	Timezone      string                         `json:"timezone"`
	Settings      state.RecapSettings            `json:"settings"`
	Activities    []sessionruntime.DailyActivity `json:"activities"`
	Usage         usage.ReportRow                `json:"usage"`
	Document      *recap.Document                `json:"document"`
	DocumentStale bool                           `json:"documentStale"`
}

func (s *Server) handleRecapRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	switch request.URL.Path {
	case "/api/recap/settings":
		s.handleRecapSettings(response, request, corsOrigin)
		return true
	case "/api/recap/dates":
		if request.Method != http.MethodGet {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		dates, err := s.recaps.Dates()
		if err != nil {
			s.sendRecapError(response, err, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, map[string]any{"dates": dates}, corsOrigin)
		return true
	case "/api/recap":
		if request.Method != http.MethodGet {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		day, _, err := s.recapDay(request.Context(), request.URL.Query().Get("date"))
		if err != nil {
			s.sendRecapError(response, err, corsOrigin)
			return true
		}
		s.sendJSON(response, http.StatusOK, day, corsOrigin)
		return true
	case "/api/recap/generate":
		if request.Method != http.MethodPost {
			s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
			return true
		}
		var body struct {
			Date  string `json:"date"`
			Force bool   `json:"force"`
		}
		if err := readJSON(request, &body); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return true
		}
		day, input, err := s.recapDay(request.Context(), body.Date)
		if err != nil {
			s.sendRecapError(response, err, corsOrigin)
			return true
		}
		ctx, cancel := context.WithTimeout(request.Context(), 5*time.Minute)
		defer cancel()
		document, err := s.recaps.Generate(ctx, day.Settings, input, body.Force)
		if err != nil {
			s.sendRecapError(response, err, corsOrigin)
			return true
		}
		day.Document = &document
		day.DocumentStale = false
		s.sendJSON(response, http.StatusOK, day, corsOrigin)
		return true
	default:
		return false
	}
}

func (s *Server) handleRecapSettings(response http.ResponseWriter, request *http.Request, corsOrigin string) {
	switch request.Method {
	case http.MethodGet:
		settings, err := s.loadRecapSettings()
		if err != nil {
			s.sendRecapError(response, err, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, settings, corsOrigin)
	case http.MethodPut:
		var requested state.RecapSettings
		if err := readJSON(request, &requested); err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		normalized, err := state.NormalizeRecapSettings(requested)
		if err != nil {
			s.sendJSON(response, http.StatusBadRequest, map[string]any{"error": err.Error()}, corsOrigin)
			return
		}
		if err := state.UpdateSettings(s.lan.settingsPath, func(settings *state.Settings) error {
			settings.Recap = &normalized
			return nil
		}); err != nil {
			s.sendRecapError(response, err, corsOrigin)
			return
		}
		s.sendJSON(response, http.StatusOK, normalized, corsOrigin)
	default:
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
	}
}

func (s *Server) recapDay(ctx context.Context, rawDate string) (recapDayResponse, recap.DayInput, error) {
	date, start, end, err := localDay(rawDate)
	if err != nil {
		return recapDayResponse{}, recap.DayInput{}, err
	}
	settings, err := s.loadRecapSettings()
	if err != nil {
		return recapDayResponse{}, recap.DayInput{}, err
	}
	report, err := s.usage.Report(ctx, usage.ReportOptions{Group: "session", Mode: usage.ModeAuto, Since: start, Until: end})
	if err != nil {
		return recapDayResponse{}, recap.DayInput{}, err
	}
	activities := sessionruntime.BuildDailyActivity(s.registry.List(true), s.registry.Get, start, end)
	observed, err := s.usage.ObservedSessions(ctx, start, end)
	if err != nil {
		return recapDayResponse{}, recap.DayInput{}, err
	}
	activities = append(activities, externalDailyActivities(report, observed, start, end)...)
	sort.Slice(activities, func(i, j int) bool {
		if activities[i].LastActivityAt != activities[j].LastActivityAt {
			return activities[i].LastActivityAt < activities[j].LastActivityAt
		}
		return activities[i].ID < activities[j].ID
	})
	document, err := s.recaps.Load(date)
	if err != nil {
		return recapDayResponse{}, recap.DayInput{}, err
	}
	timezone := time.Local.String()
	input := recap.DayInput{Date: date, Timezone: timezone, Activities: activities, Usage: report.Totals}
	documentStale := document != nil && !s.recaps.Current(document, input, settings.Provider)
	return recapDayResponse{
		Date: date, Timezone: timezone, Settings: settings,
		Activities: activities, Usage: report.Totals, Document: document, DocumentStale: documentStale,
	}, input, nil
}

func externalDailyActivities(report usage.Report, observed []usage.ObservedSession, start, end time.Time) []sessionruntime.DailyActivity {
	managed := make(map[string]struct{})
	for _, row := range report.Rows {
		if row.SessionID != "" && row.Provider != "" && row.ProviderSessionID != "" {
			managed[row.Provider+":"+row.ProviderSessionID] = struct{}{}
		}
	}
	activities := make([]sessionruntime.DailyActivity, 0, len(observed))
	for _, providerSession := range observed {
		identity := providerSession.Provider + ":" + providerSession.ProviderSessionID
		if _, exists := managed[identity]; exists {
			continue
		}
		summary := integrations.ConversationDaySummary{}
		for _, path := range providerSession.SourcePaths {
			current, err := integrations.SummarizeConversationDay(path, providerSession.Provider, start, end, providerSession.TurnIDs)
			if err != nil {
				continue
			}
			mergeConversationSummary(&summary, current)
		}
		project := ""
		if cleaned := filepath.Clean(strings.TrimSpace(summary.CWD)); cleaned != "." && cleaned != "" {
			project = filepath.Base(cleaned)
		}
		providerName := "Codex"
		if providerSession.Provider == "claude" {
			providerName = "Claude"
		}
		name := providerName
		if project != "" {
			name += " · " + project
		}
		description := summary.LastUser
		if description == "" {
			description = summary.FirstUser
		}
		activities = append(activities, sessionruntime.DailyActivity{
			ID: "provider:" + identity, Name: name,
			Description: description, Summary: summary.LastAssistant,
			Outcome: "observed", Tool: providerSession.Provider, CWD: summary.CWD,
			SourceRepo: project, CreatedAt: providerSession.FirstActivityAt,
			LastActivityAt:   providerSession.LastActivityAt,
			ProvenanceStatus: "Outside Sessions", Source: "provider",
			Origin: summary.Origin, ProviderSessionID: providerSession.ProviderSessionID,
		})
	}
	return activities
}

func mergeConversationSummary(target *integrations.ConversationDaySummary, current integrations.ConversationDaySummary) {
	if target.CWD == "" {
		target.CWD = current.CWD
	}
	if current.Origin != "" {
		target.Origin = current.Origin
	}
	if target.FirstUser == "" && current.FirstUser != "" {
		target.FirstUser = current.FirstUser
	}
	if current.LastUser != "" {
		target.LastUser = current.LastUser
	}
	if current.LastAssistant != "" {
		target.LastAssistant = current.LastAssistant
	}
	if target.FirstAt == 0 || (current.FirstAt != 0 && current.FirstAt < target.FirstAt) {
		target.FirstAt = current.FirstAt
	}
	if current.LastAt > target.LastAt {
		target.LastAt = current.LastAt
	}
	target.MessageCount += current.MessageCount
}

func (s *Server) loadRecapSettings() (state.RecapSettings, error) {
	settings, err := state.LoadSettings(s.lan.settingsPath)
	if err != nil {
		return state.RecapSettings{}, err
	}
	return state.NormalizeRecapSettings(settings.EffectiveRecap())
}

func localDay(raw string) (string, time.Time, time.Time, error) {
	if raw == "" {
		raw = time.Now().In(time.Local).Format("2006-01-02")
	}
	start, err := time.ParseInLocation("2006-01-02", raw, time.Local)
	if err != nil || start.Format("2006-01-02") != raw {
		return "", time.Time{}, time.Time{}, errors.New("date must use YYYY-MM-DD")
	}
	return raw, start, start.AddDate(0, 0, 1), nil
}

func (s *Server) sendRecapError(response http.ResponseWriter, err error, corsOrigin string) {
	status := http.StatusInternalServerError
	message := err.Error()
	if message == "date must use YYYY-MM-DD" || message == "daily recap is off; choose Codex or Claude in Settings first" {
		status = http.StatusBadRequest
	}
	s.sendJSON(response, status, map[string]any{"error": message}, corsOrigin)
}
