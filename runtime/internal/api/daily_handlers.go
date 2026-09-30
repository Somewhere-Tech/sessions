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
	sessionruntime "github.com/somewhere-tech/sessions/runtime/internal/session"
	"github.com/somewhere-tech/sessions/runtime/internal/usage"
)

type dailyDayResponse struct {
	Date       string                         `json:"date"`
	Timezone   string                         `json:"timezone"`
	Activities []sessionruntime.DailyActivity `json:"activities"`
	Usage      usage.ReportRow                `json:"usage"`
}

func (s *Server) handleDailyRoute(response http.ResponseWriter, request *http.Request, corsOrigin string) bool {
	if request.URL.Path != "/api/daily" {
		return false
	}
	if request.Method != http.MethodGet {
		s.sendJSON(response, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"}, corsOrigin)
		return true
	}
	day, err := s.dailyDay(request.Context(), request.URL.Query().Get("date"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errDailyDateFormat) {
			status = http.StatusBadRequest
		}
		s.sendJSON(response, status, map[string]any{"error": err.Error()}, corsOrigin)
		return true
	}
	s.sendJSON(response, http.StatusOK, day, corsOrigin)
	return true
}

func (s *Server) dailyDay(ctx context.Context, rawDate string) (dailyDayResponse, error) {
	date, start, end, err := localDay(rawDate)
	if err != nil {
		return dailyDayResponse{}, err
	}
	report, err := s.usage.Report(ctx, usage.ReportOptions{Group: "session", Mode: usage.ModeAuto, Since: start, Until: end})
	if err != nil {
		return dailyDayResponse{}, err
	}
	activities := sessionruntime.BuildDailyActivity(s.registry.List(true), s.registry.Get, start, end)
	observed, err := s.usage.ObservedSessions(ctx, start, end)
	if err != nil {
		return dailyDayResponse{}, err
	}
	activities = append(activities, externalDailyActivities(report, observed, start, end)...)
	sort.Slice(activities, func(i, j int) bool {
		if activities[i].LastActivityAt != activities[j].LastActivityAt {
			return activities[i].LastActivityAt < activities[j].LastActivityAt
		}
		return activities[i].ID < activities[j].ID
	})
	return dailyDayResponse{
		Date: date, Timezone: time.Local.String(), Activities: activities, Usage: report.Totals,
	}, nil
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

var errDailyDateFormat = errors.New("date must use YYYY-MM-DD")

func localDay(raw string) (string, time.Time, time.Time, error) {
	if raw == "" {
		raw = time.Now().In(time.Local).Format("2006-01-02")
	}
	start, err := time.ParseInLocation("2006-01-02", raw, time.Local)
	if err != nil || start.Format("2006-01-02") != raw {
		return "", time.Time{}, time.Time{}, errDailyDateFormat
	}
	return raw, start, start.AddDate(0, 0, 1), nil
}
