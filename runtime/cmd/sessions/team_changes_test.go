package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTeamSincePassesCursorAndPreservesReceipt(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/sessions" {
			_, _ = w.Write([]byte(`{"sessions":[{"id":"manager"}]}`))
			return
		}
		if r.URL.Path != "/api/lanes/mine" || r.URL.Query().Get("since") != "baseline" || r.URL.Query().Get("lane") != "manager" {
			t.Errorf("unexpected %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"members":[{"id":"worker","handoff":{"source":"agent-reported","push":"unknown","tests":["passed"]}}],"next_cursor":"next","delta":true,"total":2,"removed":["old"],"needs_input":1}`))
	}))
	defer server.Close()
	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "--json", "team", "manager", "--since", "baseline"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 {
		t.Fatalf("rc=%d out=%s err=%s", code, &stdout, &stderr)
	}
	var listing teamListing
	if err := json.Unmarshal(stdout.Bytes(), &listing); err != nil {
		t.Fatal(err)
	}
	if listing.NextCursor != "next" || listing.Members[0].Handoff == nil || len(listing.Removed) != 1 {
		t.Fatalf("listing=%+v", listing)
	}
}

func TestTeamSinceRejectsAllAndEmpty(t *testing.T) {
	for _, args := range [][]string{{"team", "--all", "--since", "x"}, {"team", "--since", ""}} {
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader(""), &stdout, &stderr); code == 0 {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestEmptyTeamPrintsScopedNextCheck(t *testing.T) {
	var out bytes.Buffer
	a := &app{stdout: &out}
	if err := a.writeTeam(teamListing{Caller: "manager", NextCursor: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no lanes delegated") || !strings.Contains(out.String(), "sessions team manager --since checkpoint --json") {
		t.Fatalf("missing scoped recovery command: %s", &out)
	}
}

func TestHandoffClaimsCannotEmitTerminalControls(t *testing.T) {
	var out bytes.Buffer
	a := &app{stdout: &out}
	receipt := &teamHandoff{Source: "agent-reported", Summary: "\x1b[2Jfake", Tests: []string{"\x1b[31mPASS"}}
	if err := a.writeTeamChangesFooter(teamListing{Members: []teamMember{{ID: "worker", Handoff: receipt}}}); err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(out.String(), '\x1b') {
		t.Fatalf("untrusted handoff emitted terminal control: %q", out.String())
	}
}
