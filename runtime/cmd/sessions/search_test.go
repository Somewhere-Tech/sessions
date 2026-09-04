package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	historysearch "github.com/somewhere-tech/sessions/runtime/internal/search"
)

func TestSearchCLIForwardsFiltersAndGroupsHumanOutput(t *testing.T) {
	timestamp := "2026-07-17T20:00:00Z"
	var received map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/api/search" {
			http.NotFound(response, request)
			return
		}
		received = make(map[string]string)
		for key := range request.URL.Query() {
			received[key] = request.URL.Query().Get(key)
		}
		_ = json.NewEncoder(response).Encode(historysearch.Response{
			Matches: []historysearch.Match{
				{SessionID: "aaaaaaaa-1111-4222-8333-444444444444", Name: "alpha", Tool: "codex", Role: "assistant", Timestamp: &timestamp, Snippet: "saw [[needle]] here"},
				{SessionID: "aaaaaaaa-1111-4222-8333-444444444444", Name: "alpha", Tool: "codex", Role: "assistant", Snippet: "another [[needle]]"},
				{SessionID: "bbbbbbbb-1111-4222-8333-444444444444", Name: "beta", Tool: "claude", Role: "user", Snippet: "asked for [[needle]]"},
			}, Total: 3,
		})
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "search", `needle [0-9]+`, "--session", "aaaaaaaa", "--role", "assistant", "--tool", "codex", "-n", "7", "--regex"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	wantQuery := map[string]string{
		"q": `needle [0-9]+`, "session": "aaaaaaaa", "role": "assistant",
		"tool": "codex", "limit": "7", "regex": "true",
	}
	if !mapsEqual(received, wantQuery) {
		t.Fatalf("query=%#v want=%#v", received, wantQuery)
	}
	want := "aaaaaaaa  alpha  codex\n" +
		"  assistant  2026-07-17T20:00:00Z  message 1\n    saw [[needle]] here\n\n" +
		"aaaaaaaa  alpha  codex\n  assistant  (no timestamp)  message 1\n    another [[needle]]\n\n" +
		"bbbbbbbb  beta  claude\n  user  (no timestamp)  message 1\n    asked for [[needle]]\n\n"
	if stdout.String() != want {
		t.Fatalf("stdout=%q want=%q", stdout.String(), want)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", server.URL, "search", "emails", "--ranked"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 || !mapsEqual(received, map[string]string{"q": "emails", "ranked": "true"}) {
		t.Fatalf("ranked exit=%d query=%#v stdout=%q stderr=%q", code, received, stdout.String(), stderr.String())
	}
}

func TestSearchCLIJSONShapeAndValidation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(historysearch.Response{Matches: []historysearch.Match{}, Total: 0})
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--host", server.URL, "search", "absent", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 || stderr.Len() != 0 {
		t.Fatalf("json exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var shape struct {
		Matches []historysearch.Match `json:"matches"`
		Total   int                   `json:"total"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &shape); err != nil || shape.Matches == nil || shape.Total != 0 {
		t.Fatalf("shape=%#v err=%v raw=%q", shape, err, stdout.String())
	}
	for _, args := range [][]string{
		{"search"}, {"search", "x", "--role", "system"}, {"search", "x", "--tool", "terminal"},
		{"search", "x", "-n", "0"}, {"search", "x", "--session"}, {"search", "x", "--unknown"},
		{"search", "x", "--ranked", "--regex"},
	} {
		stdout.Reset()
		stderr.Reset()
		if code := run(args, strings.NewReader(""), &stdout, &stderr); code != 1 || stderr.Len() == 0 {
			t.Errorf("args=%#v exit=%d stdout=%q stderr=%q", args, code, stdout.String(), stderr.String())
		}
	}
}

func TestSearchFleetReturnsPartialDeduplicatedDurableReferences(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	timestamp := "2026-08-01T12:00:00Z"
	shared := historysearch.Match{
		SessionID: "provider-history:claude:shared", ProviderSessionID: "provider-shared",
		Name: "Google Ads", Tool: "claude", Role: "user", MessageID: "message-shared",
		Timestamp: &timestamp, Snippet: "[[Google Ads]] budget",
	}
	local := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(response).Encode(historysearch.Response{Matches: []historysearch.Match{shared}, Total: 1})
	}))
	defer local.Close()
	remote := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		unique := shared
		unique.SessionID = "provider-history:claude:remote"
		unique.ProviderSessionID = "provider-remote"
		unique.MessageID = "message-remote"
		unique.Name = "Campaign notes"
		_ = json.NewEncoder(response).Encode(historysearch.Response{Matches: []historysearch.Match{shared, unique}, Total: 2})
	}))
	defer remote.Close()
	for _, machine := range []savedMachine{
		{Alias: "mini", MachineID: "machine-mini", Name: "Mac mini", Endpoint: remote.URL},
		{Alias: "offline", MachineID: "machine-offline", Name: "Offline Mac", Endpoint: "http://127.0.0.1:1"},
	} {
		if _, err := saveMachine(home, machine, "device-secret"); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	application, err := newApp(
		[]string{"--json", "--host", local.URL, "search", "Google Ads"},
		strings.NewReader(""), &stdout, &stderr,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer application.close()
	application.explicitTarget = false
	application.direct = true
	if err := application.dispatch(); err != nil {
		t.Fatal(err)
	}
	var result historysearch.Response
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode fleet search: %v\n%s", err, stdout.String())
	}
	if !result.Partial || len(result.Matches) != 2 || len(result.Machines) != 3 {
		t.Fatalf("fleet result = %#v", result)
	}
	if result.Matches[0].Reference != "local::provider-history:claude:shared" ||
		!reflect.DeepEqual(result.Matches[0].AvailableOn, []string{"local", "mini"}) {
		t.Fatalf("deduplicated match = %#v", result.Matches[0])
	}
	if result.Matches[1].Reference != "mini::provider-history:claude:remote" {
		t.Fatalf("remote reference = %#v", result.Matches[1])
	}
	if stderr.Len() != 0 {
		t.Fatalf("JSON search wrote stderr: %q", stderr.String())
	}
}

func TestGrepNormalizesFamiliarFlags(t *testing.T) {
	got, err := normalizeGrepArgs([]string{"-i", "-C3", "--tool", "claude", "Google Ads"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Google Ads", "--context", "3", "--tool", "claude"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("normalizeGrepArgs = %#v, want %#v", got, want)
	}
}

func TestFleetSearchOrderingDoesNotFavorTheLocalMachine(t *testing.T) {
	older := "2026-08-01T10:00:00Z"
	newer := "2026-08-01T11:00:00Z"
	matches := []historysearch.Match{
		{Reference: "local::older", Timestamp: &older, Score: 0.2},
		{Reference: "mini::newer", Timestamp: &newer, Score: 0.9},
	}
	sortFleetSearchMatches(matches, false, true)
	if matches[0].Reference != "mini::newer" {
		t.Fatalf("ranked fleet order = %#v", matches)
	}
	sortFleetSearchMatches(matches, true, true)
	if matches[0].Reference != "local::older" {
		t.Fatalf("timeline fleet order = %#v", matches)
	}
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
