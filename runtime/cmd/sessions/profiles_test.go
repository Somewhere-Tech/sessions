package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNewProfileParsingAndTeachingErrors(t *testing.T) {
	var posted createSessionRequest
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		requests++
		if request.URL.Path != "/api/sessions" || request.Method != http.MethodPost {
			http.NotFound(response, request)
			return
		}
		if err := json.NewDecoder(request.Body).Decode(&posted); err != nil {
			t.Errorf("decode create request: %v", err)
		}
		response.Header().Set("Content-Type", "application/json")
		response.WriteHeader(http.StatusCreated)
		_, _ = response.Write([]byte(`{"id":"profile-session"}`))
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "new", "--tool", "claude", "--profile", "work"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.String() != "" || posted.Profile != "work" || posted.Cmd != "claude" {
		t.Fatalf("profile create exit=%d posted=%#v stdout=%q stderr=%q", code, posted, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{
		{"--host", server.URL, "new", "--tool", "shell", "--profile", "work"},
		{"--host", server.URL, "new", "--tool", "claude", "--profile", "Work_Home"},
		{"--host", server.URL, "new", "--tool", "claude", "--profile"},
	} {
		before := requests
		stdout.Reset()
		stderr.Reset()
		if code := run(args, strings.NewReader(""), &stdout, &stderr); code == 0 || requests != before {
			t.Fatalf("invalid args %q exit=%d requests=%d->%d stdout=%q stderr=%q", args, code, before, requests, stdout.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "profile") {
			t.Fatalf("invalid args %q teaching error = %q", args, stderr.String())
		}
	}
}

func TestProfilesListTableJSONAndNoDeleteTeaching(t *testing.T) {
	const responseBody = `{"profiles":[{"tool":"claude","name":"work","path":"/private/profiles/claude/work","sessions":[{"id":"aaaaaaaa-1111-4222-8333-444444444444","name":"agent"}],"last_used":1000},{"tool":"codex","name":"personal","path":"/private/profiles/codex/personal","sessions":[],"last_used":2000}]}`
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/profiles" || request.Method != http.MethodGet {
			http.NotFound(response, request)
			return
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(responseBody))
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := run([]string{"--host", server.URL, "profiles"}, strings.NewReader(""), &stdout, &stderr)
	if code != 0 || stderr.String() != "" {
		t.Fatalf("profiles exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{"TOOL", "NAME", "SESSIONS", "LAST-USED", "PATH", "claude", "work", "agent", "/private/profiles/claude/work", "never deletes profile credentials"} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("profiles table missing %q:\n%s", want, stdout.String())
		}
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"--host", server.URL, "--json", "profiles"}, strings.NewReader(""), &stdout, &stderr)
	var profiles []profileStatus
	if code != 0 || json.Unmarshal(stdout.Bytes(), &profiles) != nil || len(profiles) != 2 || profiles[0].Name != "work" {
		t.Fatalf("profiles json exit=%d profiles=%#v stdout=%q stderr=%q", code, profiles, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"--host", server.URL, "profiles", "delete", "work"}, strings.NewReader(""), &stdout, &stderr); code == 0 ||
		!strings.Contains(stderr.String(), "never deletes profile credentials") || !strings.Contains(stderr.String(), "sessions profiles") {
		t.Fatalf("profiles delete teaching exit=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}
