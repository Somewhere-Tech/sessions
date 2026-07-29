package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
