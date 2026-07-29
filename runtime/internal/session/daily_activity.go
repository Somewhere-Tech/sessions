package session

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

// DailyActivity is the compact, factual input used by the opt-in daily recap.
// It intentionally carries summaries and metadata rather than full transcripts
// so the provider call stays cheap and does not disclose unrelated history.
type DailyActivity struct {
	ID                string            `json:"id"`
	Name              string            `json:"name"`
	Description       string            `json:"description,omitempty"`
	Summary           string            `json:"summary,omitempty"`
	Outcome           string            `json:"outcome"`
	Tool              string            `json:"tool"`
	CWD               string            `json:"cwd"`
	Branch            string            `json:"branch,omitempty"`
	SourceRepo        string            `json:"sourceRepo,omitempty"`
	Tags              map[string]string `json:"tags,omitempty"`
	CreatedAt         int64             `json:"createdAt"`
	LastActivityAt    int64             `json:"lastActivityAt"`
	ExitedAt          *int64            `json:"exitedAt,omitempty"`
	ParentSessionID   string            `json:"parentSessionId,omitempty"`
	CreatorAncestry   []string          `json:"creatorAncestry,omitempty"`
	ProvenanceStatus  string            `json:"provenanceStatus,omitempty"`
	Source            string            `json:"source,omitempty"`
	Origin            string            `json:"origin,omitempty"`
	ProviderSessionID string            `json:"providerSessionId,omitempty"`
}

// DailyActivity returns sessions which show activity inside the selected local
// day. Closed ledger-only lanes remain useful through their durable metadata;
// live/adopted sessions also contribute their last assistant summary.
func (m *Manager) DailyActivity(start, end time.Time) []DailyActivity {
	return BuildDailyActivity(m.List(true), m.Get, start, end)
}

// BuildDailyActivity is shared by the full Manager and narrow API test
// registries which expose the same List/Get boundary.
func BuildDailyActivity(infos []state.SessionInfo, get func(string) (*state.Session, bool), start, end time.Time) []DailyActivity {
	startMS, endMS := start.UnixMilli(), end.UnixMilli()
	activities := make([]DailyActivity, 0)
	for _, info := range infos {
		last := int64(0)
		for _, candidate := range []int64{info.CreatedAt, info.LastDataAt, pointerValue(info.LastUserMessageAt), pointerValue(info.ExitedAt)} {
			if withinDay(candidate, startMS, endMS) && candidate > last {
				last = candidate
			}
		}

		summary := ""
		if current, ok := get(info.ID); ok {
			for _, output := range current.Replay(0).Events {
				if withinDay(output.At, startMS, endMS) && output.At > last {
					last = output.At
				}
			}
			events := structuredEventsWithin(current.ClaudeEventLog(), startMS, endMS)
			for _, event := range events {
				if at := structuredEventAt(event); at > last {
					last = at
				}
			}
			summary = FinalAssistantSummary(events)
		}
		if last == 0 {
			continue
		}
		activities = append(activities, DailyActivity{
			ID: info.ID, Name: sessionDisplayLabel(info), Description: info.Description,
			Summary: summary, Outcome: dailyOutcome(info), Tool: string(info.Tool), CWD: info.Cwd,
			Branch: info.Branch, SourceRepo: info.SourceRepo, Tags: state.CloneTags(info.Tags),
			CreatedAt: info.CreatedAt, LastActivityAt: last, ExitedAt: info.ExitedAt,
			ParentSessionID: info.ParentSessionID, CreatorAncestry: append([]string(nil), info.CreatorAncestry...),
			ProvenanceStatus: info.ProvenanceStatus, Source: "sessions",
		})
	}
	sort.Slice(activities, func(i, j int) bool {
		if activities[i].LastActivityAt == activities[j].LastActivityAt {
			return activities[i].ID < activities[j].ID
		}
		return activities[i].LastActivityAt < activities[j].LastActivityAt
	})
	return activities
}

func structuredEventsWithin(events []json.RawMessage, start, end int64) []json.RawMessage {
	selected := make([]json.RawMessage, 0, len(events))
	for _, event := range events {
		if withinDay(structuredEventAt(event), start, end) {
			selected = append(selected, event)
		}
	}
	return selected
}

func structuredEventAt(raw json.RawMessage) int64 {
	var event struct {
		Timestamp any `json:"timestamp"`
	}
	if json.Unmarshal(raw, &event) != nil {
		return 0
	}
	switch value := event.Timestamp.(type) {
	case float64:
		if value > 0 {
			return int64(value)
		}
	case string:
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UnixMilli()
		}
	}
	return 0
}

func withinDay(value, start, end int64) bool {
	return value >= start && value < end
}

func pointerValue(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}

func dailyOutcome(info state.SessionInfo) string {
	if info.Working && !info.Exited {
		return "working"
	}
	if !info.Exited {
		return "idle"
	}
	if info.ExitCode != nil && *info.ExitCode != 0 {
		return "error"
	}
	if info.ExitSignal != nil && *info.ExitSignal != "" {
		return "error"
	}
	return "done"
}
