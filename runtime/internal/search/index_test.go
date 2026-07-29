package search

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

func TestRankedSearchOrdersByBM25(t *testing.T) {
	fixture := rankedFixture("alpha once", "alpha alpha twice")
	result := runRankedFixture(t, fixture, "alpha", filepath.Join(t.TempDir(), "search-index.db"))
	if result.Total != 2 || result.Matches[0].Text != "alpha alpha twice" {
		t.Fatalf("ranked result = %#v", result)
	}
}

func TestRankedSearchStemsTerms(t *testing.T) {
	for _, test := range []struct {
		query string
		text  string
	}{
		{query: "email", text: "the inbox contains emails"},
		{query: "emails", text: "send an email"},
	} {
		t.Run(test.query, func(t *testing.T) {
			fixture := rankedFixture(test.text)
			result := runRankedFixture(t, fixture, test.query, filepath.Join(t.TempDir(), "search-index.db"))
			if result.Total != 1 || result.Matches[0].Text != test.text {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestRankedSearchMatchesPhrases(t *testing.T) {
	fixture := rankedFixture("the first name is Ada", "the first middle name is Ada")
	result := runRankedFixture(t, fixture, `"first name"`, filepath.Join(t.TempDir(), "search-index.db"))
	if result.Total != 1 || result.Matches[0].Text != "the first name is Ada" {
		t.Fatalf("phrase result = %#v", result)
	}
}

func TestRankedSearchUsesBroadTokensByDefaultAndSupportsProximity(t *testing.T) {
	fixture := rankedFixture(
		"the founder called the default asinine",
		"dev mode should be paid only",
		"alpha one two beta",
		"alpha one two three four five beta",
	)
	indexPath := filepath.Join(t.TempDir(), "search-index.db")
	result := runRankedFixture(t, fixture, "asinine dev", indexPath)
	if result.Total != 2 {
		t.Fatalf("broad token result=%#v", result)
	}
	result = runRankedFixture(t, fixture, "near(alpha,beta,3)", indexPath)
	if result.Total != 1 || result.Matches[0].Text != "alpha one two beta" {
		t.Fatalf("proximity result=%#v", result)
	}
}

func TestRankedSearchReturnsScoreAnchorAndContext(t *testing.T) {
	fixture := rankedFixture("before marker", "alpha alpha target", "after marker")
	result, err := Run(context.Background(), fixture, nil, Options{
		Query: "alpha", Ranked: true, Context: 1,
	}, filepath.Join(t.TempDir(), "search-index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Matches[0].MessageIndex != 1 ||
		result.Matches[0].ProviderSessionID != "codex-provider-ranked" || result.Matches[0].Score != 1 ||
		len(result.Matches[0].ContextBefore) != 1 || len(result.Matches[0].ContextAfter) != 1 ||
		result.Matches[0].ContextBefore[0].Text != "before marker" ||
		result.Matches[0].ContextAfter[0].Text != "after marker" {
		t.Fatalf("anchored result=%#v", result)
	}
}

func TestRankedSearchSupportsBooleanNot(t *testing.T) {
	fixture := rankedFixture("alpha gamma", "alpha beta")
	result := runRankedFixture(t, fixture, "alpha NOT beta", filepath.Join(t.TempDir(), "search-index.db"))
	if result.Total != 1 || result.Matches[0].Text != "alpha gamma" {
		t.Fatalf("boolean result = %#v", result)
	}
}

func TestRankedSearchRefreshRemovesStaleRows(t *testing.T) {
	fixture := rankedFixture("stale marker")
	indexPath := filepath.Join(t.TempDir(), "search-index.db")
	if result := runRankedFixture(t, fixture, "stale", indexPath); result.Total != 1 {
		t.Fatalf("initial result = %#v", result)
	}
	fixture.transcript[fixture.sessions[0].ID] = integrations.TranscriptResponse{
		Messages: []integrations.TranscriptMessage{{Role: "user", Text: "fresh marker"}},
	}
	if result := runRankedFixture(t, fixture, "fresh", indexPath); result.Total != 1 {
		t.Fatalf("fresh result = %#v", result)
	}
	if result := runRankedFixture(t, fixture, "stale", indexPath); result.Total != 0 || result.Matches == nil {
		t.Fatalf("stale result = %#v", result)
	}
}

func TestRankedSearchReusesUnchangedPersistentSession(t *testing.T) {
	fixture := rankedFixture("the durable index avoids reparsing giant transcripts")
	fixture.sessions[0].SourceFingerprint = "unchanged-source"
	indexPath := filepath.Join(t.TempDir(), "search-index.db")

	if result := runRankedFixture(t, fixture, "giant", indexPath); result.Total != 1 {
		t.Fatalf("initial result = %#v", result)
	}
	if result := runRankedFixture(t, fixture, "durable", indexPath); result.Total != 1 {
		t.Fatalf("reused result = %#v", result)
	}
	if len(fixture.limits) != 1 {
		t.Fatalf("transcript parsed %d times, want once", len(fixture.limits))
	}
}

func TestRankedSearchPurgesUnavailableSessionText(t *testing.T) {
	fixture := rankedFixture("private transcript marker")
	fixture.sessions[0].SourceFingerprint = "available-source"
	indexPath := filepath.Join(t.TempDir(), "search-index.db")
	if result := runRankedFixture(t, fixture, "private", indexPath); result.Total != 1 {
		t.Fatalf("initial result = %#v", result)
	}
	fixture.sessions[0].ConversationAvailable = false
	if result := runRankedFixture(t, fixture, "private", indexPath); result.Total != 0 {
		t.Fatalf("removed result = %#v", result)
	}
	fixture.sessions[0].ConversationAvailable = true
	if result := runRankedFixture(t, fixture, "private", indexPath); result.Total != 1 {
		t.Fatalf("restored result = %#v", result)
	}
	if len(fixture.limits) != 2 {
		t.Fatalf("transcript parsed %d times, want reparse after purge", len(fixture.limits))
	}
}

func TestDefaultSearchKeepsPunctuationLiteral(t *testing.T) {
	fixture := rankedFixture("render {{first_name}} exactly")
	result, err := Run(context.Background(), fixture, nil, Options{Query: "{{first_name}}"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || !strings.Contains(result.Matches[0].Snippet, "[[{{first_name}}]]") {
		t.Fatalf("substring result = %#v", result)
	}
}

func TestRankedSearchRejectsMalformedAndRegexQueries(t *testing.T) {
	fixture := rankedFixture("alpha")
	indexPath := filepath.Join(t.TempDir(), "search-index.db")
	for _, options := range []Options{
		{Query: `"unterminated`, Ranked: true},
		{Query: "alpha", Ranked: true, Regex: true},
	} {
		if _, err := Run(context.Background(), fixture, nil, options, indexPath); err == nil || !IsOptionError(err) {
			t.Fatalf("options %#v error = %v, want option error", options, err)
		}
	}
}

func rankedFixture(texts ...string) *fakeHistory {
	session := integrations.HistorySession{
		ID: "cccccccc-1111-4222-8333-444444444444", Name: "ranked", Tool: "codex",
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

func runRankedFixture(t *testing.T, fixture *fakeHistory, query, indexPath string) Response {
	t.Helper()
	result, err := Run(context.Background(), fixture, nil, Options{Query: query, Ranked: true}, indexPath)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
