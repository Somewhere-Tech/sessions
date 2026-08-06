package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
)

// conversationFixture is one row of a fake daemon's history.
type conversationFixture struct {
	session  integrations.HistorySession
	messages []integrations.TranscriptMessage
}

type historyFixtureDaemon struct {
	server *httptest.Server
	mu     sync.Mutex
	// requests records every method+path the CLI issued, so a test can assert
	// that browsing and previewing never wrote anything.
	requests []string
	search   historysearch.Response
}

func newHistoryFixtureDaemon(
	t *testing.T, live []session, conversations []conversationFixture,
) *historyFixtureDaemon {
	t.Helper()
	daemon := &historyFixtureDaemon{}
	byID := make(map[string]conversationFixture, len(conversations))
	listing := integrations.HistoryResponse{SchemaVersion: integrations.SchemaVersion}
	for _, conversation := range conversations {
		byID[conversation.session.ID] = conversation
		listing.Sessions = append(listing.Sessions, conversation.session)
	}
	daemon.server = httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		daemon.mu.Lock()
		daemon.requests = append(daemon.requests, request.Method+" "+request.URL.Path)
		daemon.mu.Unlock()
		switch {
		case request.URL.Path == "/api/sessions":
			_ = json.NewEncoder(response).Encode(sessionsResponse{Sessions: live})
		case request.URL.Path == "/api/history":
			_ = json.NewEncoder(response).Encode(listing)
		case request.URL.Path == "/api/search":
			daemon.mu.Lock()
			answer := daemon.search
			daemon.mu.Unlock()
			_ = json.NewEncoder(response).Encode(answer)
		case strings.HasSuffix(request.URL.Path, "/preview"):
			id := strings.TrimSuffix(strings.TrimPrefix(request.URL.Path, "/api/history/"), "/preview")
			conversation, known := byID[id]
			if !known {
				http.NotFound(response, request)
				return
			}
			_ = json.NewEncoder(response).Encode(integrations.TranscriptResponse{
				SchemaVersion: integrations.SchemaVersion,
				Session:       conversation.session, Messages: conversation.messages,
			})
		default:
			http.NotFound(response, request)
		}
	}))
	t.Cleanup(daemon.server.Close)
	return daemon
}

func (d *historyFixtureDaemon) methods() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func conversationAt(id, name, tool, cwd string, messages int, at time.Time) integrations.HistorySession {
	return integrations.HistorySession{
		ID: id, Name: name, Tool: tool, CWD: cwd, MessageCount: messages,
		LastActivityAt: at.UnixMilli(), ConversationUpdatedAt: at.UnixMilli(),
		ConversationAvailable: true,
	}
}

func runHistoryCLI(t *testing.T, daemon *historyFixtureDaemon, args ...string) (string, string, int) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"--host", daemon.server.URL}, args...),
		strings.NewReader(""), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}

// The complaint this command exists for: a conversation started somewhere else
// and closed, which the user cannot find again. Browsing must need no search
// term, must order by recency, and must hand back a command per row.
func TestHistoryBrowsesWithoutAQuery(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:new", "ship the release", "codex", "/w/one", 12, now.Add(-2*time.Hour))},
		{session: conversationAt("provider:codex:old", "older codex work", "codex", "/w/two", 4, now.Add(-40*time.Hour))},
		{session: conversationAt("provider:claude:mid", "claude thing", "claude", "/w/three", 7, now.Add(-6*time.Hour))},
	})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "--tool", "codex")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "ship the release") || !strings.Contains(stdout, "older codex work") {
		t.Fatalf("browse dropped a codex conversation: %q", stdout)
	}
	if strings.Contains(stdout, "claude thing") {
		t.Fatalf("--tool codex leaked a claude conversation: %q", stdout)
	}
	if strings.Index(stdout, "ship the release") > strings.Index(stdout, "older codex work") {
		t.Fatalf("rows are not newest-first: %q", stdout)
	}
	if !strings.Contains(stdout, "sessions resume provider:codex:new") {
		t.Fatalf("row carried no resume command: %q", stdout)
	}
	// Enough to recognise it without opening it.
	for _, want := range []string{"12 messages", "codex", "/w/one"} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("row is missing %q: %q", want, stdout)
		}
	}
}

// A conversation that is running right now is not resumable — Sessions' own
// guard refuses it and says to attach — so the browser must print attach for
// it. Printing resume there would be a command that fails on exactly the
// conversation a user is most likely to pick.
func TestHistoryPrintsAttachForALiveConversation(t *testing.T) {
	now := time.Now()
	const id = "11111111-2222-4333-8444-555555555555"
	daemon := newHistoryFixtureDaemon(t,
		[]session{{ID: id, Cmd: "claude", Tool: "claude-code"}},
		[]conversationFixture{
			{session: conversationAt(id, "running now", "claude", "/w/live", 9, now)},
		})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "sessions resume") {
		t.Fatalf("live conversation offered resume, which the daemon refuses: %q", stdout)
	}
	if !strings.Contains(stdout, "sessions attach 11111111") || !strings.Contains(stdout, "LIVE NOW") {
		t.Fatalf("live conversation was not marked or not attachable: %q", stdout)
	}
}

// A conversation nothing still holds gets no command at all and says why,
// matching `sessions recover`'s rule that a printed command is one that works.
func TestHistoryRefusesToInventACommandForALostConversation(t *testing.T) {
	now := time.Now()
	lost := conversationAt("provider:claude:gone", "pruned away", "claude", "/w/gone", 3, now)
	lost.ConversationAvailable = false
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{{session: lost}})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "--all")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Contains(stdout, "sessions resume") {
		t.Fatalf("unrecoverable conversation was offered a resume command: %q", stdout)
	}
	if !strings.Contains(stdout, "cannot be resumed") {
		t.Fatalf("unrecoverable conversation gave no reason: %q", stdout)
	}
	// And it is not in the default view at all, which lists what you can return to.
	stdout, _, _ = runHistoryCLI(t, daemon, "history")
	if strings.Contains(stdout, "pruned away") {
		t.Fatalf("default browse listed a conversation nothing can reopen: %q", stdout)
	}
	if !strings.Contains(stdout, "1 conversations are recorded") {
		t.Fatalf("empty browse hid the fact that history exists: %q", stdout)
	}
}

// Recency has to follow the conversation, not the record. One housekeeping
// sweep can stamp every finished session's record with the same instant; the
// transcripts did not change, and it is the transcripts the user remembers.
func TestHistoryOrdersByConversationActivityNotRecordActivity(t *testing.T) {
	now := time.Now()
	real := conversationAt("provider:codex:real", "the one you want", "codex", "/w/real", 2065, now.Add(-6*time.Hour))
	swept := conversationAt("sess-swept", "finished test lane", "codex", "/w/testing", 2, now.Add(-9*time.Hour))
	// The sweep moved the record forward; the conversation itself did not move.
	swept.LastActivityAt = now.Add(-1 * time.Minute).UnixMilli()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{{session: swept}, {session: real}})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if strings.Index(stdout, "the one you want") > strings.Index(stdout, "finished test lane") {
		t.Fatalf("record housekeeping outranked real conversation activity: %q", stdout)
	}
}

// Preview reads; it must not create or mark anything.
func TestHistoryPreviewShowsTheTailAndWritesNothing(t *testing.T) {
	now := time.Now()
	timestamp := "2026-08-05T12:00:00Z"
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{{
		session: conversationAt("provider:codex:one", "candidate", "codex", "/w/one", 4, now),
		messages: []integrations.TranscriptMessage{
			{Role: "user", Text: "first thing"},
			{Role: "tool", Kind: "bash", Text: "noise that is not a conversation"},
			{Role: "assistant", Text: "middle answer"},
			{Role: "user", Text: "last question", Timestamp: &timestamp},
			{Role: "assistant", Text: "last answer"},
		},
	}})
	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "--preview", "2")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "last question") || !strings.Contains(stdout, "last answer") {
		t.Fatalf("preview did not show the end of the conversation: %q", stdout)
	}
	if strings.Contains(stdout, "first thing") {
		t.Fatalf("--preview 2 showed more than two exchanges: %q", stdout)
	}
	if strings.Contains(stdout, "noise that is not a conversation") {
		t.Fatalf("preview showed tool traffic rather than the conversation: %q", stdout)
	}
	for _, request := range daemon.methods() {
		if !strings.HasPrefix(request, "GET ") {
			t.Fatalf("browsing or previewing issued %q; it must only read", request)
		}
	}
}

// A conversation is not a message. When a query narrows the browse, the answer
// is still one row per conversation, and it must survive a daemon whose search
// route does not send the per-session rollup.
func TestHistoryQueryNarrowsToConversationsWithoutARollup(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:hit", "the hardening lane", "codex", "/w/hit", 30, now)},
		{session: conversationAt("provider:codex:miss", "unrelated", "codex", "/w/miss", 5, now.Add(-time.Hour))},
	})
	// Deliberately no Sessions rollup, the way an older daemon answers.
	daemon.search = historysearch.Response{
		Matches: []historysearch.Match{
			{SessionID: "provider:codex:hit", Name: "the hardening lane", Snippet: "[[hardening]] the runner"},
			{SessionID: "provider:codex:hit", Name: "the hardening lane", Snippet: "more [[hardening]]"},
		},
		Total: 2,
	}
	stdout, stderr, code := runHistoryCLI(t, daemon, "history", "hardening")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	if !strings.Contains(stdout, "the hardening lane") {
		t.Fatalf("query dropped the conversation that matched: %q", stdout)
	}
	if strings.Contains(stdout, "unrelated") {
		t.Fatalf("query kept a conversation that did not match: %q", stdout)
	}
	if strings.Count(stdout, "sessions resume provider:codex:hit") != 1 {
		t.Fatalf("two matching messages produced more than one conversation row: %q", stdout)
	}
	if !strings.Contains(stdout, "1 conversation matches") {
		t.Fatalf("footer did not report the conversation count: %q", stdout)
	}
}

// The rollup is the better source when it is there: it counts every hit rather
// than the ones that reached the page.
func TestHistoryQueryPrefersTheSessionRollupWhenPresent(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:hit", "the hardening lane", "codex", "/w/hit", 30, now)},
	})
	daemon.search = historysearch.Response{
		Sessions: []historysearch.SessionHits{{
			SessionID: "provider:codex:hit", Name: "the hardening lane", Hits: 42,
			Snippets: []string{"rollup snippet"},
		}},
	}
	stdout, _, code := runHistoryCLI(t, daemon, "history", "hardening")
	if code != 0 {
		t.Fatalf("exit=%d", code)
	}
	if !strings.Contains(stdout, "42 matching messages") || !strings.Contains(stdout, "rollup snippet") {
		t.Fatalf("rollup evidence was not used: %q", stdout)
	}
}

func TestHistoryJSONIsOneDocumentWithTheDocumentedShape(t *testing.T) {
	now := time.Now()
	daemon := newHistoryFixtureDaemon(t, nil, []conversationFixture{
		{session: conversationAt("provider:codex:one", "candidate", "codex", "/w/one", 4, now)},
		{session: conversationAt("provider:codex:two", "other", "codex", "/w/two", 6, now.Add(-time.Hour))},
	})
	stdout, stderr, code := runHistoryCLI(t, daemon, "--json", "history", "-n", "1")
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
	decoder := json.NewDecoder(strings.NewReader(stdout))
	var answer historyBrowseResponse
	if err := decoder.Decode(&answer); err != nil {
		t.Fatalf("history --json was not decodable: %v (%q)", err, stdout)
	}
	if decoder.More() {
		t.Fatal("history --json wrote more than one document")
	}
	if answer.SchemaVersion != integrations.SchemaVersion {
		t.Fatalf("schemaVersion = %d", answer.SchemaVersion)
	}
	if answer.Known != 2 || answer.Matched != 2 || answer.Shown != 1 {
		t.Fatalf("counts = known %d matched %d shown %d, want 2/2/1", answer.Known, answer.Matched, answer.Shown)
	}
	if len(answer.Conversations) != 1 {
		t.Fatalf("conversations = %d, want 1", len(answer.Conversations))
	}
	row := answer.Conversations[0]
	if row.Status != historyStatusResumable ||
		strings.Join(row.Resume, " ") != "sessions resume provider:codex:one" {
		t.Fatalf("row = %#v", row)
	}
	if row.LastActiveAt == "" || row.LastActiveAtMS == 0 || row.Messages != 4 {
		t.Fatalf("row lacks what identifies it: %#v", row)
	}
}

func TestHistoryJSONFailureCarriesTheExitCode(t *testing.T) {
	daemon := newHistoryFixtureDaemon(t, nil, nil)
	stdout, _, code := runHistoryCLI(t, daemon, "--json", "history", "--tool", "emacs")
	if code == 0 {
		t.Fatal("an invalid --tool exited 0")
	}
	var failure struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Code  int    `json:"code"`
	}
	if err := json.Unmarshal([]byte(stdout), &failure); err != nil {
		t.Fatalf("failure was not JSON: %v (%q)", err, stdout)
	}
	if failure.OK || failure.Code != code || failure.Error == "" {
		t.Fatalf("failure document = %#v, exit %d", failure, code)
	}
}

// `sessions search --since today --tool codex` used to be refused as a typo:
// "the query must come before flags". The user asking it cannot supply a word,
// because not remembering one is why they are asking. The refusal now hands
// back the command that answers the question.
func TestSearchWithOnlyFiltersHandsBackTheBrowseCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run([]string{"search", "--since", "today", "--tool", "codex"},
		strings.NewReader(""), &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "sessions history --since today --tool codex") {
		t.Fatalf("refusal did not hand back a working command: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "must come before flags") {
		t.Fatalf("refusal still reads as a syntax complaint: %q", stderr.String())
	}
}

// A real mistyped query keeps its old, correct explanation.
func TestSearchStillExplainsAFlagUsedAsTheQuery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"search", "--nonsense", "words"}, strings.NewReader(""), &stdout, &stderr); code != 1 {
		t.Fatalf("exit=%d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "must come before flags") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestParseHistoryTimeUnderstandsHowPeopleSayWhen(t *testing.T) {
	now := time.Date(2026, 8, 5, 21, 30, 0, 0, time.Local)
	startOfToday := time.Date(2026, 8, 5, 0, 0, 0, 0, time.Local)
	tests := []struct {
		raw      string
		endOfDay bool
		want     time.Time
	}{
		{raw: "today", want: startOfToday},
		{raw: "yesterday", want: startOfToday.AddDate(0, 0, -1)},
		{raw: "today", endOfDay: true, want: startOfToday.AddDate(0, 0, 1)},
		{raw: "3d", want: now.Add(-72 * time.Hour)},
		{raw: "6h", want: now.Add(-6 * time.Hour)},
		{raw: "2w", want: now.Add(-14 * 24 * time.Hour)},
		{raw: "2026-07-01", want: time.Date(2026, 7, 1, 0, 0, 0, 0, time.Local)},
		{raw: "2026-07-01T10:00:00Z", want: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)},
	}
	for _, test := range tests {
		got, label, err := parseHistoryTime(test.raw, now, test.endOfDay)
		if err != nil {
			t.Fatalf("%q: %v", test.raw, err)
		}
		if got != test.want.UnixMilli() {
			t.Fatalf("%q = %d, want %d", test.raw, got, test.want.UnixMilli())
		}
		if label == "" {
			t.Fatalf("%q produced no description of the bound", test.raw)
		}
	}
	if _, _, err := parseHistoryTime("last tuesday", now, false); err == nil {
		t.Fatal("an unreadable date was accepted")
	}
}

// --preview alone is one word; --preview 8 is the same option with the count
// given. A following flag must never be swallowed as the count.
func TestPluckOptionalCountKeepsFollowingFlags(t *testing.T) {
	args := []string{"--preview", "--tool", "codex"}
	count, present, err := pluckOptionalCount(&args, "--preview", 4, 20)
	if err != nil || !present || count != 4 {
		t.Fatalf("count=%d present=%v err=%v", count, present, err)
	}
	if strings.Join(args, " ") != "--tool codex" {
		t.Fatalf("remaining args = %q", args)
	}
	args = []string{"--preview", "8", "--tool", "codex"}
	count, present, err = pluckOptionalCount(&args, "--preview", 4, 20)
	if err != nil || !present || count != 8 {
		t.Fatalf("count=%d present=%v err=%v", count, present, err)
	}
	if strings.Join(args, " ") != "--tool codex" {
		t.Fatalf("remaining args = %q", args)
	}
	args = []string{"--preview", "999"}
	if _, _, err = pluckOptionalCount(&args, "--preview", 4, 20); err == nil {
		t.Fatal("an out-of-range count was accepted")
	}
}
