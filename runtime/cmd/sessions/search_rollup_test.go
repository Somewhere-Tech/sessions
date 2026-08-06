package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
)

// rollupResponseServer answers with both a match page and the session rollup
// behind it, which is what a real daemon returns.
func rollupResponseServer(t *testing.T, response historysearch.Response) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(writer).Encode(response)
	}))
	t.Cleanup(server.Close)
	return server
}

// The rollup is the answer to "which session was that", which is the question
// an agent is actually asking. It was computed per machine and then discarded
// by the fleet merge, so only a single-daemon caller ever saw it -- and the
// fleet path is the default.
func TestFleetSearchKeepsTheSessionRollup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	local := rollupResponseServer(t, historysearch.Response{
		Matches: []historysearch.Match{searchMatchFixture("session-a", "auth rewrite")},
		Total:   1,
		Sessions: []historysearch.SessionHits{{
			SessionID: "session-a", Name: "auth rewrite", Hits: 12, Score: 0.61,
			TitleMatch: true, FirstHitAt: "2026-07-01T00:00:00Z", LastHitAt: "2026-08-01T00:00:00Z",
		}},
		EffectiveQuery: `"auth" AND "rewrite"`, MatchMode: "strict",
		TotalHits: 12, TotalSessions: 1,
	})

	application, stdout, _ := fleetSearchApp(t, local.URL, "search", "auth rewrite")
	if err := application.dispatch(); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	var decoded historysearch.Response
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("undecodable search response: %v (%q)", err, stdout.String())
	}
	if len(decoded.Sessions) != 1 {
		t.Fatalf("rollup carried %d sessions, want 1 — the fleet merge dropped it", len(decoded.Sessions))
	}
	rollup := decoded.Sessions[0]
	if rollup.Hits != 12 || !rollup.TitleMatch {
		t.Fatalf("rollup = %+v, want the hit count and title flag preserved", rollup)
	}
	if !strings.Contains(rollup.SessionID, "session-a") {
		t.Fatalf("rollup session id = %q, want it to still name the session", rollup.SessionID)
	}
	// An agent cannot tell how its query was interpreted unless this survives.
	if decoded.EffectiveQuery != `"auth" AND "rewrite"` || decoded.MatchMode != "strict" {
		t.Fatalf("effective query = %q mode = %q, want both carried through",
			decoded.EffectiveQuery, decoded.MatchMode)
	}
	if decoded.TotalHits != 12 || decoded.TotalSessions != 1 {
		t.Fatalf("totals = %d hits / %d sessions, want 12 / 1", decoded.TotalHits, decoded.TotalSessions)
	}
}

// Two machines interpreting one query differently means they are running
// different versions. Reporting either interpretation would be a guess about
// which produced these results.
func TestFleetSearchWithdrawsADisputedEffectiveQuery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	local := rollupResponseServer(t, historysearch.Response{
		Matches:        []historysearch.Match{searchMatchFixture("session-a", "auth rewrite")},
		Total:          1,
		EffectiveQuery: `"auth" AND "rewrite"`, MatchMode: "strict",
	})
	peer := rollupResponseServer(t, historysearch.Response{
		Matches:        []historysearch.Match{searchMatchFixture("session-b", "auth notes")},
		Total:          1,
		EffectiveQuery: `"auth" OR "rewrite"`, MatchMode: "broad",
	})

	if _, err := saveMachine(home, savedMachine{
		Alias: "peer", MachineID: "machine-peer", Name: "Peer", Endpoint: peer.URL,
	}, "device-secret"); err != nil {
		t.Fatal(err)
	}

	application, stdout, _ := fleetSearchApp(t, local.URL, "search", "auth rewrite")
	if err := application.dispatch(); err != nil {
		t.Fatalf("search failed: %v", err)
	}
	var decoded historysearch.Response
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("undecodable search response: %v", err)
	}
	if decoded.EffectiveQuery != "" || decoded.MatchMode != "" {
		t.Fatalf("effective query = %q mode = %q, want both withdrawn when the fleet disagrees",
			decoded.EffectiveQuery, decoded.MatchMode)
	}
	if !decoded.RollupPartial {
		t.Fatal("a disagreeing fleet was not reported as partial")
	}
}
