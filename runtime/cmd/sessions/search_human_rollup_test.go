package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
)

// humanSearch runs one search against a daemon that answers with response, and
// returns what a person sees. The rollup fields are what `--json` has carried
// since the search engine started computing them; this is the same daemon
// answer read by someone who did not ask for JSON.
func humanSearch(t *testing.T, response historysearch.Response, args ...string) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(response)
	}))
	t.Cleanup(server.Close)
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	code := run(
		append([]string{"--host", server.URL, "search"}, args...),
		strings.NewReader(""), &stdout, &stderr,
	)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("search exit=%d stderr=%q", code, stderr.String())
	}
	return stdout.String()
}

func rollupMatch(sessionID, name string, index int) historysearch.Match {
	timestamp := "2026-08-01T12:00:00Z"
	return historysearch.Match{
		SessionID: sessionID, Name: name, Tool: "claude", Role: "user",
		MessageID: fmt.Sprintf("%s-%d", sessionID, index), MessageIndex: index,
		Timestamp: &timestamp, Snippet: "the [[auth]] rewrite",
	}
}

// A page of messages cannot say which conversations they came from without the
// reader grouping the rows by eye, and the hits that never reached the page
// were visible only to a --json caller. Both are in the rollup the daemon
// already sends.
func TestSearchHumanOutputNamesTheConversationsBehindTheMatches(t *testing.T) {
	output := humanSearch(t, historysearch.Response{
		Matches: []historysearch.Match{
			rollupMatch("aaaaaaaa-1111-4222-8333-444444444444", "auth rewrite", 0),
			rollupMatch("bbbbbbbb-1111-4222-8333-444444444444", "billing", 4),
		},
		Total: 2,
		Sessions: []historysearch.SessionHits{
			{SessionID: "aaaaaaaa-1111-4222-8333-444444444444", Name: "auth rewrite",
				Hits: 9, TitleMatch: true, Score: 0.9},
			{SessionID: "bbbbbbbb-1111-4222-8333-444444444444", Name: "billing", Hits: 3},
		},
		EffectiveQuery: `"auth"`, MatchMode: "strict",
		TotalHits: 12, TotalSessions: 2,
	}, "auth")

	footer := "12 matches · in 2 conversations · showing 2\n" +
		"  aaaaaaaa  auth rewrite  9 matches · title match\n" +
		"  bbbbbbbb  billing       3 matches\n"
	if !strings.HasSuffix(output, footer) {
		t.Fatalf("output = %q, want it to end with the session rollup %q", output, footer)
	}
	// The rollup is a summary, not a second browser: search's unit is still
	// the message, so the matches stay above it in full.
	if !strings.Contains(output, "the [[auth]] rewrite") {
		t.Fatalf("output = %q, want the message rows kept", output)
	}
}

// A truncated scan produces lower bounds. Printing them as totals would state
// as fact something the scan never established, and the reader would stop
// looking for the hits it never counted.
func TestSearchHumanRollupNeverPrintsAPartialCountAsComplete(t *testing.T) {
	output := humanSearch(t, historysearch.Response{
		Matches: []historysearch.Match{
			rollupMatch("aaaaaaaa-1111-4222-8333-444444444444", "auth rewrite", 0),
		},
		Total: 1,
		Sessions: []historysearch.SessionHits{
			{SessionID: "aaaaaaaa-1111-4222-8333-444444444444", Name: "auth rewrite", Hits: 40},
		},
		TotalHits: 40, TotalSessions: 1, RollupPartial: true,
	}, "auth")

	for _, want := range []string{"at least 40 matches", "in at least 1 conversation", "40 matches"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output = %q, want %q", output, want)
		}
	}
	if strings.Contains(output, "\n40 matches") {
		t.Fatalf("output = %q, want no bare total for a truncated scan", output)
	}
}

// The sentence must not claim the corpus contains something it does not. The
// ladder leaves a rung that matched a handful as readily as one that matched
// nothing -- on a sentence-length query a lone match is more likely to be a
// coincidence than the answer -- so a reader whose search DID match one message
// used to be told "No message had all of those words" about their own corpus.
// Being caught in one falsehood is enough to make the rest of the answer
// untrustworthy, which is the opposite of what this line is for.
func TestTheWideningNoticeDoesNotDenyAMatchThatExists(t *testing.T) {
	// A relaxed answer that nevertheless contains a message holding every term:
	// exactly the case the old wording described as "no message".
	output := humanSearch(t, historysearch.Response{
		Matches: []historysearch.Match{
			rollupMatch("aaaaaaaa-1111-4222-8333-444444444444", "auth rewrite", 0),
		},
		Total:          1,
		EffectiveQuery: `"auth" OR "rewrite"`, MatchMode: "broad",
		TotalHits: 1, TotalSessions: 1,
	}, "auth rewrite")
	for _, denial := range []string{"No message had", "no message had", "Nothing matched"} {
		if strings.Contains(output, denial) {
			t.Fatalf("the widening notice asserts an absence the ladder cannot know: %q", output)
		}
	}
	if !strings.Contains(output, "all of those words") {
		t.Fatalf("output = %q, want it still to say what was relaxed", output)
	}
}

// Relaxation is why a search for a phrase returns rows sharing one word with
// it. Receiving that silently reads as a broken search rather than a widened
// one, and the reader's next move is to distrust the tool instead of
// rephrasing.
func TestSearchHumanOutputSaysWhenTheSearchWidened(t *testing.T) {
	response := historysearch.Response{
		Matches: []historysearch.Match{
			rollupMatch("aaaaaaaa-1111-4222-8333-444444444444", "auth rewrite", 0),
		},
		Total:          1,
		EffectiveQuery: `"auth" OR "rewrite"`, MatchMode: "broad",
		TotalHits: 1, TotalSessions: 1,
	}
	output := humanSearch(t, response, "auth rewrite")
	if !strings.HasPrefix(output, "Too few messages had all of those words, so it looked for any one of them") {
		t.Fatalf("output = %q, want the widening said before the results it explains", output)
	}
	if !strings.Contains(output, `matched as: "auth" OR "rewrite"`) {
		t.Fatalf("output = %q, want the expression that actually ran", output)
	}

	response.MatchMode = "quorum"
	response.EffectiveQuery = `"auth" AND "rewrite"`
	if quorum := humanSearch(t, response, "auth rewrite"); !strings.HasPrefix(
		quorum, "Too few messages had all of those words, so it looked for the most distinctive of them") {
		t.Fatalf("output = %q, want a quorum relaxation reported too", quorum)
	}

	// An honest empty answer still says the widening was tried, or the reader
	// cannot tell a real absence from a query they should rephrase.
	empty := humanSearch(t, historysearch.Response{
		Matches: []historysearch.Match{}, Total: 0,
		EffectiveQuery: `"auth" OR "rewrite"`, MatchMode: "broad",
	}, "auth rewrite")
	if !strings.Contains(empty, "Too few messages had all of those words") ||
		!strings.Contains(empty, "(no matches)") {
		t.Fatalf("output = %q, want the widening reported above the empty answer", empty)
	}
}

// A strict search of exactly what was typed, answered by one conversation whose
// every hit is on the page, has nothing to add: the rows already said it. This
// is also the shape an older daemon sends, which returns no rollup at all.
func TestSearchHumanOutputStaysQuietWhenTheRollupAddsNothing(t *testing.T) {
	strict := humanSearch(t, historysearch.Response{
		Matches: []historysearch.Match{
			rollupMatch("aaaaaaaa-1111-4222-8333-444444444444", "auth rewrite", 0),
		},
		Total: 1,
		Sessions: []historysearch.SessionHits{
			{SessionID: "aaaaaaaa-1111-4222-8333-444444444444", Name: "auth rewrite", Hits: 1},
		},
		EffectiveQuery: `"auth"`, MatchMode: "strict",
		TotalHits: 1, TotalSessions: 1,
	}, "auth")
	older := humanSearch(t, historysearch.Response{
		Matches: []historysearch.Match{
			rollupMatch("aaaaaaaa-1111-4222-8333-444444444444", "auth rewrite", 0),
		},
		Total: 1,
	}, "auth")
	if strict != older {
		t.Fatalf("a strict single-conversation search printed %q, want the same %q an older daemon produces",
			strict, older)
	}
	if strings.Contains(strict, "conversation") {
		t.Fatalf("output = %q, want no rollup when the rows already answered it", strict)
	}
}

// The footer is a pointer, not a browse. Past a handful of conversations it
// stops and hands the reader the command whose job that is.
func TestSearchHumanRollupHandsAWideAnswerToHistory(t *testing.T) {
	response := historysearch.Response{
		Matches: []historysearch.Match{
			rollupMatch("aaaaaaaa-1111-4222-8333-444444444444", "auth rewrite", 0),
		},
		Total: 1, TotalHits: 30, TotalSessions: 12,
	}
	for index := 0; index < 12; index++ {
		response.Sessions = append(response.Sessions, historysearch.SessionHits{
			SessionID: fmt.Sprintf("%08d-1111-4222-8333-444444444444", index),
			Name:      fmt.Sprintf("conversation %d", index), Hits: 30 - index,
		})
	}
	output := humanSearch(t, response, "auth rewrite")
	if strings.Count(output, "conversation ") != searchRollupSessions {
		t.Fatalf("output = %q, want exactly %d conversations named", output, searchRollupSessions)
	}
	if !strings.Contains(output, `… and 4 more · browse them with sessions history "auth rewrite"`) {
		t.Fatalf("output = %q, want the rest handed to the conversation browser", output)
	}
}
