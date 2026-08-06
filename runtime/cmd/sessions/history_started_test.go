package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

func startedAt(session integrations.HistorySession, at time.Time) integrations.HistorySession {
	session.CreatedAt = at.UnixMilli()
	return session
}

// The user's own description of what he needs from a browse: "the added
// benefit is that you could see where and when it got started." The daemon has
// always sent created_at and the row discarded it, so the browse could only
// ever say when a conversation was last touched.
//
// It matters most for Codex, whose rollouts are filed under the date they
// started and are appended to for weeks -- the 1.15 GB rollout on this machine
// is filed under 2026-07-19 and was written to seventeen days later. Sorting
// by last activity is right, but it leaves a row that says "yesterday" for a
// conversation begun three weeks ago, which is exactly the one a person
// cannot place.
func TestHistoryShowsWhenAConversationStarted(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: startedAt(
			conversationAt("provider:codex:old", "long running rollout", "codex", "/w/testing", 900,
				now.Add(-24*time.Hour)),
			now.AddDate(0, 0, -18))},
		{session: startedAt(
			conversationAt("provider:claude:today", "quick fix", "claude", "/w/app", 12,
				now.Add(-2*time.Hour)),
			now.Add(-3*time.Hour))},
	})

	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "-n", "10")
	if code != exitSatisfied {
		t.Fatalf("history exit=%d stderr=%q", code, stderr)
	}

	startedDay := now.AddDate(0, 0, -18).Format("2006-01-02")
	if !strings.Contains(stdout, "started "+startedDay) {
		t.Errorf("a conversation begun %s and last touched yesterday did not say when it "+
			"started; the row cannot be told from one begun yesterday.\n%s", startedDay, stdout)
	}

	// The same-day conversation must stay quiet, or every row in a normal
	// day's work repeats its own date and the column stops carrying meaning.
	for _, line := range strings.Split(stdout, "\n") {
		if strings.Contains(line, "quick fix") {
			continue
		}
		if strings.Contains(line, "12 messages") && strings.Contains(line, "started ") {
			t.Errorf("a conversation started and finished the same day announced a start "+
				"date: %q", strings.TrimSpace(line))
		}
	}
}

// A caller that sorts or filters needs the number, not the rendered date, and
// needs it under a stable key.
func TestHistoryJSONCarriesTheStartTime(t *testing.T) {
	now := time.Now()
	started := now.AddDate(0, 0, -18)
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: startedAt(
			conversationAt("provider:codex:old", "long running rollout", "codex", "/w/testing", 900,
				now.Add(-24*time.Hour)),
			started)},
	})

	stdout, stderr, code := runHistoryCLI(t, daemon, "--json", "history", "-n", "10")
	if code != exitSatisfied {
		t.Fatalf("history exit=%d stderr=%q", code, stderr)
	}
	var document struct {
		Conversations []struct {
			StartedAtMS    int64  `json:"started_at_ms"`
			StartedAt      string `json:"started_at"`
			LastActiveAtMS int64  `json:"last_active_at_ms"`
		} `json:"conversations"`
	}
	if err := json.Unmarshal([]byte(stdout), &document); err != nil {
		t.Fatalf("undecodable history document: %v (raw %q)", err, stdout)
	}
	if len(document.Conversations) != 1 {
		t.Fatalf("got %d conversations, want 1", len(document.Conversations))
	}
	row := document.Conversations[0]
	if row.StartedAtMS != started.UnixMilli() {
		t.Errorf("started_at_ms = %d, want %d", row.StartedAtMS, started.UnixMilli())
	}
	if row.StartedAt == "" {
		t.Error("started_at was empty; the string form is what a human reads back")
	}
	if row.StartedAtMS >= row.LastActiveAtMS {
		t.Errorf("started_at_ms %d is not before last_active_at_ms %d",
			row.StartedAtMS, row.LastActiveAtMS)
	}
}

// A daemon that does not send created_at -- an older one on another machine in
// the fleet -- must produce a row that simply omits the start, not a row
// claiming the conversation began at the zero time.
func TestHistoryOmitsAnUnknownStartTime(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:nostart", "no created_at", "codex", "/w/testing", 5,
			now.Add(-24*time.Hour))},
	})

	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "-n", "10")
	if code != exitSatisfied {
		t.Fatalf("history exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "started ") {
		t.Errorf("a row with no created_at announced a start date:\n%s", stdout)
	}
	if strings.Contains(stdout, "1970") {
		t.Errorf("an absent start time rendered as the epoch:\n%s", stdout)
	}
}
