package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func millis(at time.Time) *int64 {
	value := at.UnixMilli()
	return &value
}

// The incident this filter comes from: an orchestrator lane on a self-scheduled
// half-hourly cadence writes its own prompts into its transcript as user turns.
// It is the most recently active conversation on the machine and nobody has
// said a word to it in an hour, while the lane the owner was actually talking
// to has been quiet since. Browsing by transcript activity puts the machinery
// on top; --touched asks the other question.
func TestHistoryTouchedKeepsOnlyConversationsAPersonSpokeInto(t *testing.T) {
	now := time.Now()
	const (
		integrator = "11111111-2222-4333-8444-000000000001"
		spokenTo   = "11111111-2222-4333-8444-000000000002"
		subagent   = "11111111-2222-4333-8444-000000000003"
	)
	live := []session{
		// Ticking every half hour, last spoken to an hour ago.
		{ID: integrator, Cmd: "claude", Tool: "claude-code",
			LastHumanMessageAt: millis(now.Add(-time.Hour))},
		// Quiet since, but the owner spoke into it five minutes ago.
		{ID: spokenTo, Cmd: "claude", Tool: "claude-code",
			LastHumanMessageAt: millis(now.Add(-5 * time.Minute))},
		// Driven entirely by its parent: agent contact, never human.
		{ID: subagent, Cmd: "claude", Tool: "claude-code",
			LastAgentMessageAt: millis(now.Add(-time.Minute))},
	}
	daemon := newHistoryFixtureDaemon(t, live, []conversationFixture{
		{session: conversationAt(integrator, "integrator lane", "claude", "/w/one", 812, now.Add(-time.Minute))},
		{session: conversationAt(spokenTo, "the lane you asked", "claude", "/w/two", 40, now.Add(-3*time.Hour))},
		{session: conversationAt(subagent, "delegated worker", "claude", "/w/three", 60, now.Add(-30*time.Second))},
		{session: conversationAt("provider:claude:cold", "last week somewhere", "claude", "/w/four", 9, now.Add(-6*time.Hour))},
	})

	// Without the filter, transcript activity decides, and the ticking lane and
	// the subagent outrank the conversation the owner was in.
	plain, stderr, code := runHistoryCLI(t, daemon, "history")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Index(plain, "the lane you asked") < strings.Index(plain, "integrator lane") {
		t.Fatalf("default browse is no longer newest-first by transcript activity: %q", plain)
	}

	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "--touched")
	if code != 0 {
		t.Fatalf("--touched exit=%d stderr=%q", code, stderr)
	}
	for _, gone := range []string{"delegated worker", "last week somewhere"} {
		if strings.Contains(stdout, gone) {
			t.Errorf("--touched kept %q, which no person has spoken into: %q", gone, stdout)
		}
	}
	for _, kept := range []string{"integrator lane", "the lane you asked"} {
		if !strings.Contains(stdout, kept) {
			t.Errorf("--touched dropped %q, which a person did speak into: %q", kept, stdout)
		}
	}
	if strings.Index(stdout, "the lane you asked") > strings.Index(stdout, "integrator lane") {
		t.Fatalf("--touched ordered by transcript activity rather than human recency, so the "+
			"lane whose own cron fired a minute ago outranked the one the owner spoke to five "+
			"minutes ago — the exact confusion the filter exists to end: %q", stdout)
	}
}

func TestHistoryTouchedIsCarriedInJSON(t *testing.T) {
	now := time.Now()
	const spokenTo = "11111111-2222-4333-8444-000000000009"
	spokenAt := now.Add(-7 * time.Minute)
	daemon := newHistoryFixtureDaemon(t,
		[]session{{ID: spokenTo, Cmd: "claude", Tool: "claude-code", LastHumanMessageAt: millis(spokenAt)}},
		[]conversationFixture{
			{session: conversationAt(spokenTo, "spoken to", "claude", "/w/two", 12, now.Add(-time.Hour))},
			{session: conversationAt("provider:claude:quiet", "never spoken to", "claude", "/w/x", 3, now)},
		})
	stdout, stderr, code := runHistoryCLI(t, daemon, "--json", "history", "--touched")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	var body historyBrowseResponse
	if err := json.Unmarshal([]byte(stdout), &body); err != nil {
		t.Fatalf("decode: %v\n%s", err, stdout)
	}
	if len(body.Conversations) != 1 || body.Conversations[0].ID != spokenTo {
		t.Fatalf("conversations = %#v", body.Conversations)
	}
	if body.Conversations[0].LastHumanMessageAtMS != spokenAt.UnixMilli() {
		t.Errorf("last_human_message_at_ms = %d, want %d",
			body.Conversations[0].LastHumanMessageAtMS, spokenAt.UnixMilli())
	}
	if body.Matched != 1 {
		t.Errorf("matched = %d, want 1", body.Matched)
	}
}

// The two columns sit side by side because the whole point is that they can
// disagree. The ticking lane's LAST-USER is a minute old and no person has ever
// spoken into it; reading LAST-USER as human contact is what let a fleet census
// report machine chatter as the owner's week.
func TestLSShowsLastHumanBesideLastUser(t *testing.T) {
	now := time.Now()
	const (
		ticking = "41000000-0000-4000-8000-000000000001"
		spoken  = "42000000-0000-4000-8000-000000000002"
	)
	t.Setenv("HOME", t.TempDir())
	body := `{"sessions":[` +
		`{"id":"` + ticking + `","name":"integrator","description":"","cmd":"claude","cwd":"/tmp",` +
		`"createdAt":1,"pid":11,"tool":"claude-code","working":false,"lastDataAt":1,` +
		`"lastUserMessageAt":` + millisText(now.Add(-time.Minute)) + `,` +
		`"lastHumanMessageAt":null,"lastAgentMessageAt":null,"exited":false,"pinned":false},` +
		`{"id":"` + spoken + `","name":"workbench","description":"","cmd":"claude","cwd":"/tmp",` +
		`"createdAt":1,"pid":12,"tool":"claude-code","working":false,"lastDataAt":1,` +
		`"lastUserMessageAt":` + millisText(now.Add(-2*time.Hour)) + `,` +
		`"lastHumanMessageAt":` + millisText(now.Add(-2*time.Hour)) + `,"lastAgentMessageAt":null,` +
		`"exited":false,"pinned":false}` +
		`]}`
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/sessions" {
			_, _ = response.Write([]byte(body))
			return
		}
		http.NotFound(response, request)
	}))
	t.Cleanup(server.Close)

	stdout, stderr, code := runOwnershipCLI(t, server.URL, "ls")
	if code != 0 || stderr != "" {
		t.Fatalf("ls exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "LAST-HUMAN") {
		t.Fatalf("ls did not show LAST-HUMAN even though a person had spoken: %q", stdout)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	header, tick := lines[0], ""
	for _, line := range lines {
		if strings.Contains(line, "integrator") {
			tick = line
		}
	}
	if strings.Index(header, "LAST-HUMAN") < strings.Index(header, "LAST-USER") {
		t.Errorf("LAST-HUMAN is not beside LAST-USER: %q", header)
	}
	// The ticking lane reports a minute of transcript activity and no human
	// contact at all, which is exactly the pair a reader has to be able to see.
	columns := strings.Fields(tick)
	if len(columns) < 3 || columns[len(columns)-1] != "11" ||
		columns[len(columns)-2] != "-" || columns[len(columns)-3] != "1m" {
		t.Errorf("the ticking lane's LAST-USER/LAST-HUMAN pair = %q, want a recent "+
			"transcript turn beside no human contact", tick)
	}
}

func millisText(at time.Time) string {
	return strconv.FormatInt(at.UnixMilli(), 10)
}

// The LAST-HUMAN column follows PIN and PROFILE: it appears when it would say
// something and stays out of the way when it would not.
func TestListingShowsLastHumanOnlyWhenSomebodyHasSpoken(t *testing.T) {
	now := time.Now()
	quiet := []sessionRecord{{value: session{ID: "a", LastUserMessageAt: millis(now)}}}
	if recordsHaveHumanMessages(quiet) {
		t.Error("LAST-HUMAN would appear for a session whose only recent activity is a transcript user turn")
	}
	spoken := append(quiet, sessionRecord{value: session{ID: "b", LastHumanMessageAt: millis(now)}})
	if !recordsHaveHumanMessages(spoken) {
		t.Error("LAST-HUMAN stayed hidden even though a person had spoken")
	}
}
