package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestGCDryRunByDefaultAndRequiresApplyToMutate(t *testing.T) {
	var dryRuns []bool
	var ages []int64
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/retention/gc" {
			http.NotFound(response, request)
			return
		}
		var body struct {
			OlderThanMS int64 `json:"older_than_ms"`
			DryRun      bool  `json:"dry_run"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		dryRuns = append(dryRuns, body.DryRun)
		ages = append(ages, body.OlderThanMS)
		status := "would_archive"
		if !body.DryRun {
			status = "archived"
		}
		response.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(response, `{"dry_run":`+
			map[bool]string{true: "true", false: "false"}[body.DryRun]+
			`,"cutoff_ms":1,"items":[{"id":"00000000-0000-4000-8000-000000000001","kind":"lane","closed_at_ms":1,"status":"`+
			status+`"}]}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())

	stdout, stderr, code := runGCCLI(server.URL, "gc", "--older-than", "7d")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Dry run") {
		t.Fatalf("dry run exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = runGCCLI(server.URL, "gc", "--older-than", "7d", "--apply")
	if code != 0 || stderr != "" || !strings.Contains(stdout, "Archived 1") {
		t.Fatalf("apply exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if len(dryRuns) != 2 || !dryRuns[0] || dryRuns[1] {
		t.Fatalf("dry_run requests = %#v", dryRuns)
	}
	if len(ages) != 2 || ages[0] != (7*24*time.Hour).Milliseconds() || ages[1] != ages[0] {
		t.Fatalf("retention ages = %#v", ages)
	}
}

func TestGCRejectsApplyAndDryRunTogether(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stdout, stderr, code := runGCCLI("", "gc", "--apply", "--dry-run")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "cannot be combined") {
		t.Fatalf("exit=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestParseRetentionAgeRejectsValuesBeyondTenYears(t *testing.T) {
	for _, value := range []string{"3651d", "87601h"} {
		if _, err := parseRetentionAge(value); err == nil {
			t.Fatalf("parseRetentionAge(%q) unexpectedly succeeded", value)
		}
	}
}

func runGCCLI(host string, args ...string) (string, string, int) {
	arguments := args
	if host != "" {
		arguments = append([]string{"--host", host}, args...)
	}
	var stdout, stderr bytes.Buffer
	code := run(arguments, strings.NewReader(""), &stdout, &stderr)
	return stdout.String(), stderr.String(), code
}
