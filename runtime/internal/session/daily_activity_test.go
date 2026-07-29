package session

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestDailyOutcome(t *testing.T) {
	zero, one := 0, 1
	if got := dailyOutcome(state.SessionInfo{Working: true}); got != "working" {
		t.Fatalf("working outcome = %q", got)
	}
	if got := dailyOutcome(state.SessionInfo{Exited: true, ExitCode: &zero}); got != "done" {
		t.Fatalf("done outcome = %q", got)
	}
	if got := dailyOutcome(state.SessionInfo{Exited: true, ExitCode: &one}); got != "error" {
		t.Fatalf("error outcome = %q", got)
	}
}

func TestWithinDay(t *testing.T) {
	start := time.Date(2026, 7, 22, 0, 0, 0, 0, time.Local).UnixMilli()
	end := time.Date(2026, 7, 23, 0, 0, 0, 0, time.Local).UnixMilli()
	if !withinDay(start, start, end) || withinDay(end, start, end) || withinDay(start-1, start, end) {
		t.Fatal("withinDay boundaries are not half-open")
	}
}

func TestBuildDailyActivityKeepsSelectedDayTimestamp(t *testing.T) {
	start := time.Date(2026, 7, 22, 0, 0, 0, 0, time.Local)
	end := start.AddDate(0, 0, 1)
	created := start.Add(2 * time.Hour).UnixMilli()
	after := end.Add(time.Hour).UnixMilli()
	activities := BuildDailyActivity([]state.SessionInfo{
		{ID: "created-today", Name: "Created today", CreatedAt: created, LastDataAt: after},
		{ID: "outside", Name: "Outside", CreatedAt: start.Add(-time.Hour).UnixMilli(), LastDataAt: after},
	}, func(string) (*state.Session, bool) { return nil, false }, start, end)
	if len(activities) != 1 || activities[0].ID != "created-today" || activities[0].LastActivityAt != created {
		t.Fatalf("activities = %#v", activities)
	}
}

func TestStructuredEventsWithinUsesRFC3339AndMillisecondTimestamps(t *testing.T) {
	start := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 0, 1)
	events := []json.RawMessage{
		json.RawMessage(`{"timestamp":"2026-07-22T12:00:00Z"}`),
		json.RawMessage(`{"timestamp":1784732400000}`),
		json.RawMessage(`{"timestamp":"2026-07-23T12:00:00Z"}`),
	}
	selected := structuredEventsWithin(events, start.UnixMilli(), end.UnixMilli())
	if len(selected) != 2 {
		t.Fatalf("selected = %s", selected)
	}
}
