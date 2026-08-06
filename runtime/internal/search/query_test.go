package search

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

// A sentence is not a boolean expression. The old builder ORed every word
// including the stopwords, which on a real corpus matched 73% of every message
// indexed and let a row whose only merit was the word "the" rank second.
func TestSearchQueryIsConjunctiveAndDropsStopwords(t *testing.T) {
	parsed, err := parseSearchQuery("the session where we fixed the auth bug", false)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := parsed.strictExpression(), `"session" AND "fixed" AND "auth" AND "bug"`; got != want {
		t.Fatalf("strict expression = %q, want %q", got, want)
	}
	for _, term := range parsed.required {
		if searchStopwords[strings.ToLower(term.text)] {
			t.Fatalf("stopword %q survived into the required terms", term.text)
		}
	}
}

// A query of nothing but stopwords is still a query someone meant.
func TestSearchQueryKeepsStopwordsWhenTheyAreAllThereIs(t *testing.T) {
	parsed, err := parseSearchQuery("the", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.strictExpression(); got != `"the"` {
		t.Fatalf("expression = %q", got)
	}
}

// Bare AND, OR, and NOT are words in a sentence far more often than they are
// operators, and treating them as operators rewrote the whole query silently.
func TestSearchQueryTreatsProseOperatorsAsProse(t *testing.T) {
	withOperator, err := parseSearchQuery("lanes AND worktrees cleanup", false)
	if err != nil {
		t.Fatal(err)
	}
	without, err := parseSearchQuery("lanes worktrees cleanup", false)
	if err != nil {
		t.Fatal(err)
	}
	if withOperator.strictExpression() != without.strictExpression() {
		t.Fatalf("prose operator changed the query: %q vs %q",
			withOperator.strictExpression(), without.strictExpression())
	}
	raw, err := parseSearchQuery("fts:lanes AND worktrees cleanup", false)
	if err != nil {
		t.Fatal(err)
	}
	if !raw.raw || raw.rawExpr != "lanes AND worktrees cleanup" {
		t.Fatalf("raw opt-in = %#v", raw)
	}
}

// A pasted path is one whitespace-delimited token, so quoting it whole demands
// the tokens be contiguous — which fails for exactly the case people paste: an
// absolute path whose tail is the repo-relative path in the transcript.
func TestSearchQueryExpandsPastedPaths(t *testing.T) {
	for _, test := range []struct{ query, want string }{
		{query: "/Users/uzair/files.go", want: `("/Users/uzair/files.go" OR "uzair/files.go" OR "files.go")`},
		{
			query: "runtime/internal/api/files.go",
			want:  `("runtime/internal/api/files.go" OR "internal/api/files.go" OR "api/files.go" OR "files.go")`,
		},
		{query: "files.go", want: `"files.go"`},
	} {
		t.Run(test.query, func(t *testing.T) {
			parsed, err := parseSearchQuery(test.query, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := parsed.strictExpression(); got != test.want {
				t.Fatalf("expression = %q, want %q", got, test.want)
			}
		})
	}
}

// The prefix marker has to sit outside the quotes. Inside them FTS5 reads it as
// punctuation and narrows silently to the exact word.
func TestSearchQueryKeepsPrefixMarkerOutsideTheQuotes(t *testing.T) {
	parsed, err := parseSearchQuery("auth*", false)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.strictExpression(); got != `"auth"*` {
		t.Fatalf("expression = %q", got)
	}
}

// A stray quote in pasted text is not a reason to refuse the search.
func TestSearchQueryToleratesAnUnterminatedQuote(t *testing.T) {
	parsed, err := parseSearchQuery(`deploy "first name`, false)
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.strictExpression(); got != `"deploy" AND "first name"` {
		t.Fatalf("expression = %q", got)
	}
}

func TestSearchQueryRejectsATermlessQuery(t *testing.T) {
	for _, query := range []string{"   ", "--", "!!"} {
		if _, err := parseSearchQuery(query, false); err == nil || !IsOptionError(err) {
			t.Fatalf("query %q error = %v, want an option error", query, err)
		}
	}
}

func TestRankedSearchExpandsPrefixTerms(t *testing.T) {
	fixture := rankedFixture("the authentication handshake", "unrelated body")
	result := runRankedFixture(t, fixture, "authent*", filepath.Join(t.TempDir(), "search-index.db"))
	if result.Total != 1 || result.Matches[0].Text != "the authentication handshake" {
		t.Fatalf("prefix result = %#v", result)
	}
}

// An absolute path pasted from one machine has to find the repo-relative path
// the transcript actually recorded.
func TestRankedSearchFindsAPastedAbsolutePath(t *testing.T) {
	fixture := rankedFixture("the fix landed in runtime/internal/api/files.go today")
	result := runRankedFixture(t, fixture, "/Users/someone/api/files.go", filepath.Join(t.TempDir(), "search-index.db"))
	if result.Total != 1 {
		t.Fatalf("path result = %#v (effective %q)", result, result.EffectiveQuery)
	}
}

// The most direct reading of "always find a session you lost": a session's own
// title has to return that session, even when its transcript never says those
// words.
func TestRankedSearchFindsASessionByItsOwnTitle(t *testing.T) {
	fixture := namedRankedFixture(
		"Sessions cleanup architecture", "/repo/sessions",
		"unrelated chatter about lunch", "more unrelated chatter",
	)
	result := runRankedFixture(t, fixture, "Sessions cleanup architecture", filepath.Join(t.TempDir(), "search-index.db"))
	if result.Total == 0 {
		t.Fatalf("title search returned nothing: %#v", result)
	}
	if len(result.Sessions) == 0 || !result.Sessions[0].TitleMatch ||
		result.Sessions[0].Name != "Sessions cleanup architecture" {
		t.Fatalf("rollup = %#v", result.Sessions)
	}
}

// A title match has to outrank a session that merely mentions the same words.
func TestRankedSearchRanksTitleMatchesAboveBodyMentions(t *testing.T) {
	titled := integrations.HistorySession{
		ID: "11111111-1111-4222-8333-444444444444", Name: "kryptonite rollout", Tool: "claude",
		CWD: "/repo/one", ConversationAvailable: true,
	}
	mentioning := integrations.HistorySession{
		ID: "22222222-1111-4222-8333-444444444444", Name: "unrelated", Tool: "codex",
		CWD: "/repo/two", ConversationAvailable: true,
	}
	fixture := &fakeHistory{
		sessions: []integrations.HistorySession{titled, mentioning},
		transcript: map[string]integrations.TranscriptResponse{
			titled.ID: {Messages: []integrations.TranscriptMessage{{Role: "user", Text: "let us begin"}}},
			mentioning.ID: {Messages: []integrations.TranscriptMessage{
				{Role: "user", Text: strings.Repeat("filler ", 40) + "kryptonite rollout was mentioned once"},
			}},
		},
	}
	result := runRankedFixture(t, fixture, "kryptonite rollout", filepath.Join(t.TempDir(), "search-index.db"))
	if len(result.Matches) != 2 || result.Matches[0].SessionID != titled.ID {
		t.Fatalf("title match did not rank first: %#v", result.Matches)
	}
	if result.Sessions[0].SessionID != titled.ID || !result.Sessions[0].TitleMatch {
		t.Fatalf("rollup = %#v", result.Sessions)
	}
}

// The working directory is the other string people remember about a session.
func TestRankedSearchFindsASessionByItsWorkingDirectory(t *testing.T) {
	fixture := namedRankedFixture("nameless", "/repo/brightroom", "unrelated chatter")
	result := runRankedFixture(t, fixture, "brightroom", filepath.Join(t.TempDir(), "search-index.db"))
	if result.Total == 0 {
		t.Fatalf("cwd search returned nothing (effective %q)", result.EffectiveQuery)
	}
}

// A caller cannot tell a real absence from a badly phrased query unless the
// response says what actually ran.
func TestRankedSearchReportsTheEffectiveQueryAndMode(t *testing.T) {
	fixture := rankedFixture("alpha beta gamma")
	result := runRankedFixture(t, fixture, "the alpha and beta", filepath.Join(t.TempDir(), "search-index.db"))
	if result.EffectiveQuery != `"alpha" AND "beta"` || result.MatchMode != "strict" {
		t.Fatalf("effective query = %q mode = %q", result.EffectiveQuery, result.MatchMode)
	}
}

// A conjunction that matches nothing must relax rather than return nothing:
// one wrong word in a remembered sentence cannot cost the whole search.
func TestRankedSearchRelaxesAnEmptyConjunction(t *testing.T) {
	fixture := rankedFixture("worktree cleanup notes")
	indexPath := filepath.Join(t.TempDir(), "search-index.db")
	result := runRankedFixture(t, fixture, "worktree cleanup zzzznotinthecorpus", indexPath)
	if result.Total != 1 || result.MatchMode != "quorum" {
		t.Fatalf("relaxed result = %#v", result)
	}
	if !strings.Contains(result.EffectiveQuery, "OR") {
		t.Fatalf("quorum expression = %q, want an optional group", result.EffectiveQuery)
	}
	// Two terms cannot form a meaningful quorum, so they fall straight to the
	// broad rung rather than to an anchor that is a strict subset of it.
	result = runRankedFixture(t, fixture, "cleanup zzzznotinthecorpus", indexPath)
	if result.Total != 1 || result.MatchMode != "broad" {
		t.Fatalf("broad result = %#v", result)
	}
}

// A score that depends on how many rows were asked for is a score nobody can
// threshold on.
func TestRankedSearchScoresAreStableAcrossPageSizes(t *testing.T) {
	texts := make([]string, 0, 12)
	for index := 0; index < 12; index++ {
		texts = append(texts, strings.Repeat("marker ", index+1)+"filler")
	}
	fixture := rankedFixture(texts...)
	indexPath := filepath.Join(t.TempDir(), "search-index.db")
	small := runRankedLimit(t, fixture, "marker", indexPath, 3)
	large := runRankedLimit(t, fixture, "marker", indexPath, 12)
	if small.Total != 3 || large.Total != 12 {
		t.Fatalf("totals = %d and %d", small.Total, large.Total)
	}
	byMessage := make(map[int]float64, len(large.Matches))
	for _, match := range large.Matches {
		byMessage[match.MessageIndex] = match.Score
	}
	for _, match := range small.Matches {
		if byMessage[match.MessageIndex] != match.Score {
			t.Fatalf("message %d scored %v on a page of 3 and %v on a page of 12",
				match.MessageIndex, match.Score, byMessage[match.MessageIndex])
		}
	}
	last := large.Matches[len(large.Matches)-1]
	if last.Score <= 0 {
		t.Fatalf("last match scored %v; a matching row is never zero-relevant", last.Score)
	}
	if last.Score >= 1 {
		t.Fatalf("last match scored %v; the scale is bounded above by 1", last.Score)
	}
}

// A distinctive term has to score visibly above a term every message contains,
// or the number is decoration rather than a signal.
func TestRankedSearchScoresRankSelectivityAbsolutely(t *testing.T) {
	texts := make([]string, 0, 10)
	for index := 0; index < 9; index++ {
		texts = append(texts, "common filler text")
	}
	texts = append(texts, "common filler text with a distinctive kryptonite")
	fixture := rankedFixture(texts...)
	indexPath := filepath.Join(t.TempDir(), "search-index.db")
	rare := runRankedFixture(t, fixture, "kryptonite", indexPath)
	common := runRankedFixture(t, fixture, "filler", indexPath)
	if rare.Total != 1 || common.Total != 10 {
		t.Fatalf("totals = %d and %d", rare.Total, common.Total)
	}
	if rare.Matches[0].Score <= common.Matches[0].Score {
		t.Fatalf("rare term scored %v, common term scored %v",
			rare.Matches[0].Score, common.Matches[0].Score)
	}
}

// The question is "which session", and a page of messages cannot answer it.
func TestRankedSearchRollsMatchesUpToSessions(t *testing.T) {
	fixture := multiSessionRankedFixture()
	result := runRankedFixture(t, fixture, "marker", filepath.Join(t.TempDir(), "search-index.db"))
	if result.TotalSessions != 2 || len(result.Sessions) != 2 {
		t.Fatalf("rollup = %#v", result.Sessions)
	}
	byID := map[string]SessionHits{}
	for _, session := range result.Sessions {
		byID[session.SessionID] = session
	}
	noisy := byID["11111111-1111-4222-8333-444444444444"]
	if noisy.Hits != 4 || len(noisy.Snippets) == 0 || noisy.CWD != "/repo/noisy" {
		t.Fatalf("noisy rollup = %#v", noisy)
	}
	if noisy.FirstHitAt == "" || noisy.LastHitAt == "" || noisy.FirstHitAt >= noisy.LastHitAt {
		t.Fatalf("rollup window = %q..%q", noisy.FirstHitAt, noisy.LastHitAt)
	}
	if quiet := byID["22222222-1111-4222-8333-444444444444"]; quiet.Hits != 1 {
		t.Fatalf("quiet rollup = %#v", quiet)
	}
}

// One busy session must not own the page and hide every other session that
// answered the query.
func TestRankedSearchSpreadsThePageAcrossSessions(t *testing.T) {
	fixture := multiSessionRankedFixture()
	result := runRankedLimit(t, fixture, "marker", filepath.Join(t.TempDir(), "search-index.db"), 4)
	sessions := map[string]bool{}
	for _, match := range result.Matches {
		sessions[match.SessionID] = true
	}
	if result.Total != 4 || len(sessions) != 2 {
		t.Fatalf("page covered %d sessions in %d matches: %#v", len(sessions), result.Total, result.Matches)
	}
}

func runRankedLimit(t *testing.T, fixture *fakeHistory, query, indexPath string, limit int) Response {
	t.Helper()
	result, err := Run(context.Background(), fixture, nil, Options{
		Query: query, Ranked: true, Limit: limit,
	}, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func namedRankedFixture(name, cwd string, texts ...string) *fakeHistory {
	session := integrations.HistorySession{
		ID: "cccccccc-1111-4222-8333-444444444444", Name: name, Tool: "codex", CWD: cwd,
		ProviderSessionID:     "codex-provider-ranked",
		ConversationAvailable: true,
	}
	messages := make([]integrations.TranscriptMessage, 0, len(texts))
	for _, text := range texts {
		messages = append(messages, integrations.TranscriptMessage{Role: "user", Text: text})
	}
	return &fakeHistory{
		sessions:   []integrations.HistorySession{session},
		transcript: map[string]integrations.TranscriptResponse{session.ID: {Messages: messages}},
	}
}

func multiSessionRankedFixture() *fakeHistory {
	noisy := integrations.HistorySession{
		ID: "11111111-1111-4222-8333-444444444444", Name: "noisy", Tool: "claude",
		CWD: "/repo/noisy", ConversationAvailable: true,
	}
	quiet := integrations.HistorySession{
		ID: "22222222-1111-4222-8333-444444444444", Name: "quiet", Tool: "codex",
		CWD: "/repo/quiet", ConversationAvailable: true,
	}
	stamps := []string{
		"2026-07-01T10:00:00Z", "2026-07-01T11:00:00Z",
		"2026-07-01T12:00:00Z", "2026-07-01T13:00:00Z",
	}
	noisyMessages := make([]integrations.TranscriptMessage, 0, 4)
	for index := range stamps {
		at := stamps[index]
		noisyMessages = append(noisyMessages, integrations.TranscriptMessage{
			Role: "user", Timestamp: &at, Text: "marker in the noisy session",
		})
	}
	quietStamp := "2026-07-02T10:00:00Z"
	return &fakeHistory{
		sessions: []integrations.HistorySession{noisy, quiet},
		transcript: map[string]integrations.TranscriptResponse{
			noisy.ID: {Messages: noisyMessages},
			quiet.ID: {Messages: []integrations.TranscriptMessage{
				{Role: "user", Timestamp: &quietStamp, Text: "one marker in the quiet session"},
			}},
		},
	}
}
