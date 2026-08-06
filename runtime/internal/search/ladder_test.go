package search

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
)

// ladderFixture reproduces the shape of the real failure, not a toy of it.
//
// The target is the conversation a person is actually looking for. It says
// what they remember the subject being, and it never contains the verb they
// invented while describing it -- nobody writes "talked" into the thing they
// were talking about.
//
// The decoy is one unrelated message that happens to contain every word of the
// query at once. On the real corpus there was exactly one such message, in a
// session about something else entirely, and it was enough to end the search.
func ladderFixture() *fakeHistory {
	target := integrations.HistorySession{
		ID: "11111111-1111-4111-8111-111111111111", Name: "advertising handover",
		Tool: "codex", CWD: "/repo/ads", ConversationAvailable: true,
	}
	decoy := integrations.HistorySession{
		ID: "22222222-2222-4222-8222-222222222222", Name: "unrelated notes",
		Tool: "claude", CWD: "/repo/other", ConversationAvailable: true,
	}
	filler := integrations.HistorySession{
		ID: "33333333-3333-4333-8333-333333333333", Name: "filler",
		Tool: "claude", CWD: "/repo/filler", ConversationAvailable: true,
	}
	return &fakeHistory{
		sessions: []integrations.HistorySession{target, decoy, filler},
		transcript: map[string]integrations.TranscriptResponse{
			// A real conversation about a subject returns to it. The one on the
			// live corpus ran to hundreds of messages and said "taking" twenty
			// times; a fixture that gave the target a single mention would be
			// modelling a passing remark, which is what the decoy is.
			target.ID: {Messages: []integrations.TranscriptMessage{
				{Role: "user", Text: "I want you to take over my Google Ads account."},
				{Role: "assistant", Text: "Taking over the Google Ads account now; " +
					"the account has three active campaigns."},
				{Role: "user", Text: "Good. The ads budget for that account is fixed."},
				{Role: "assistant", Text: "Paused two ads in the account; the google " +
					"ads editor shows the change."},
				{Role: "user", Text: "Keep taking the low performing ads out of the account."},
			}},
			decoy.ID: {Messages: []integrations.TranscriptMessage{
				{Role: "user", Text: "We talked about taking the google ads account " +
					"approach for something completely different."},
			}},
			filler.ID: {Messages: []integrations.TranscriptMessage{
				{Role: "user", Text: "Unrelated conversation about deployment."},
			}},
		},
	}
}

// The query is how a person actually asks: a sentence describing a memory,
// with a verb that exists only in the description.
const rememberedSentence = "where we talked about taking over the google ads account"

func sessionRank(t *testing.T, result Response, id string) int {
	t.Helper()
	for index, session := range result.Sessions {
		if session.SessionID == id {
			return index + 1
		}
	}
	return 0
}

// One accidental message must not be able to end the search. Before this, the
// decoy was the entire result: it was the only message containing every word,
// so the strict rung was non-empty and the ladder stopped on it.
func TestACoincidenceDoesNotHideTheConversation(t *testing.T) {
	result := runRankedFixture(t, ladderFixture(), rememberedSentence,
		filepath.Join(t.TempDir(), "search-index.db"))

	const target = "11111111-1111-4111-8111-111111111111"
	rank := sessionRank(t, result, target)
	if rank == 0 {
		var names []string
		for _, session := range result.Sessions {
			names = append(names, session.Name)
		}
		t.Fatalf("the conversation being searched for was absent; got %d sessions %v "+
			"(match mode %q). A single unrelated message containing every word of the "+
			"sentence ended the search.", len(result.Sessions), names, result.MatchMode)
	}
	// Deliberately not asserting rank 1. Which of two matching sessions sorts
	// first is a property of bm25 over the whole corpus -- on the real one the
	// target won at -21.50 against the decoy's -17.10, because the real decoy's
	// message was long and diluted, while this fixture's is short and dense, so
	// here the decoy sorts first. Pinning rank 1 would mean shaping the fixture
	// until it produced the answer, which proves nothing. Being present at all
	// is the defect that was fixed; being ahead of an unrelated session is the
	// weakest ordering claim this corpus can honestly support.
	if filler := sessionRank(t, result, "33333333-3333-4333-8333-333333333333"); filler != 0 && rank > filler {
		t.Errorf("target ranked %d, behind an unrelated session at %d", rank, filler)
	}
}

// The rung that fires has to be reported honestly, because it is the only
// signal a caller has for how much the query was relaxed.
func TestTheRelaxedRungIsReported(t *testing.T) {
	result := runRankedFixture(t, ladderFixture(), rememberedSentence,
		filepath.Join(t.TempDir(), "search-index.db"))
	if result.MatchMode == "strict" {
		t.Fatalf("match mode is still %q, so the ladder never left the rung that "+
			"was hiding the answer", result.MatchMode)
	}
	if result.MatchMode == "" {
		t.Error("match mode was empty; a caller cannot tell a precise answer from a relaxed one")
	}
}

// Asking for particular words must keep meaning exactly that. A short query is
// a request, not a description, and one match is the answer rather than an
// accident -- so the relaxation must not reach it and start returning
// everything that merely shares a word.
func TestAShortQueryStaysPrecise(t *testing.T) {
	for _, query := range []string{"Google Ads", "deployment"} {
		t.Run(query, func(t *testing.T) {
			result := runRankedFixture(t, ladderFixture(), query,
				filepath.Join(t.TempDir(), "search-index.db"))
			if result.MatchMode == "broad" {
				t.Fatalf("a %d-word query relaxed to broad; short queries must stay "+
					"precise or every identifier search returns the corpus",
					len(strings.Fields(query)))
			}
			if len(result.Sessions) == 0 {
				t.Fatal("no sessions at all")
			}
		})
	}
}

// The bar scales with the sentence, so it must be reachable: a long query whose
// terms genuinely do co-occur, repeatedly, is a precise answer and must not be
// relaxed away into the rest of the corpus.
func TestALongQueryThatGenuinelyMatchesStaysStrict(t *testing.T) {
	result := runRankedFixture(t, ladderFixture(),
		"take over my google ads account",
		filepath.Join(t.TempDir(), "search-index.db"))
	const target = "11111111-1111-4111-8111-111111111111"
	if rank := sessionRank(t, result, target); rank != 1 {
		t.Fatalf("target ranked %d, want 1 (match mode %q)", rank, result.MatchMode)
	}
}
